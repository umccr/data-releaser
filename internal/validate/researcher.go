package validate

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/micahhausler/aws-iam-policy/policy"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	latticepkg "github.com/umccr/data-releaser/internal/lattice"
)

// ValidateResearcherOpts holds the dependencies for researcher-side validation.
type ValidateResearcherOpts struct {
	RAMClient    internalaws.RAMClient
	LambdaClient internalaws.LambdaClient
	S3Client     internalaws.S3Client
	AccountID    string
	// ManifestBucket is the Data Control Manifest_Bucket whose cross-account policy
	// is checked for the researcher's read grant.
	ManifestBucket string
	// ManifestPrefix is the account's manifest key prefix (e.g. manifests/<accountId>/*).
	ManifestPrefix string
}

// ValidateResearcher runs researcher-side validation checks and returns per-check results.
// It continues running all checks even if some fail.
func ValidateResearcher(ctx context.Context, opts ValidateResearcherOpts) []Check {
	var checks []Check
	checks = append(checks, checkRAMAssociation(ctx, opts.RAMClient, opts.AccountID))
	checks = append(checks, checkManifestBucketPolicy(ctx, opts.S3Client, opts.ManifestBucket, opts.ManifestPrefix, opts.AccountID))
	checks = append(checks, researcherCheckTestLambda(ctx, opts.LambdaClient)...)
	return checks
}

// checkRAMAssociation verifies that the VPC Lattice service network association
// to the target researcher account is present and in an active/associated state.
func checkRAMAssociation(ctx context.Context, ramClient internalaws.RAMClient, accountID string) Check {
	const name = "VPC Lattice / RAM Association"
	if err := latticepkg.NewChecker(ramClient).VerifyAssociation(ctx, accountID); err != nil {
		return fail(name, "%s", err)
	}
	return pass(name)
}

// awsPrincipals returns the AWS-account principals a statement grants to, and whether
// the statement grants to everyone.
//
// The policy library reports a bare-string Principal (both `"*"` and
// `"arn:aws:iam::...:root"`, which AWS accepts as shorthand) with kind "All", so the two
// have to be told apart by value rather than by kind.
func awsPrincipals(p *policy.Principal) (principals []string, all bool) {
	if p == nil {
		return nil, false
	}
	if slices.Contains(p.Kinds(), policy.PrincipalKindAll) {
		// MarshalJSON round-trips the original string form, quoted.
		raw, err := p.MarshalJSON()
		if err != nil {
			return nil, false
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false
		}
		if s == policy.PrincipalAll {
			return nil, true
		}
		return []string{s}, false
	}
	if awsPrincipal := p.AWS(); awsPrincipal != nil {
		return awsPrincipal.Values(), false
	}
	return nil, false
}

// principalMatchesAccount reports whether any of a statement's principals identifies
// accountID.
//
// A principal is either the bare account ID or an ARN whose account field is that ID --
// matched on the parsed account field, not by substring, so an unrelated ARN that merely
// contains the digits somewhere (a role path, say) does not count as a grant.
func principalMatchesAccount(p *policy.Principal, accountID string) bool {
	principals, all := awsPrincipals(p)
	if all {
		return true
	}
	for _, candidate := range principals {
		candidate = strings.TrimSpace(candidate)
		if candidate == accountID {
			return true
		}
		if parsed, err := awsarn.Parse(candidate); err == nil && parsed.AccountID == accountID {
			return true
		}
	}
	return false
}

// actionGrantsGetObject reports whether an action list covers s3:GetObject, accepting the
// action itself or a wildcard that subsumes it.
func actionGrantsGetObject(actions *policy.StringOrSlice) bool {
	if actions == nil {
		return false
	}
	for _, a := range actions.Values() {
		switch strings.ToLower(strings.TrimSpace(a)) {
		case "s3:getobject", "s3:*", "*":
			return true
		}
	}
	return false
}

// resourceCoversPrefix reports whether a resource list grants access to every object
// under the account's own manifest prefix in bucket.
//
// Each resource is an S3 object ARN, optionally ending in a `*` wildcard. Matching is
// done on the parsed ARN's resource path ("<bucket>/<key>") rather than by substring, so
// a grant on a different bucket, or on a sibling account's prefix, is not mistaken for
// this account's. A bare "*" resource covers everything.
func resourceCoversPrefix(resources *policy.StringOrSlice, bucket, prefixPath string) bool {
	if resources == nil {
		return false
	}
	want := bucket + "/" + prefixPath
	for _, r := range resources.Values() {
		r = strings.TrimSpace(r)
		if r == "*" {
			return true
		}
		parsed, err := awsarn.Parse(r)
		if err != nil || parsed.Service != "s3" {
			continue
		}
		// A trailing "*" grants everything below the path it follows; anything else must
		// name the prefix exactly.
		if pattern, ok := strings.CutSuffix(parsed.Resource, "*"); ok {
			if strings.HasPrefix(want, pattern) {
				return true
			}
			continue
		}
		if parsed.Resource == want {
			return true
		}
	}
	return false
}

