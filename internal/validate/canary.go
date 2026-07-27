// Package validate - canary test-setup mode for end-to-end validation.
// Implements Requirement 15.6 and 15.8: creates a short-lived synthetic Share_Record
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
)

// CanaryOpts holds the dependencies for the canary test-setup mode.
type CanaryOpts struct {
	ShareStore   *auth.ShareStore
	LambdaClient internalaws.LambdaClient
	Config       *config.CanaryConfig
}

// canaryLambdaResponse represents the expected response from the Test_Lambda
// used during canary test-setup invocation.
type canaryLambdaResponse struct {
	StatusCode      int             `json:"statusCode"`
	LatticeResponse json.RawMessage `json:"latticeResponse"`
}

// RunCanaryTest creates a short-lived synthetic ShareRecord for the configured canary
// library, invokes the Test_Lambda to confirm canary data is retrievable end-to-end,
// and always cleans up the synthetic share (including on error, best-effort).
// Returns a Check indicating success/failure of the simulated share path.
// Requirements: 15.6, 15.8
func RunCanaryTest(ctx context.Context, opts CanaryOpts) Check {
	check := Check{Name: "Canary Test-Setup (end-to-end share path)"}

	if opts.Config == nil {
		check.Passed = false
		check.Error = "canary configuration not set (validation.canary required for --test-setup)"
		return check
	}

	if opts.Config.LibraryID == "" || opts.Config.Account == "" {
		check.Passed = false
		check.Error = "canary libraryId and account are required in configuration"
		return check
	}

	// Step 1: Create a short-lived synthetic Share_Record (5 min expiry)
	expiry := time.Now().UTC().Add(5 * time.Minute)
	targetVpcArn := fmt.Sprintf("arn:aws:vpc-lattice:ap-southeast-2:%s:servicenetworkserviceassociation/*", opts.Config.Account)

	s3Prefix := ""
	if opts.Config.Bucket != "" && opts.Config.Key != "" {
		s3Prefix = fmt.Sprintf("s3://%s/%s", opts.Config.Bucket, canaryParentPrefix(opts.Config.Key))
	}

	createInput := auth.CreateInput{
		ResearcherAccountID: opts.Config.Account,
		TargetVpcArn:        targetVpcArn,
		LibraryIDs:          []string{opts.Config.LibraryID},
		S3Prefixes:          []string{s3Prefix},
		ExpiresAt:           &expiry,
	}

	record, err := opts.ShareStore.Create(ctx, createInput)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("failed to create synthetic canary share: %v", err)
		return check
	}

	// Always clean up the synthetic share on exit (best-effort), including on error.
	defer func() {
		_, _ = opts.ShareStore.Revoke(ctx, record.ShareID)
	}()

	// Step 2: Discover the Test_Lambda in the canary account
	testLambdaName, err := findCanaryTestLambda(ctx, opts.LambdaClient)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("failed to discover Test_Lambda: %v", err)
		return check
	}

	// Step 3: Invoke the Test_Lambda with the canary library ID
	resp, err := invokeCanaryTestLambda(ctx, opts.LambdaClient, testLambdaName, opts.Config.LibraryID)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("failed to invoke Test_Lambda: %v", err)
		return check
	}

	// Step 4: Validate the response
	if resp.StatusCode != 200 {
		check.Passed = false
		check.Error = fmt.Sprintf("Test_Lambda returned statusCode %d, expected 200", resp.StatusCode)
		return check
	}

	if resp.LatticeResponse == nil || len(resp.LatticeResponse) == 0 || string(resp.LatticeResponse) == "null" {
		check.Passed = false
		check.Error = "Test_Lambda returned statusCode 200 but latticeResponse is missing or empty"
		return check
	}

	check.Passed = true
	return check
}

// findCanaryTestLambda discovers the Test_Lambda function whose name contains "TestLambda".
func findCanaryTestLambda(ctx context.Context, client internalaws.LambdaClient) (string, error) {
	var marker *string
	for {
		output, err := client.ListFunctions(ctx, &lambda.ListFunctionsInput{
			Marker: marker,
		})
		if err != nil {
			return "", fmt.Errorf("listing Lambda functions: %w", err)
		}

		for _, fn := range output.Functions {
			if fn.FunctionName != nil && containsTestLambdaName(*fn.FunctionName) {
				return *fn.FunctionName, nil
			}
		}

		if output.NextMarker == nil || *output.NextMarker == "" {
			break
		}
		marker = output.NextMarker
	}

	return "", fmt.Errorf("no Lambda function containing 'TestLambda' found; deploy it via the consumer construct's deployTestLambda option")
}

// containsTestLambdaName checks whether a function name contains "TestLambda" (case-insensitive).
func containsTestLambdaName(name string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower("TestLambda"))
}

// invokeCanaryTestLambda invokes the Test_Lambda with a request for the canary library ID
// and parses the response.
func invokeCanaryTestLambda(ctx context.Context, client internalaws.LambdaClient, functionName string, libraryID string) (*canaryLambdaResponse, error) {
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

	var resp canaryLambdaResponse
	if err := json.Unmarshal(output.Payload, &resp); err != nil {
		return nil, fmt.Errorf("parsing Test_Lambda response: %w (payload: %s)", err, string(output.Payload))
	}

	return &resp, nil
}

// canaryParentPrefix extracts the parent prefix (directory) from an S3 key.
// For example, "data/sample1/file.bam" → "data/sample1/"
func canaryParentPrefix(key string) string {
	if key == "" {
		return ""
	}
	idx := strings.LastIndex(key, "/")
	if idx < 0 {
		return ""
	}
	return key[:idx+1]
}
