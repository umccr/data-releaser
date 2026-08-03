package validate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/umccr/data-releaser/internal/config"
)

// jsonResponse builds an *http.Response with the given status and body.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// healthyHTTPClient returns 200 responses for both the authorization service and the
// htsget-rs service-info endpoint, routing on the request path.
func healthyHTTPClient() *stubHTTPClient {
	return &stubHTTPClient{
		doFn: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "service-info") {
				return jsonResponse(http.StatusOK, `{"id":"htsget"}`), nil
			}
			return jsonResponse(http.StatusOK, `{"htsgetAuth":[]}`), nil
		},
	}
}

// succeedingAthena returns an Athena mock whose probe query immediately reports SUCCEEDED.
func succeedingAthena() *stubAthenaClient {
	return &stubAthenaClient{
		getExecFn: func(ctx context.Context, params *athena.GetQueryExecutionInput) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: athenatypes.QueryExecutionStateSucceeded,
					},
				},
			}, nil
		},
	}
}

func healthyProviderConfig() *config.Config {
	return &config.Config{
		Warehouse: config.WarehouseConfig{
			AthenaWorkgroup: "primary",
			Database:        "mart",
			Catalog:         "AwsDataCatalog",
		},
		Endpoints: config.EndpointsConfig{
			Authorization:       "https://auth.example.com/authorize",
			HtsgetBaseURL:       "https://htsget.example.com",
			APIGatewayRestAPIID: "abc123",
			APIGatewayStageName: "prod",
		},
		Validation: config.ValidationConfig{
			Canary: &config.CanaryConfig{
				Bucket: "data-account-bucket",
				Key:    "canary/reads.bam",
			},
		},
		Manifest: config.ManifestConfig{
			Bucket:      "data-control-manifest-bucket",
			KeyTemplate: config.DefaultManifestKeyTemplate,
		},
	}
}

func healthyProviderOpts() ValidateProviderOpts {
	return ValidateProviderOpts{
		STSClient:             &stubSTSClient{},
		AthenaClient:          succeedingAthena(),
		S3Client:              &stubS3Client{},
		DynamoDBClient:        newStubDDBClient(),
		APIGatewayClient:      &stubAPIGatewayClient{},
		HTTPClient:            healthyHTTPClient(),
		Config:                healthyProviderConfig(),
		ShareRecordsTableName: "ShareRecords",
	}
}

// findCheck returns the check whose name contains the given substring.
func findCheck(t *testing.T, checks []Check, nameContains string) Check {
	t.Helper()
	for _, c := range checks {
		if strings.Contains(c.Name, nameContains) {
			return c
		}
	}
	t.Fatalf("no check found containing %q; got %d checks", nameContains, len(checks))
	return Check{}
}

func TestValidateProvider_AllPass(t *testing.T) {
	checks := ValidateProvider(context.Background(), healthyProviderOpts())

	if len(checks) != 8 {
		t.Fatalf("expected 8 provider checks, got %d", len(checks))
	}
	for _, c := range checks {
		if !c.Passed {
			t.Errorf("expected check %q to pass, got failure: %s", c.Name, c.Error)
		}
	}
}

// TestValidateProvider_ContinuesOnFailure verifies that a failure in an early check
// (credential resolution) does not abort the run — every check still executes and
// reports its own pass/fail (Req 15.7).
func TestValidateProvider_ContinuesOnFailure(t *testing.T) {
	opts := healthyProviderOpts()
	opts.STSClient = &stubSTSClient{
		fn: func(ctx context.Context, params *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
			return nil, errors.New("no credentials in chain")
		},
	}

	checks := ValidateProvider(context.Background(), opts)

	if len(checks) != 8 {
		t.Fatalf("expected all 8 checks to run despite an early failure, got %d", len(checks))
	}

	cred := findCheck(t, checks, "Credential Resolution")
	if cred.Passed {
		t.Error("expected the credential check to fail")
	}

	// Every other check must still have executed and passed.
	for _, c := range checks {
		if strings.Contains(c.Name, "Credential Resolution") {
			continue
		}
		if !c.Passed {
			t.Errorf("expected check %q to still run and pass, got failure: %s", c.Name, c.Error)
		}
	}
}

