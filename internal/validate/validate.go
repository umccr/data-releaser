// Package validate implements the infrastructure validation (preflight) checks
// for the sharing infrastructure, including provider-side and researcher-side checks.
package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
)

// Check represents the result of a single validation check.
type Check struct {
	Name   string
	Passed bool
	Error  string
}

// HTTPClient is an interface for making HTTP requests (allows testing).
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// ValidateProviderOpts holds the dependencies for provider-side validation.
type ValidateProviderOpts struct {
	STSClient        internalaws.STSClient
	AthenaClient     internalaws.AthenaClient
	S3Client         internalaws.S3Client
	DynamoDBClient   internalaws.DynamoDBClient
	APIGatewayClient internalaws.APIGatewayClient
	HTTPClient       HTTPClient
	Config           *config.Config
	// ShareRecordsTableName is the DynamoDB table the Infrastructure_Stack provisions.
	ShareRecordsTableName string
}

// ValidateProvider runs all provider-side checks and returns per-check results.
// It continues running all checks even if some fail (Req 15.7).
func ValidateProvider(ctx context.Context, opts ValidateProviderOpts) []Check {
	var checks []Check

	// Check 1: Credential resolution (STS GetCallerIdentity)
	checks = append(checks, checkCredentials(ctx, opts.STSClient))

	// Check 2: OrcaVault Mart reachability (Athena probe query)
	checks = append(checks, checkOrcaVaultMart(ctx, opts.AthenaClient, opts.Config.Warehouse))

	// Check 3: Authorization Service reachability and schema conformance
	checks = append(checks, checkAuthorizationService(ctx, opts.HTTPClient, opts.Config.Endpoints.Authorization))

	// Check 4: Htsget_Rs health/service-info endpoint
	checks = append(checks, checkHtsgetRs(ctx, opts.HTTPClient, opts.Config.Endpoints.HtsgetBaseURL))

	// Check 5: Cross-account S3 HeadObject on canary object
	checks = append(checks, checkCrossAccountS3(ctx, opts.S3Client, opts.Config.Validation))

	// Check 6: Infrastructure_Stack resources present and consistent with config (Req 15.10)
	checks = append(checks, checkShareRecordsTable(ctx, opts.DynamoDBClient, opts.ShareRecordsTableName))
	checks = append(checks, checkManifestBucket(ctx, opts.S3Client, opts.Config.Manifest.Bucket))
	checks = append(checks, checkAPIGatewayStage(ctx, opts.APIGatewayClient, opts.Config.Endpoints))

	return checks
}

// checkCredentials verifies that AWS credentials resolve and are functional
// by calling STS GetCallerIdentity.
func checkCredentials(ctx context.Context, stsClient internalaws.STSClient) Check {
	check := Check{Name: "Credential Resolution (STS GetCallerIdentity)"}

	output, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("credential check failed: %v", err)
		return check
	}

	if output.Account == nil || *output.Account == "" {
		check.Passed = false
		check.Error = "credential check failed: STS returned empty account"
		return check
	}

	check.Passed = true
	return check
}

