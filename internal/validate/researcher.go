package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	latticepkg "github.com/umccr/data-releaser/internal/lattice"
)

// researcherLambdaResponse represents the expected response structure from the Test_Lambda
// during researcher-side validation.
type researcherLambdaResponse struct {
	StatusCode      int             `json:"statusCode"`
	LatticeResponse json.RawMessage `json:"latticeResponse"`
}

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
// It continues running all checks even if some fail (Req 15.7).
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
	check := Check{Name: "VPC Lattice / RAM Association"}
	checker := latticepkg.NewChecker(ramClient)
	if err := checker.VerifyAssociation(ctx, accountID); err != nil {
		check.Passed = false
		check.Error = err.Error()
		return check
	}
	check.Passed = true
	return check
}

// bucketPolicyDocument is a minimal representation of an S3 bucket policy for grant checking.
type bucketPolicyDocument struct {
	Statement []bucketPolicyStatement `json:"Statement"`
}

// bucketPolicyStatement is a single statement in a bucket policy. Principal, Action,
// and Resource may each be a string or a list, so they are decoded flexibly.
type bucketPolicyStatement struct {
	Effect    string          `json:"Effect"`
	Principal json.RawMessage `json:"Principal"`
	Action    json.RawMessage `json:"Action"`
	Resource  json.RawMessage `json:"Resource"`
}

// coerceStringList decodes a JSON value that may be a string or an array of strings.
func coerceStringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	return nil
}

// coercePrincipalStrings extracts all principal identifiers from a Principal value,
// which may be "*", a string, an array, or an object like {"AWS": [...]}.
func coercePrincipalStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// Try string or []string first.
	if vals := coerceStringList(raw); vals != nil {
		return vals
	}
	// Try object form: {"AWS": "..."} or {"AWS": [...]}.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		var out []string
		for _, v := range obj {
			out = append(out, coerceStringList(v)...)
		}
		return out
	}
	return nil
}

// checkManifestBucketPolicy verifies the Manifest_Bucket's cross-account bucket policy
// grants the target Researcher_Account read access (s3:GetObject) to its own manifest
// prefix. If the grant is absent, it reports that the onboarding step which adds the
// cross-account read grant to the Manifest_Bucket policy is incomplete (Req 15.9).
func checkManifestBucketPolicy(ctx context.Context, s3Client internalaws.S3Client, bucket, prefix, accountID string) Check {
	check := Check{Name: "Manifest_Bucket Cross-Account Read Grant"}

	if s3Client == nil {
		check.Passed = false
		check.Error = "S3 client not configured"
		return check
	}
	if bucket == "" {
		check.Passed = false
		check.Error = "manifest.bucket not configured; cannot verify the researcher's cross-account read grant"
		return check
	}

	out, err := s3Client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("could not read the Manifest_Bucket %q policy; the researcher's cross-account read grant may be missing "+
			"and the onboarding step which adds it to the Manifest_Bucket policy is incomplete: %v", bucket, err)
		return check
	}
	if out.Policy == nil || *out.Policy == "" {
		check.Passed = false
		check.Error = fmt.Sprintf("the Manifest_Bucket %q has no bucket policy; the researcher's cross-account read grant is missing "+
			"and the onboarding step which adds it to the Manifest_Bucket policy is incomplete", bucket)
		return check
	}

	var doc bucketPolicyDocument
	if err := json.Unmarshal([]byte(*out.Policy), &doc); err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("the Manifest_Bucket %q policy is not valid JSON: %v", bucket, err)
		return check
	}

	// Derive the prefix path segment used to scope the grant (e.g. "manifests/<accountId>/").
	prefixPath := strings.TrimSuffix(prefix, "*")

	for _, st := range doc.Statement {
		if !strings.EqualFold(st.Effect, "Allow") {
			continue
		}

		// Principal must reference the researcher account.
		principalMatch := false
		for _, p := range coercePrincipalStrings(st.Principal) {
			if strings.Contains(p, accountID) {
				principalMatch = true
				break
			}
		}
		if !principalMatch {
			continue
		}

		// Action must grant s3:GetObject (or a wildcard covering it).
		actionMatch := false
		for _, a := range coerceStringList(st.Action) {
			la := strings.ToLower(a)
			if la == "s3:getobject" || la == "s3:*" || la == "*" {
				actionMatch = true
				break
			}
		}
		if !actionMatch {
			continue
		}

		// Resource must be scoped to the account's own manifest prefix.
		resourceMatch := false
		for _, r := range coerceStringList(st.Resource) {
			if strings.Contains(r, bucket) && (prefixPath == "" || strings.Contains(r, prefixPath) || strings.Contains(r, accountID)) {
				resourceMatch = true
				break
			}
		}
		if !resourceMatch {
			continue
		}

		check.Passed = true
		return check
	}

	check.Passed = false
	check.Error = fmt.Sprintf("the Manifest_Bucket %q policy does not grant account %s read (s3:GetObject) on its prefix %q; "+
		"the researcher's cross-account read grant is missing and the onboarding step which adds it to the Manifest_Bucket policy is incomplete",
		bucket, accountID, prefix)
	return check
}

