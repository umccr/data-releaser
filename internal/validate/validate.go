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
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/warehouse"
)

// Check represents the result of a single validation check.
type Check struct {
	Name   string
	Passed bool
	Error  string
	// Skipped marks a check that could not meaningfully run in this context, as
	// distinct from one that ran and failed. A skipped check is not a failure and
	// must not gate the exit status. Error carries the reason.
	Skipped bool
}

// skip returns a skipped, non-failing check with the given reason.
func skip(name, reason string) Check {
	return Check{Name: name, Passed: true, Skipped: true, Error: reason}
}

// pass returns a check that ran and succeeded.
func pass(name string) Check {
	return Check{Name: name, Passed: true}
}

// fail returns a failing check whose Error is the formatted reason. Passed is left
// false, so a check only ever passes by saying so explicitly.
func fail(name, format string, args ...any) Check {
	return Check{Name: name, Error: fmt.Sprintf(format, args...)}
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
	checks = append(checks, checkAuthorizationService(ctx, opts.HTTPClient, opts.Config.Endpoints.Authorization, opts.Config.Validation))

	// Check 4: Htsget_Rs health/service-info endpoint
	checks = append(checks, checkHtsgetRs(ctx, opts.HTTPClient, opts.Config.Endpoints.HtsgetBaseURL, opts.Config.Validation))

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
	const name = "Credential Resolution (STS GetCallerIdentity)"

	output, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fail(name, "credential check failed: %v", err)
	}

	if output.Account == nil || *output.Account == "" {
		return fail(name, "credential check failed: STS returned empty account")
	}

	return pass(name)
}

// probeTimeout bounds how long the Athena reachability probe waits for a terminal
// query state before reporting the mart unreachable.
const probeTimeout = 60 * time.Second

// checkOrcaVaultMart verifies the OrcaVault Mart is reachable via Athena by running a
// probe query through the warehouse client, so submit-and-poll behaviour is shared with
// the real resolution path rather than reimplemented here.
func checkOrcaVaultMart(ctx context.Context, athenaClient internalaws.AthenaClient, whCfg config.WarehouseConfig) Check {
	const name = "OrcaVault Mart Reachability (Athena)"

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	client := warehouse.NewClient(athenaClient, whCfg)
	if _, err := client.RunQuery(probeCtx, client.ProbeQuery()); err != nil {
		return fail(name, "Athena probe query failed: %v", err)
	}

	return pass(name)
}

// checkAuthorizationService verifies the Authorization Service is reachable
// and returns a JSON response conforming to the expected schema.
//
// The authorization API is private by default: an interface endpoint inside the
// data account VPC is the only route to it, and the API's resource policy denies
// anything that arrives another way. From an operator's machine the hostname does
// not even resolve, so this check would fail on a healthy deployment — set
// validation.skipAuthorizationCheck for that topology and probe the callout from
// inside the VPC instead.
func checkAuthorizationService(ctx context.Context, httpClient HTTPClient, authEndpoint string, valCfg config.ValidationConfig) Check {
	const name = "Authorization Service Reachability"

	if valCfg.SkipAuthorizationCheck {
		return skip(name,
			"skipped: validation.skipAuthorizationCheck is set because the authorization "+
				"API is private and only reachable through its VPC interface endpoint")
	}

	if authEndpoint == "" {
		return fail(name, "authorization endpoint not configured")
	}

	// Issue a GET request to the authorization endpoint using a VPC ARN that belongs
	// to no share, so the response is a deny — which is still schema-conformant and
	// creates nothing.
	const probeVpcArn = "arn:aws:ec2:ap-southeast-2:000000000000:vpc/vpc-00000000000000000"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authEndpoint, nil)
	if err != nil {
		return fail(name, "failed to construct request: %v", err)
	}

	// Send the identity the same way htsget-rs does, as a forwarded context header,
	// so this check exercises the real code path rather than a probe-only one.
	req.Header.Set(auth.SourceVpcArnHeader, probeVpcArn)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fail(name, "authorization service unreachable: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(name, "failed to read authorization service response: %v", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(name, "authorization service returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Verify response is valid JSON with expected schema structure
	var authResp map[string]any
	if err := json.Unmarshal(body, &authResp); err != nil {
		return fail(name, "authorization service response is not valid JSON: %v", err)
	}

	// Check for the presence of the htsgetAuth field (schema conformance)
	if _, ok := authResp["htsgetAuth"]; !ok {
		return fail(name, "authorization service response missing 'htsgetAuth' field (schema non-conformant)")
	}

	return pass(name)
}