// checkOrcaVaultMart verifies the OrcaVault Mart is reachable via Athena
// by executing a probe query: SELECT 1 FROM mart.bam LIMIT 1
func checkOrcaVaultMart(ctx context.Context, athenaClient internalaws.AthenaClient, whCfg config.WarehouseConfig) Check {
	check := Check{Name: "OrcaVault Mart Reachability (Athena)"}

	probeSQL := fmt.Sprintf("SELECT 1 FROM %s.bam LIMIT 1", whCfg.Database)

	startInput := &athena.StartQueryExecutionInput{
		QueryString: &probeSQL,
		WorkGroup:   &whCfg.AthenaWorkgroup,
		QueryExecutionContext: &athenatypes.QueryExecutionContext{
			Database: &whCfg.Database,
			Catalog:  &whCfg.Catalog,
		},
	}

	startOutput, err := athenaClient.StartQueryExecution(ctx, startInput)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("Athena probe query failed to start: %v", err)
		return check
	}

	if startOutput.QueryExecutionId == nil {
		check.Passed = false
		check.Error = "Athena returned nil query execution ID"
		return check
	}

	queryID := *startOutput.QueryExecutionId

	// Poll for completion (with timeout)
	pollCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	getInput := &athena.GetQueryExecutionInput{
		QueryExecutionId: &queryID,
	}

	for {
		getOutput, err := athenaClient.GetQueryExecution(pollCtx, getInput)
		if err != nil {
			check.Passed = false
			check.Error = fmt.Sprintf("Athena probe query polling failed: %v", err)
			return check
		}

		if getOutput.QueryExecution == nil || getOutput.QueryExecution.Status == nil {
			check.Passed = false
			check.Error = "Athena returned nil query execution status"
			return check
		}

		state := getOutput.QueryExecution.Status.State
		switch state {
		case athenatypes.QueryExecutionStateSucceeded:
			check.Passed = true
			return check
		case athenatypes.QueryExecutionStateFailed:
			reason := "unknown error"
			if getOutput.QueryExecution.Status.StateChangeReason != nil {
				reason = *getOutput.QueryExecution.Status.StateChangeReason
			}
			check.Passed = false
			check.Error = fmt.Sprintf("Athena probe query failed: %s", reason)
			return check
		case athenatypes.QueryExecutionStateCancelled:
			check.Passed = false
			check.Error = "Athena probe query was cancelled"
			return check
		}

		select {
		case <-pollCtx.Done():
			check.Passed = false
			check.Error = "Athena probe query timed out waiting for completion"
			return check
		case <-time.After(1 * time.Second):
		}
	}
}

// checkAuthorizationService verifies the Authorization Service is reachable
// and returns a JSON response conforming to the expected schema.
func checkAuthorizationService(ctx context.Context, httpClient HTTPClient, authEndpoint string) Check {
	check := Check{Name: "Authorization Service Reachability"}

	if authEndpoint == "" {
		check.Passed = false
		check.Error = "authorization endpoint not configured"
		return check
	}

	// Issue a GET request to the authorization endpoint.
	// We use a non-existent VPC ARN to get a deny response which is still schema-conformant.
	url := authEndpoint + "?sourceVpcArn=arn:aws:vpc-lattice:ap-southeast-2:000000000000:servicenetworkserviceassociation/probe"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("failed to construct request: %v", err)
		return check
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("authorization service unreachable: %v", err)
		return check
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("failed to read authorization service response: %v", err)
		return check
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		check.Passed = false
		check.Error = fmt.Sprintf("authorization service returned HTTP %d: %s", resp.StatusCode, string(body))
		return check
	}

	// Verify response is valid JSON with expected schema structure
	var authResp map[string]interface{}
	if err := json.Unmarshal(body, &authResp); err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("authorization service response is not valid JSON: %v", err)
		return check
	}

	// Check for the presence of the htsgetAuth field (schema conformance)
	if _, ok := authResp["htsgetAuth"]; !ok {
		check.Passed = false
		check.Error = "authorization service response missing 'htsgetAuth' field (schema non-conformant)"
		return check
	}

	check.Passed = true
	return check
}

// checkHtsgetRs verifies the Htsget_Rs endpoint is healthy by hitting the
// /service-info endpoint.
func checkHtsgetRs(ctx context.Context, httpClient HTTPClient, htsgetBaseURL string) Check {
	check := Check{Name: "Htsget_Rs Health (service-info)"}

	if htsgetBaseURL == "" {
		check.Passed = false
		check.Error = "htsgetBaseUrl not configured"
		return check
	}

	url := htsgetBaseURL + "/service-info"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("failed to construct request: %v", err)
		return check
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("htsget-rs unreachable: %v", err)
		return check
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		check.Passed = false
		check.Error = fmt.Sprintf("htsget-rs /service-info returned HTTP %d (expected 200)", resp.StatusCode)
		return check
	}

	check.Passed = true
	return check
}

