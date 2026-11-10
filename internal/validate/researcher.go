package validate

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/micahhausler/aws-iam-policy/policy"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/iampolicy"
	latticepkg "github.com/umccr/data-releaser/internal/lattice"
)

// ValidateResearcherOpts holds the dependencies for researcher-side validation.
type ValidateResearcherOpts struct {
	RAMClient    internalaws.RAMClient
	LambdaClient internalaws.LambdaClient
	S3Client     internalaws.S3Client
	AccountID    string
	// ManifestBucket is the Data Control manifest bucket whose cross-account policy
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

// checkManifestBucketPolicy verifies the manifest bucket's cross-account bucket policy
// grants the target researcher account read access (s3:GetObject) to its own manifest
// prefix. If the grant is absent, it reports that the onboarding step which adds the
// cross-account read grant to the manifest bucket policy is incomplete.
func checkManifestBucketPolicy(ctx context.Context, s3Client internalaws.S3Client, bucket, prefix, accountID string) Check {
	const name = "Manifest Bucket Cross-Account Read Grant"

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
		return fail(name, "could not read the manifest bucket %q policy; the researcher's cross-account read grant may be missing "+
			"and the onboarding step which adds it to the manifest bucket policy is incomplete: %v", bucket, err)
	}
	if out.Policy == nil || *out.Policy == "" {
		return fail(name, "the manifest bucket %q has no bucket policy; the researcher's cross-account read grant is missing "+
			"and the onboarding step which adds it to the manifest bucket policy is incomplete", bucket)
	}

	var doc policy.Policy
	if err := json.Unmarshal([]byte(*out.Policy), &doc); err != nil {
		return fail(name, "the manifest bucket %q policy is not valid JSON: %v", bucket, err)
	}
	if doc.Statements == nil {
		return fail(name, "the manifest bucket %q policy has no statements; the researcher's cross-account read grant is missing "+
			"and the onboarding step which adds it to the manifest bucket policy is incomplete", bucket)
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
		if !iampolicy.PrincipalsOf(st.Principal).IncludesAccount(accountID) {
			continue
		}
		if !iampolicy.ActionGrants(st.Action, "s3:GetObject") {
			continue
		}
		if !iampolicy.ResourceCoversPrefix(st.Resource, bucket, prefixPath) {
			continue
		}
		return pass(name)
	}

	return fail(name, "the manifest bucket %q policy does not grant account %s read (s3:GetObject) on its prefix %q; "+
		"the researcher's cross-account read grant is missing and the onboarding step which adds it to the manifest bucket policy is incomplete",
		bucket, accountID, prefix)
}

// researcherCheckTestLambda discovers and invokes the test lambda in the researcher
// account. Discovery always yields a check; invocation only runs if a function was
// found, since the test lambda is optional.
func researcherCheckTestLambda(ctx context.Context, lambdaClient internalaws.LambdaClient) []Check {
	const (
		discoveryName  = "Test Lambda Discovery"
		invocationName = "Test Lambda Invocation"
	)

	functionName, err := discoverTestLambda(ctx, lambdaClient)
	switch {
	case err != nil:
		return []Check{fail(discoveryName, "failed to list Lambda functions: %v", err)}
	case functionName == "":
		// Absence is reported as an unavailable-but-passing check: the test lambda is
		// optional, so not deploying it must not fail validation.
		return []Check{{
			Name:   discoveryName,
			Passed: true,
			Error: "UNAVAILABLE: test lambda not found in target account; " +
				"deploy it via the consumer construct's deployTestLambda option for end-to-end validation",
		}}
	}

	return []Check{pass(discoveryName), invokeResearcherTestLambda(ctx, lambdaClient, functionName, invocationName)}
}

// invokeResearcherTestLambda invokes the discovered test lambda and reports whether it
// fetched the canary data end to end.
func invokeResearcherTestLambda(ctx context.Context, lambdaClient internalaws.LambdaClient, functionName, name string) Check {
	invokeOutput, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: &functionName,
	})
	if err != nil {
		return fail(name, "failed to invoke test lambda %q: %v", functionName, err)
	}

	if invokeOutput.FunctionError != nil && *invokeOutput.FunctionError != "" {
		if invokeOutput.Payload != nil {
			return fail(name, "test lambda returned function error: %s (payload: %s)",
				*invokeOutput.FunctionError, string(invokeOutput.Payload))
		}
		return fail(name, "test lambda returned function error: %s", *invokeOutput.FunctionError)
	}

	if invokeOutput.Payload == nil {
		return fail(name, "test lambda returned nil payload")
	}

	var resp testLambdaResponse
	if err := json.Unmarshal(invokeOutput.Payload, &resp); err != nil {
		return fail(name, "test lambda response is not valid JSON: %v (payload: %s)",
			err, string(invokeOutput.Payload))
	}

	if ok, reason := validateTestLambdaResponse(resp); !ok {
		return fail(name, "%s", reason)
	}

	return pass(name)
}