// checkManifestBucketPolicy verifies the Manifest_Bucket's cross-account bucket policy
// grants the target Researcher_Account read access (s3:GetObject) to its own manifest
// prefix. If the grant is absent, it reports that the onboarding step which adds the
// cross-account read grant to the Manifest_Bucket policy is incomplete.
func checkManifestBucketPolicy(ctx context.Context, s3Client internalaws.S3Client, bucket, prefix, accountID string) Check {
	const name = "Manifest_Bucket Cross-Account Read Grant"

	if s3Client == nil {
		return fail(name, "S3 client not configured")
	}
	if bucket == "" {
		return fail(name, "manifest.bucket not configured; cannot verify the researcher's cross-account read grant")
	}

	out, err := s3Client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return fail(name, "could not read the Manifest_Bucket %q policy; the researcher's cross-account read grant may be missing "+
			"and the onboarding step which adds it to the Manifest_Bucket policy is incomplete: %v", bucket, err)
	}
	if out.Policy == nil || *out.Policy == "" {
		return fail(name, "the Manifest_Bucket %q has no bucket policy; the researcher's cross-account read grant is missing "+
			"and the onboarding step which adds it to the Manifest_Bucket policy is incomplete", bucket)
	}

	var doc policy.Policy
	if err := json.Unmarshal([]byte(*out.Policy), &doc); err != nil {
		return fail(name, "the Manifest_Bucket %q policy is not valid JSON: %v", bucket, err)
	}
	if doc.Statements == nil {
		return fail(name, "the Manifest_Bucket %q policy has no statements; the researcher's cross-account read grant is missing "+
			"and the onboarding step which adds it to the Manifest_Bucket policy is incomplete", bucket)
	}

	// Derive the prefix path segment used to scope the grant (e.g. "manifests/<accountId>/").
	prefixPath := strings.TrimSuffix(prefix, "*")

	// This looks for a grant; it is not a full policy evaluation. In particular an
	// explicit Deny elsewhere in the policy would override the Allow found here, so a
	// pass means "the onboarding grant is present", not "the researcher can definitely
	// read". Deny statements are skipped rather than interpreted.
	for _, st := range doc.Statements.Values() {
		if !strings.EqualFold(st.Effect, policy.EffectAllow) {
			continue
		}
		if !principalMatchesAccount(st.Principal, accountID) {
			continue
		}
		if !actionGrantsGetObject(st.Action) {
			continue
		}
		if !resourceCoversPrefix(st.Resource, bucket, prefixPath) {
			continue
		}
		return pass(name)
	}

	return fail(name, "the Manifest_Bucket %q policy does not grant account %s read (s3:GetObject) on its prefix %q; "+
		"the researcher's cross-account read grant is missing and the onboarding step which adds it to the Manifest_Bucket policy is incomplete",
		bucket, accountID, prefix)
}

// researcherCheckTestLambda discovers and invokes the Test_Lambda in the researcher
// account. Discovery always yields a check; invocation only runs if a function was
// found, since the Test_Lambda is optional.
func researcherCheckTestLambda(ctx context.Context, lambdaClient internalaws.LambdaClient) []Check {
	const (
		discoveryName  = "Test_Lambda Discovery"
		invocationName = "Test_Lambda Invocation"
	)

	functionName, err := discoverTestLambda(ctx, lambdaClient)
	switch {
	case err != nil:
		return []Check{fail(discoveryName, "failed to list Lambda functions: %v", err)}
	case functionName == "":
		// Absence is reported as an unavailable-but-passing check: the Test_Lambda is
		// optional, so not deploying it must not fail validation.
		return []Check{{
			Name:   discoveryName,
			Passed: true,
			Error: "UNAVAILABLE: Test_Lambda not found in target account; " +
				"deploy it via the consumer construct's deployTestLambda option for end-to-end validation",
		}}
	}

	return []Check{pass(discoveryName), invokeResearcherTestLambda(ctx, lambdaClient, functionName, invocationName)}
}

// invokeResearcherTestLambda invokes the discovered Test_Lambda and reports whether it
// fetched the canary data end to end.
func invokeResearcherTestLambda(ctx context.Context, lambdaClient internalaws.LambdaClient, functionName, name string) Check {
	invokeOutput, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: &functionName,
	})
	if err != nil {
		return fail(name, "failed to invoke Test_Lambda %q: %v", functionName, err)
	}

	if invokeOutput.FunctionError != nil && *invokeOutput.FunctionError != "" {
		if invokeOutput.Payload != nil {
			return fail(name, "Test_Lambda returned function error: %s (payload: %s)",
				*invokeOutput.FunctionError, string(invokeOutput.Payload))
		}
		return fail(name, "Test_Lambda returned function error: %s", *invokeOutput.FunctionError)
	}

	if invokeOutput.Payload == nil {
		return fail(name, "Test_Lambda returned nil payload")
	}

	var resp testLambdaResponse
	if err := json.Unmarshal(invokeOutput.Payload, &resp); err != nil {
		return fail(name, "Test_Lambda response is not valid JSON: %v (payload: %s)",
			err, string(invokeOutput.Payload))
	}

	if ok, reason := validateTestLambdaResponse(resp); !ok {
		return fail(name, "%s", reason)
	}

	return pass(name)
}
