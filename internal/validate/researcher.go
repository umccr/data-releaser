package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/lambda"

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
	AccountID    string
}

// ValidateResearcher runs researcher-side validation checks and returns per-check results.
// It continues running all checks even if some fail (Req 15.7).
func ValidateResearcher(ctx context.Context, opts ValidateResearcherOpts) []Check {
	var checks []Check
	checks = append(checks, checkRAMAssociation(ctx, opts.RAMClient, opts.AccountID))
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