// checkCrossAccountS3 verifies cross-account S3 access by issuing a HeadObject
// on a configured canary object in the Data Account.
func checkCrossAccountS3(ctx context.Context, s3Client internalaws.S3Client, valCfg config.ValidationConfig) Check {
	check := Check{Name: "Cross-Account S3 Access (Data Account)"}

	if valCfg.Canary == nil {
		check.Passed = false
		check.Error = "no canary configuration found (validation.canary not set in config)"
		return check
	}

	if valCfg.Canary.Bucket == "" || valCfg.Canary.Key == "" {
		check.Passed = false
		check.Error = "canary bucket or key not configured (validation.canary.bucket and validation.canary.key required)"
		return check
	}

	input := &s3.HeadObjectInput{
		Bucket: &valCfg.Canary.Bucket,
		Key:    &valCfg.Canary.Key,
	}

	_, err := s3Client.HeadObject(ctx, input)
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("S3 HeadObject on canary object failed: %v", err)
		return check
	}

	check.Passed = true
	return check
}

// checkShareRecordsTable verifies the Share_Record DynamoDB table provisioned by the
// Infrastructure_Stack exists and is active (Req 15.10).
func checkShareRecordsTable(ctx context.Context, ddbClient internalaws.DynamoDBClient, tableName string) Check {
	check := Check{Name: "Share_Record DynamoDB Table (Infrastructure_Stack)"}

	if ddbClient == nil {
		check.Passed = false
		check.Error = "DynamoDB client not configured"
		return check
	}
	if tableName == "" {
		check.Passed = false
		check.Error = "Share_Record table name not configured"
		return check
	}

	_, err := ddbClient.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("DynamoDB table %q not found or inaccessible; "+
			"the Infrastructure_Stack DynamoDB table deployment step is missing or misconfigured: %v", tableName, err)
		return check
	}

	check.Passed = true
	return check
}

// checkManifestBucket verifies the Manifest_Bucket provisioned by the Infrastructure_Stack
// exists and is writable by the Data Control identity via a HeadBucket call (Req 15.10).
func checkManifestBucket(ctx context.Context, s3Client internalaws.S3Client, bucket string) Check {
	check := Check{Name: "Manifest_Bucket (Infrastructure_Stack)"}

	if bucket == "" {
		check.Passed = false
		check.Error = "manifest.bucket not configured; the Manifest_Bucket name must be sourced from the Infrastructure_Stack output"
		return check
	}

	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("manifest bucket %q missing or not writable by the Data Control identity; "+
			"the Infrastructure_Stack Manifest_Bucket deployment step is missing or misconfigured: %v", bucket, err)
		return check
	}

	check.Passed = true
	return check
}

// checkAPIGatewayStage verifies the API Gateway REST API stage named in configuration
// exists (Req 15.10).
func checkAPIGatewayStage(ctx context.Context, apiClient internalaws.APIGatewayClient, endpoints config.EndpointsConfig) Check {
	check := Check{Name: "API Gateway REST API Stage (Infrastructure_Stack)"}

	if apiClient == nil {
		check.Passed = false
		check.Error = "API Gateway client not configured"
		return check
	}
	if endpoints.APIGatewayRestAPIID == "" {
		check.Passed = false
		check.Error = "endpoints.apiGatewayRestApiId not configured; cannot verify the API Gateway stage"
		return check
	}
	if endpoints.APIGatewayStageName == "" {
		check.Passed = false
		check.Error = "endpoints.apiGatewayStageName not configured; cannot verify the API Gateway stage"
		return check
	}

	_, err := apiClient.GetStage(ctx, &apigateway.GetStageInput{
		RestApiId: aws.String(endpoints.APIGatewayRestAPIID),
		StageName: aws.String(endpoints.APIGatewayStageName),
	})
	if err != nil {
		check.Passed = false
		check.Error = fmt.Sprintf("API Gateway stage %q on REST API %q not found; "+
			"the Infrastructure_Stack API Gateway deployment step is missing or misconfigured: %v",
			endpoints.APIGatewayStageName, endpoints.APIGatewayRestAPIID, err)
		return check
	}

	check.Passed = true
	return check
}