// The authorization API is a private endpoint, so from an operator's machine the
// probe fails on a perfectly healthy deployment. The skip must report as skipped
// rather than passed-by-accident, must not gate the exit status, and must not
// issue the request at all — a hostname that does not resolve would otherwise
// stall the whole run.
func TestValidateProvider_SkipAuthorizationCheck(t *testing.T) {
	opts := healthyProviderOpts()
	opts.Config.Validation.SkipAuthorizationCheck = true

	called := false
	opts.HTTPClient = &stubHTTPClient{
		doFn: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "service-info") {
				return jsonResponse(http.StatusOK, `{"id":"htsget"}`), nil
			}
			called = true
			return nil, errors.New("no such host")
		},
	}

	checks := ValidateProvider(context.Background(), opts)

	authCheck := findCheck(t, checks, "Authorization Service")
	if !authCheck.Skipped {
		t.Error("expected the authorization check to report as skipped")
	}
	if !authCheck.Passed {
		t.Error("a skipped check must not gate the exit status")
	}
	if !strings.Contains(authCheck.Error, "skipAuthorizationCheck") {
		t.Errorf("expected the reason to name the setting, got %q", authCheck.Error)
	}
	if called {
		t.Error("expected no HTTP request to the private endpoint when skipping")
	}
}

// Without the flag the probe still runs, so a genuinely broken deployment is not
// hidden by the option existing.
func TestValidateProvider_AuthorizationCheckRunsByDefault(t *testing.T) {
	opts := healthyProviderOpts()
	opts.HTTPClient = &stubHTTPClient{
		doFn: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "service-info") {
				return jsonResponse(http.StatusOK, `{"id":"htsget"}`), nil
			}
			return nil, errors.New("no such host")
		},
	}

	authCheck := findCheck(t, ValidateProvider(context.Background(), opts), "Authorization Service")
	if authCheck.Skipped || authCheck.Passed {
		t.Errorf("expected an unreachable authorization service to fail, got %+v", authCheck)
	}
}

func TestValidateProvider_ReportsPerCheckFailures(t *testing.T) {
	opts := healthyProviderOpts()

	// Fail three independent infrastructure checks; the rest stay healthy.
	ddb := newStubDDBClient()
	ddb.describeFn = func(ctx context.Context, params *dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
		return nil, errors.New("ResourceNotFoundException: table missing")
	}
	opts.DynamoDBClient = ddb

	opts.S3Client = &stubS3Client{
		headBucketFn: func(ctx context.Context, params *s3.HeadBucketInput) (*s3.HeadBucketOutput, error) {
			return nil, errors.New("NotFound: bucket missing")
		},
	}

	opts.APIGatewayClient = &stubAPIGatewayClient{
		getStageFn: func(ctx context.Context, params *apigateway.GetStageInput) (*apigateway.GetStageOutput, error) {
			return nil, errors.New("NotFoundException: stage missing")
		},
	}

	checks := ValidateProvider(context.Background(), opts)

	table := findCheck(t, checks, "DynamoDB Table")
	if table.Passed || table.Error == "" {
		t.Error("expected the DynamoDB table check to fail with a descriptive error")
	}
	bucket := findCheck(t, checks, "Manifest_Bucket")
	if bucket.Passed || bucket.Error == "" {
		t.Error("expected the manifest bucket check to fail with a descriptive error")
	}
	stage := findCheck(t, checks, "API Gateway")
	if stage.Passed || stage.Error == "" {
		t.Error("expected the API Gateway stage check to fail with a descriptive error")
	}

	// Credential and Athena checks are independent and should still pass.
	if c := findCheck(t, checks, "Credential Resolution"); !c.Passed {
		t.Errorf("expected credential check to pass, got: %s", c.Error)
	}
}