// researcherCheckTestLambda discovers and invokes the Test_Lambda in the researcher account.
func researcherCheckTestLambda(ctx context.Context, lambdaClient internalaws.LambdaClient) []Check {
	var checks []Check

	discoveryCheck := Check{Name: "Test_Lambda Discovery"}
	functionName, err := researcherDiscoverTestLambda(ctx, lambdaClient)
	if err != nil {
		discoveryCheck.Passed = false
		discoveryCheck.Error = fmt.Sprintf("failed to list Lambda functions: %v", err)
		checks = append(checks, discoveryCheck)
		return checks
	}

	if functionName == "" {
		discoveryCheck.Passed = true
		discoveryCheck.Error = "UNAVAILABLE: Test_Lambda not found in target account; " +
			"deploy it via the consumer construct's deployTestLambda option for end-to-end validation"
		checks = append(checks, discoveryCheck)
		return checks
	}

	discoveryCheck.Passed = true
	checks = append(checks, discoveryCheck)

	invocationCheck := Check{Name: "Test_Lambda Invocation"}
	invokeOutput, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName: &functionName,
	})
	if err != nil {
		invocationCheck.Passed = false
		invocationCheck.Error = fmt.Sprintf("failed to invoke Test_Lambda %q: %v", functionName, err)
		checks = append(checks, invocationCheck)
		return checks
	}

	if invokeOutput.FunctionError != nil && *invokeOutput.FunctionError != "" {
		invocationCheck.Passed = false
		invocationCheck.Error = fmt.Sprintf("Test_Lambda returned function error: %s", *invokeOutput.FunctionError)
		if invokeOutput.Payload != nil {
			invocationCheck.Error += fmt.Sprintf(" (payload: %s)", string(invokeOutput.Payload))
		}
		checks = append(checks, invocationCheck)
		return checks
	}

	if invokeOutput.Payload == nil {
		invocationCheck.Passed = false
		invocationCheck.Error = "Test_Lambda returned nil payload"
		checks = append(checks, invocationCheck)
		return checks
	}

	var resp researcherLambdaResponse
	if err := json.Unmarshal(invokeOutput.Payload, &resp); err != nil {
		invocationCheck.Passed = false
		invocationCheck.Error = fmt.Sprintf("Test_Lambda response is not valid JSON: %v (payload: %s)",
			err, string(invokeOutput.Payload))
		checks = append(checks, invocationCheck)
		return checks
	}

	if resp.StatusCode != 200 {
		invocationCheck.Passed = false
		invocationCheck.Error = fmt.Sprintf("Test_Lambda returned statusCode %d (expected 200)", resp.StatusCode)
		checks = append(checks, invocationCheck)
		return checks
	}

	if resp.LatticeResponse == nil || len(resp.LatticeResponse) == 0 || string(resp.LatticeResponse) == "null" {
		invocationCheck.Passed = false
		invocationCheck.Error = "Test_Lambda returned statusCode 200 but latticeResponse is missing or empty"
		checks = append(checks, invocationCheck)
		return checks
	}

	invocationCheck.Passed = true
	checks = append(checks, invocationCheck)
	return checks
}

// researcherDiscoverTestLambda searches for a Lambda function with "TestLambda" in its name.
func researcherDiscoverTestLambda(ctx context.Context, lambdaClient internalaws.LambdaClient) (string, error) {
	var marker *string
	for {
		input := &lambda.ListFunctionsInput{
			Marker: marker,
		}
		output, err := lambdaClient.ListFunctions(ctx, input)
		if err != nil {
			return "", err
		}
		for _, fn := range output.Functions {
			if fn.FunctionName != nil && strings.Contains(*fn.FunctionName, "TestLambda") {
				return *fn.FunctionName, nil
			}
		}
		if output.NextMarker == nil || *output.NextMarker == "" {
			break
		}
		marker = output.NextMarker
	}
	return "", nil
}
