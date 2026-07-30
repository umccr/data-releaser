package validate

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/umccr/data-releaser/internal/auth"
	"github.com/umccr/data-releaser/internal/config"
)

// testLambdaLister returns a Lambda mock that reports a single deployed Test_Lambda.
func testLambdaLister(invoke func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error)) *stubLambdaClient {
	return &stubLambdaClient{
		listFunctionsFn: func(ctx context.Context, params *lambda.ListFunctionsInput) (*lambda.ListFunctionsOutput, error) {
			return &lambda.ListFunctionsOutput{
				Functions: []lambdatypes.FunctionConfiguration{
					{FunctionName: aws.String("data-releaser-TestLambda")},
				},
			}, nil
		},
		invokeFn: invoke,
	}
}

func canaryConfig() *config.CanaryConfig {
	return &config.CanaryConfig{
		LibraryID: "L2400001",
		Account:   "534840902377",
		Bucket:    "data-account-bucket",
		Key:       "canary/reads.bam",
	}
}

// countRevoked returns the number of stored share records that carry a revokedAt attribute.
func countRevoked(items map[string]map[string]types.AttributeValue) int {
	n := 0
	for _, item := range items {
		if _, ok := item["revokedAt"]; ok {
			n++
		}
	}
	return n
}

func TestRunCanaryTest_NilConfig(t *testing.T) {
	ddb := newStubDDBClient()
	opts := CanaryOpts{
		ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
		LambdaClient: &stubLambdaClient{},
		Config:       nil,
	}
	check := RunCanaryTest(context.Background(), opts)
	if check.Passed {
		t.Fatal("expected failure when canary config is nil")
	}
}

func TestRunCanaryTest_MissingFields(t *testing.T) {
	ddb := newStubDDBClient()
	opts := CanaryOpts{
		ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
		LambdaClient: &stubLambdaClient{},
		Config:       &config.CanaryConfig{}, // no libraryId/account
	}
	check := RunCanaryTest(context.Background(), opts)
	if check.Passed {
		t.Fatal("expected failure when canary libraryId/account are missing")
	}
}

func TestRunCanaryTest_HappyPathAndCleanup(t *testing.T) {
	ddb := newStubDDBClient()
	lambdaClient := testLambdaLister(func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return &lambda.InvokeOutput{
			Payload: []byte(`{"statusCode":200,"latticeResponse":{"htsgetAuth":[{"location":"s3://x"}]}}`),
		}, nil
	})

	opts := CanaryOpts{
		ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
		LambdaClient: lambdaClient,
		Config:       canaryConfig(),
	}

	check := RunCanaryTest(context.Background(), opts)
	if !check.Passed {
		t.Fatalf("expected canary test to pass, got failure: %s", check.Error)
	}

	// The synthetic share must be cleaned up (revoked) after the run (Req 15.8).
	if len(ddb.items) != 1 {
		t.Fatalf("expected exactly 1 synthetic share record, got %d", len(ddb.items))
	}
	if countRevoked(ddb.items) != 1 {
		t.Error("expected the synthetic canary share to be revoked (cleaned up) after the run")
	}
}

// TestRunCanaryTest_CleanupOnError verifies the synthetic share is still cleaned up when
// the end-to-end invocation fails (Req 15.8 — cleanup on error, best-effort).
func TestRunCanaryTest_CleanupOnError(t *testing.T) {
	ddb := newStubDDBClient()
	lambdaClient := testLambdaLister(func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return nil, errors.New("lambda invocation failed")
	})

	opts := CanaryOpts{
		ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
		LambdaClient: lambdaClient,
		Config:       canaryConfig(),
	}

	check := RunCanaryTest(context.Background(), opts)
	if check.Passed {
		t.Fatal("expected canary test to fail when Test_Lambda invocation errors")
	}

	if len(ddb.items) != 1 {
		t.Fatalf("expected exactly 1 synthetic share record, got %d", len(ddb.items))
	}
	if countRevoked(ddb.items) != 1 {
		t.Error("expected the synthetic canary share to be cleaned up even after an error")
	}
}