// checkHtsgetRs verifies the Htsget_Rs endpoint is healthy by hitting the
// /service-info endpoint.
//
// When htsget-rs is fronted by VPC Lattice its domain only resolves from inside an
// associated VPC, so this check cannot run from an operator's machine — it would fail
// on a healthy deployment. Set validation.skipHtsgetCheck for that topology and verify
// htsget-rs from inside the consumer VPC instead (the aws-vpc-lattice consumer
// construct's optional test Lambda does exactly this).
func checkHtsgetRs(ctx context.Context, httpClient HTTPClient, htsgetBaseURL string, valCfg config.ValidationConfig) Check {
	const name = "Htsget_Rs Health (service-info)"

	if valCfg.SkipHtsgetCheck {
		return skip(name,
			"skipped: validation.skipHtsgetCheck is set because htsget-rs is only "+
				"reachable from inside a VPC associated with its VPC Lattice service network")
	}

	if htsgetBaseURL == "" {
		return fail(name, "htsgetBaseUrl not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, htsgetBaseURL+"/service-info", nil)
	if err != nil {
		return fail(name, "failed to construct request: %v", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fail(name, "htsget-rs unreachable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fail(name, "htsget-rs /service-info returned HTTP %d (expected 200)", resp.StatusCode)
	}

	return pass(name)
}

// checkCrossAccountS3 verifies cross-account S3 access by issuing a HeadObject
// on a configured canary object in the Data Account.
func checkCrossAccountS3(ctx context.Context, s3Client internalaws.S3Client, valCfg config.ValidationConfig) Check {
	const name = "Cross-Account S3 Access (Data Account)"

	if valCfg.Canary == nil {
		return fail(name, "no canary configuration found (validation.canary not set in config)")
	}

	if valCfg.Canary.Bucket == "" || valCfg.Canary.Key == "" {
		return fail(name, "canary bucket or key not configured (validation.canary.bucket and validation.canary.key required)")
	}

	_, err := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &valCfg.Canary.Bucket,
		Key:    &valCfg.Canary.Key,
	})
	if err != nil {
		return fail(name, "S3 HeadObject on canary object failed: %v", err)
	}

	return pass(name)
}

// checkShareRecordsTable verifies the Share_Record DynamoDB table provisioned by the
// Infrastructure_Stack exists and is active (Req 15.10).
func checkShareRecordsTable(ctx context.Context, ddbClient internalaws.DynamoDBClient, tableName string) Check {
	const name = "Share_Record DynamoDB Table (Infrastructure_Stack)"

	if ddbClient == nil {
		return fail(name, "DynamoDB client not configured")
	}
	if tableName == "" {
		return fail(name, "Share_Record table name not configured")
	}

	_, err := ddbClient.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return fail(name, "DynamoDB table %q not found or inaccessible; "+
			"the Infrastructure_Stack DynamoDB table deployment step is missing or misconfigured: %v", tableName, err)
	}

	return pass(name)
}

// checkManifestBucket verifies the Manifest_Bucket provisioned by the Infrastructure_Stack
// exists and is writable by the Data Control identity via a HeadBucket call (Req 15.10).
func checkManifestBucket(ctx context.Context, s3Client internalaws.S3Client, bucket string) Check {
	const name = "Manifest_Bucket (Infrastructure_Stack)"

	if bucket == "" {
		return fail(name, "manifest.bucket not configured; the Manifest_Bucket name must be sourced from the Infrastructure_Stack output")
	}

	// Same reachability test the share preflight uses, so both agree on what a usable
	// Manifest_Bucket is.
	if err := manifest.NewWriter(s3Client).VerifyBucketWritable(ctx, bucket); err != nil {
		return fail(name, "manifest bucket %q missing or not writable by the Data Control identity; "+
			"the Infrastructure_Stack Manifest_Bucket deployment step is missing or misconfigured: %v", bucket, err)
	}

	return pass(name)
}

// checkAPIGatewayStage verifies the API Gateway REST API stage named in configuration
// exists (Req 15.10).
func checkAPIGatewayStage(ctx context.Context, apiClient internalaws.APIGatewayClient, endpoints config.EndpointsConfig) Check {
	const name = "API Gateway REST API Stage (Infrastructure_Stack)"

	if apiClient == nil {
		return fail(name, "API Gateway client not configured")
	}
	if endpoints.APIGatewayRestAPIID == "" {
		return fail(name, "endpoints.apiGatewayRestApiId not configured; cannot verify the API Gateway stage")
	}
	if endpoints.APIGatewayStageName == "" {
		return fail(name, "endpoints.apiGatewayStageName not configured; cannot verify the API Gateway stage")
	}

	_, err := apiClient.GetStage(ctx, &apigateway.GetStageInput{
		RestApiId: aws.String(endpoints.APIGatewayRestAPIID),
		StageName: aws.String(endpoints.APIGatewayStageName),
	})
	if err != nil {
		return fail(name, "API Gateway stage %q on REST API %q not found; "+
			"the Infrastructure_Stack API Gateway deployment step is missing or misconfigured: %v",
			endpoints.APIGatewayStageName, endpoints.APIGatewayRestAPIID, err)
	}

	return pass(name)
}
