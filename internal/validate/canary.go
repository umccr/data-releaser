// Package validate - canary test-setup mode for end-to-end validation. Creates a short-lived synthetic Share_Record
// for the configured canary library, invokes the Test_Lambda to confirm canary data
// is retrievable end-to-end, and cleans up the synthetic share on exit (best-effort).
package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/warehouse"
)

// CanaryOpts holds the dependencies for the canary test-setup mode.
type CanaryOpts struct {
	ShareStore   *auth.ShareStore
	LambdaClient internalaws.LambdaClient
	Config       *config.CanaryConfig
	// TargetVpcArn is the EC2 VPC ARN the synthetic share is keyed on, resolved from
	// the canary account's researcher config. It is supplied rather than derived so the
	// canary shares one source of truth with the share command: this is the identity
	// VPC Lattice reports and the authorization callout matches on, and there is no
	// value it can safely be guessed from.
	TargetVpcArn string
}

// testLambdaResponse represents the expected response from the Test_Lambda, shared by
// the canary test-setup invocation and the researcher-side invocation check.
type testLambdaResponse struct {
	StatusCode      int             `json:"statusCode"`
	LatticeResponse json.RawMessage `json:"latticeResponse"`
}

// validateTestLambdaResponse reports whether a Test_Lambda response indicates a
// successful end-to-end fetch. The returned reason is empty when ok is true.
func validateTestLambdaResponse(resp testLambdaResponse) (ok bool, reason string) {
	if resp.StatusCode != 200 {
		return false, fmt.Sprintf("Test_Lambda returned statusCode %d (expected 200)", resp.StatusCode)
	}
	if len(resp.LatticeResponse) == 0 || string(resp.LatticeResponse) == "null" {
		return false, "Test_Lambda returned statusCode 200 but latticeResponse is missing or empty"
	}
	return true, ""
}

// RunCanaryTest creates a short-lived synthetic ShareRecord for the configured canary
// library, invokes the Test_Lambda to confirm canary data is retrievable end-to-end,
// and always cleans up the synthetic share (including on error, best-effort).
// Returns a Check indicating success/failure of the simulated share path.
func RunCanaryTest(ctx context.Context, opts CanaryOpts) Check {
	const name = "Canary Test-Setup (end-to-end share path)"

	if opts.Config == nil {
		return fail(name, "canary configuration not set (validation.canary required for --test-setup)")
	}

	if opts.Config.LibraryID == "" || opts.Config.Account == "" {
		return fail(name, "canary libraryId and account are required in configuration")
	}

	// Step 1: Create a short-lived synthetic Share_Record (5 min expiry).
	//
	// The synthetic share must be shaped exactly like a real one, or the canary
	// validates a path production never takes. Both fields below were previously built
	// ad hoc here and got the convention wrong: the ARN was a vpc-lattice association
	// ARN (VPC Lattice reports the EC2 VPC ARN as a request's source, so the callout
	// could never match it), and the prefix carried an "s3://bucket/" scheme that the
	// bucket-less htsget id cannot start with. The ARN is now supplied by the caller and
	// checked by ShareStore.Create; the prefix uses the same helper the share command
	// does.
	expiry := time.Now().UTC().Add(5 * time.Minute)

	s3Prefix := ""
	if opts.Config.Key != "" {
		s3Prefix = warehouse.DerivePrefix(opts.Config.Key)
	}

	createInput := auth.CreateInput{
		ResearcherAccountID: opts.Config.Account,
		TargetVpcArn:        opts.TargetVpcArn,
		LibraryIDs:          []string{opts.Config.LibraryID},
		S3Prefixes:          []string{s3Prefix},
		ExpiresAt:           &expiry,
	}

	record, err := opts.ShareStore.Create(ctx, createInput)
	if err != nil {
		return fail(name, "failed to create synthetic canary share: %v", err)
	}

	// Always clean up the synthetic share on exit (best-effort), including on error.
	defer func() {
		_, _ = opts.ShareStore.Revoke(ctx, record.ShareID)
	}()

	// Step 2: Discover the Test_Lambda in the canary account. It is required here —
	// without it there is no way to confirm the share path end to end.
	testLambdaName, err := discoverTestLambda(ctx, opts.LambdaClient)
	if err != nil {
		return fail(name, "failed to discover Test_Lambda: %v", err)
	}
	if testLambdaName == "" {
		return fail(name, "failed to discover Test_Lambda: %v", errNoTestLambda)
	}

	// Step 3: Invoke the Test_Lambda with the canary library ID
	resp, err := invokeCanaryTestLambda(ctx, opts.LambdaClient, testLambdaName, opts.Config.LibraryID)
	if err != nil {
		return fail(name, "failed to invoke Test_Lambda: %v", err)
	}

	// Step 4: Validate the response
	if ok, reason := validateTestLambdaResponse(*resp); !ok {
		return fail(name, "%s", reason)
	}

	return pass(name)
}

// testLambdaNameFragment is the substring identifying the Test_Lambda deployed by the
// consumer construct's deployTestLambda option. Matched case-insensitively, since the
// deployed physical name carries a CDK-generated suffix and casing is not guaranteed.
const testLambdaNameFragment = "testlambda"

// discoverTestLambda finds the Test_Lambda function in the target account, paging
// through ListFunctions. It returns ("", nil) when no such function exists: absence is
// not an error here, because the Test_Lambda is optional and each caller reports it
// differently (the canary treats it as fatal, researcher-side validation as
// UNAVAILABLE).
func discoverTestLambda(ctx context.Context, client internalaws.LambdaClient) (string, error) {
	pager := lambda.NewListFunctionsPaginator(client, &lambda.ListFunctionsInput{})
	for pager.HasMorePages() {
		output, err := pager.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("listing Lambda functions: %w", err)
		}

		for _, fn := range output.Functions {
			if fn.FunctionName != nil && strings.Contains(strings.ToLower(*fn.FunctionName), testLambdaNameFragment) {
				return *fn.FunctionName, nil
			}
		}
	}
	return "", nil
}

// errNoTestLambda reports that the optional Test_Lambda is not deployed.
var errNoTestLambda = fmt.Errorf("no Lambda function containing 'TestLambda' found; deploy it via the consumer construct's deployTestLambda option")

// invokeCanaryTestLambda invokes the Test_Lambda with a request for the canary library ID
// and parses the response.
func invokeCanaryTestLambda(ctx context.Context, client internalaws.LambdaClient, functionName string, libraryID string) (*testLambdaResponse, error) {
	// Build the payload: request the canary library's reads endpoint
	payload := map[string]string{
		"path": fmt.Sprintf("reads/%s", libraryID),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling Test_Lambda payload: %w", err)
	}

	output, err := client.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   &functionName,
		Payload:        payloadBytes,
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
	})
	if err != nil {
		return nil, fmt.Errorf("invoking Test_Lambda %q: %w", functionName, err)
	}

	if output.FunctionError != nil && *output.FunctionError != "" {
		return nil, fmt.Errorf("Test_Lambda execution error: %s", *output.FunctionError)
	}

	if output.Payload == nil {
		return nil, fmt.Errorf("Test_Lambda returned nil payload")
	}

	var resp testLambdaResponse
	if err := json.Unmarshal(output.Payload, &resp); err != nil {
		return nil, fmt.Errorf("parsing Test_Lambda response: %w (payload: %s)", err, string(output.Payload))
	}

	return &resp, nil
}
