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
	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/testgen"
)

// testLambdaLister returns a Lambda mock that reports a single deployed test lambda.
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

// canaryAccount is the researcher account the canary's synthetic share targets.
const canaryAccount = "534840902377"

func canaryConfig() *config.CanaryConfig {
	return &config.CanaryConfig{
		LibraryID: "L2400001",
		Account:   canaryAccount,
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
	ddb := awstest.NewDynamoDB()
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
	ddb := awstest.NewDynamoDB()
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

// TestRunCanaryTest_RejectsBadVpcArn covers the canary refusing to write a synthetic
// share whose target VPC ARN could never match an authorization callout. Such a share
// would make the canary report a failure that looks like a broken deployment.
func TestRunCanaryTest_RejectsBadVpcArn(t *testing.T) {
	for name, arn := range map[string]string{
		"empty": "",
		// The form the canary used to build for itself: VPC Lattice reports the EC2
		// VPC ARN as a request's source, never an association ARN.
		"lattice association": "arn:aws:vpc-lattice:ap-southeast-2:" + canaryAccount + ":servicenetworkserviceassociation/*",
		"wrong account":       testgen.VPCArnForAccount("999999999999"),
	} {
		t.Run(name, func(t *testing.T) {
			ddb := awstest.NewDynamoDB()
			check := RunCanaryTest(context.Background(), CanaryOpts{
				ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
				LambdaClient: &stubLambdaClient{},
				Config:       canaryConfig(),
				TargetVpcArn: arn,
			})
			if check.Passed {
				t.Errorf("expected failure for target VPC ARN %q", arn)
			}
			if len(ddb.Items) != 0 {
				t.Errorf("expected no synthetic share to be written, got %d", len(ddb.Items))
			}
		})
	}
}

func TestRunCanaryTest_HappyPathAndCleanup(t *testing.T) {
	ddb := awstest.NewDynamoDB()
	lambdaClient := testLambdaLister(func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return &lambda.InvokeOutput{
			Payload: []byte(`{"statusCode":200,"latticeResponse":{"htsgetAuth":[{"location":"s3://x"}]}}`),
		}, nil
	})

	opts := CanaryOpts{
		ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
		LambdaClient: lambdaClient,
		Config:       canaryConfig(),
		TargetVpcArn: testgen.VPCArnForAccount(canaryAccount),
	}

	check := RunCanaryTest(context.Background(), opts)
	if !check.Passed {
		t.Fatalf("expected canary test to pass, got failure: %s", check.Error)
	}

	// The synthetic share must be cleaned up (revoked) after the run.
	if len(ddb.Items) != 1 {
		t.Fatalf("expected exactly 1 synthetic share record, got %d", len(ddb.Items))
	}
	if countRevoked(ddb.Items) != 1 {
		t.Error("expected the synthetic canary share to be revoked (cleaned up) after the run")
	}
}

// TestRunCanaryTest_CleanupOnError verifies the synthetic share is still cleaned up when
// the end-to-end invocation fails, since cleanup is best-effort but unconditional.
func TestRunCanaryTest_CleanupOnError(t *testing.T) {
	ddb := awstest.NewDynamoDB()
	lambdaClient := testLambdaLister(func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return nil, errors.New("lambda invocation failed")
	})

	opts := CanaryOpts{
		ShareStore:   auth.NewShareStore(ddb, "ShareRecords"),
		LambdaClient: lambdaClient,
		Config:       canaryConfig(),
		TargetVpcArn: testgen.VPCArnForAccount(canaryAccount),
	}

	check := RunCanaryTest(context.Background(), opts)
	if check.Passed {
		t.Fatal("expected canary test to fail when test lambda invocation errors")
	}

	if len(ddb.Items) != 1 {
		t.Fatalf("expected exactly 1 synthetic share record, got %d", len(ddb.Items))
	}
	if countRevoked(ddb.Items) != 1 {
		t.Error("expected the synthetic canary share to be cleaned up even after an error")
	}
}

// TestDiscoverTestLambda covers the three outcomes callers distinguish, including a
// test lambda that only appears on a later ListFunctions page.
func TestDiscoverTestLambda(t *testing.T) {
	page := func(marker *string, names ...string) *lambda.ListFunctionsOutput {
		out := &lambda.ListFunctionsOutput{NextMarker: marker}
		for _, n := range names {
			out.Functions = append(out.Functions, lambdatypes.FunctionConfiguration{FunctionName: aws.String(n)})
		}
		return out
	}

	t.Run("found on a later page", func(t *testing.T) {
		calls := 0
		client := &stubLambdaClient{
			listFunctionsFn: func(ctx context.Context, params *lambda.ListFunctionsInput) (*lambda.ListFunctionsOutput, error) {
				calls++
				switch calls {
				case 1:
					return page(aws.String("marker-2"), "unrelated-one", "unrelated-two"), nil
				case 2:
					return page(nil, "data-releaser-testlambda-A1B2C3"), nil
				default:
					t.Fatal("unexpected extra ListFunctions call")
					return nil, nil
				}
			},
		}

		name, err := discoverTestLambda(context.Background(), client)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "data-releaser-testlambda-A1B2C3" {
			t.Errorf("expected the test lambda from page 2, got %q", name)
		}
		if calls != 2 {
			t.Errorf("expected 2 pages to be walked, got %d", calls)
		}
	})

	t.Run("absent reports no error", func(t *testing.T) {
		client := &stubLambdaClient{
			listFunctionsFn: func(ctx context.Context, params *lambda.ListFunctionsInput) (*lambda.ListFunctionsOutput, error) {
				return page(nil, "unrelated-one"), nil
			},
		}

		name, err := discoverTestLambda(context.Background(), client)
		if err != nil {
			t.Fatalf("absence must not be an error, got %v", err)
		}
		if name != "" {
			t.Errorf("expected no function name, got %q", name)
		}
	})

	t.Run("list error propagates", func(t *testing.T) {
		client := &stubLambdaClient{
			listFunctionsFn: func(ctx context.Context, params *lambda.ListFunctionsInput) (*lambda.ListFunctionsOutput, error) {
				return nil, errors.New("access denied")
			},
		}

		if _, err := discoverTestLambda(context.Background(), client); err == nil {
			t.Fatal("expected the ListFunctions error to propagate")
		}
	})
}
