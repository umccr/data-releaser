package validate

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ram"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// This file collects configurable mocks for the AWS-client interfaces the validate
// package depends on. Each mock exposes function fields so individual tests can inject
// success or failure behaviour without a live AWS account. Defaults return successful,
// empty responses so a test only overrides the seam it cares about.

// --- STS ---------------------------------------------------------------------

type stubSTSClient struct {
	fn func(ctx context.Context, params *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)
}

var _ internalaws.STSClient = (*stubSTSClient)(nil)

func (m *stubSTSClient) GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if m.fn != nil {
		return m.fn(ctx, params)
	}
	acct := "534840902377"
	return &sts.GetCallerIdentityOutput{Account: &acct}, nil
}

// --- Athena ------------------------------------------------------------------

type stubAthenaClient struct {
	startFn      func(ctx context.Context, params *athena.StartQueryExecutionInput) (*athena.StartQueryExecutionOutput, error)
	getExecFn    func(ctx context.Context, params *athena.GetQueryExecutionInput) (*athena.GetQueryExecutionOutput, error)
	getResultsFn func(ctx context.Context, params *athena.GetQueryResultsInput) (*athena.GetQueryResultsOutput, error)
}

var _ internalaws.AthenaClient = (*stubAthenaClient)(nil)

func (m *stubAthenaClient) StartQueryExecution(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	if m.startFn != nil {
		return m.startFn(ctx, params)
	}
	id := "query-123"
	return &athena.StartQueryExecutionOutput{QueryExecutionId: &id}, nil
}

func (m *stubAthenaClient) GetQueryExecution(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	if m.getExecFn != nil {
		return m.getExecFn(ctx, params)
	}
	return &athena.GetQueryExecutionOutput{}, nil
}

func (m *stubAthenaClient) GetQueryResults(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	if m.getResultsFn != nil {
		return m.getResultsFn(ctx, params)
	}
	return &athena.GetQueryResultsOutput{}, nil
}

// --- S3 ----------------------------------------------------------------------

type stubS3Client struct {
	putObjectFn       func(ctx context.Context, params *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	deleteObjectFn    func(ctx context.Context, params *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	headObjectFn      func(ctx context.Context, params *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	headBucketFn      func(ctx context.Context, params *s3.HeadBucketInput) (*s3.HeadBucketOutput, error)
	getBucketPolicyFn func(ctx context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error)
}

var _ internalaws.S3Client = (*stubS3Client)(nil)

func (m *stubS3Client) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if m.putObjectFn != nil {
		return m.putObjectFn(ctx, params)
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *stubS3Client) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if m.deleteObjectFn != nil {
		return m.deleteObjectFn(ctx, params)
	}
	return &s3.DeleteObjectOutput{}, nil
}

func (m *stubS3Client) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headObjectFn != nil {
		return m.headObjectFn(ctx, params)
	}
	return &s3.HeadObjectOutput{}, nil
}

func (m *stubS3Client) HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if m.headBucketFn != nil {
		return m.headBucketFn(ctx, params)
	}
	return &s3.HeadBucketOutput{}, nil
}

func (m *stubS3Client) GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	if m.getBucketPolicyFn != nil {
		return m.getBucketPolicyFn(ctx, params)
	}
	return &s3.GetBucketPolicyOutput{}, nil
}

// --- DynamoDB ----------------------------------------------------------------

// stubDDBClient is a configurable DynamoDB mock. It keeps an in-memory item map keyed
// by shareId so it can back a real auth.ShareStore (used by the canary tests), while
// still allowing per-test overrides of DescribeTable and the other operations.
type stubDDBClient struct {
	items        map[string]map[string]types.AttributeValue
	describeFn   func(ctx context.Context, params *dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error)
	putItemFn    func(ctx context.Context, params *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	getItemFn    func(ctx context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	updateItemFn func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
}

var _ internalaws.DynamoDBClient = (*stubDDBClient)(nil)

func newStubDDBClient() *stubDDBClient {
	return &stubDDBClient{items: make(map[string]map[string]types.AttributeValue)}
}

func (m *stubDDBClient) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if m.putItemFn != nil {
		return m.putItemFn(ctx, params)
	}
	if m.items != nil {
		if idAttr, ok := params.Item["shareId"].(*types.AttributeValueMemberS); ok {
			m.items[idAttr.Value] = params.Item
		}
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (m *stubDDBClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, params)
	}
	if idAttr, ok := params.Key["shareId"].(*types.AttributeValueMemberS); ok {
		if item, exists := m.items[idAttr.Value]; exists {
			return &dynamodb.GetItemOutput{Item: item}, nil
		}
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (m *stubDDBClient) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.updateItemFn != nil {
		return m.updateItemFn(ctx, params)
	}
	// Apply revokedAt / expiresAt updates onto the stored item so state transitions
	// (e.g. the canary cleanup revoke) are observable in follow-up assertions.
	if idAttr, ok := params.Key["shareId"].(*types.AttributeValueMemberS); ok {
		if item, exists := m.items[idAttr.Value]; exists {
			if v, ok := params.ExpressionAttributeValues[":revokedAt"]; ok {
				item["revokedAt"] = v
			}
			if v, ok := params.ExpressionAttributeValues[":expiresAt"]; ok {
				item["expiresAt"] = v
			}
		}
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (m *stubDDBClient) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	var items []map[string]types.AttributeValue
	accountID := ""
	if v, ok := params.ExpressionAttributeValues[":accountId"].(*types.AttributeValueMemberS); ok {
		accountID = v.Value
	}
	for _, item := range m.items {
		if acct, ok := item["researcherAccountId"].(*types.AttributeValueMemberS); ok && acct.Value == accountID {
			items = append(items, item)
		}
	}
	return &dynamodb.QueryOutput{Items: items}, nil
}

func (m *stubDDBClient) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	var items []map[string]types.AttributeValue
	for _, item := range m.items {
		items = append(items, item)
	}
	return &dynamodb.ScanOutput{Items: items}, nil
}

func (m *stubDDBClient) DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if m.describeFn != nil {
		return m.describeFn(ctx, params)
	}
	return &dynamodb.DescribeTableOutput{}, nil
}

// --- API Gateway -------------------------------------------------------------

type stubAPIGatewayClient struct {
	getStageFn func(ctx context.Context, params *apigateway.GetStageInput) (*apigateway.GetStageOutput, error)
}

var _ internalaws.APIGatewayClient = (*stubAPIGatewayClient)(nil)

func (m *stubAPIGatewayClient) GetStage(ctx context.Context, params *apigateway.GetStageInput, optFns ...func(*apigateway.Options)) (*apigateway.GetStageOutput, error) {
	if m.getStageFn != nil {
		return m.getStageFn(ctx, params)
	}
	return &apigateway.GetStageOutput{}, nil
}

// --- RAM ---------------------------------------------------------------------

type stubRAMClient struct {
	fn func(ctx context.Context, params *ram.GetResourceShareAssociationsInput) (*ram.GetResourceShareAssociationsOutput, error)
}

var _ internalaws.RAMClient = (*stubRAMClient)(nil)

func (m *stubRAMClient) GetResourceShareAssociations(ctx context.Context, params *ram.GetResourceShareAssociationsInput, optFns ...func(*ram.Options)) (*ram.GetResourceShareAssociationsOutput, error) {
	if m.fn != nil {
		return m.fn(ctx, params)
	}
	return &ram.GetResourceShareAssociationsOutput{}, nil
}

// --- Lambda ------------------------------------------------------------------

type stubLambdaClient struct {
	invokeFn        func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error)
	listFunctionsFn func(ctx context.Context, params *lambda.ListFunctionsInput) (*lambda.ListFunctionsOutput, error)
}

var _ internalaws.LambdaClient = (*stubLambdaClient)(nil)

func (m *stubLambdaClient) Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	if m.invokeFn != nil {
		return m.invokeFn(ctx, params)
	}
	return &lambda.InvokeOutput{}, nil
}

func (m *stubLambdaClient) ListFunctions(ctx context.Context, params *lambda.ListFunctionsInput, optFns ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error) {
	if m.listFunctionsFn != nil {
		return m.listFunctionsFn(ctx, params)
	}
	return &lambda.ListFunctionsOutput{}, nil
}

// --- HTTP --------------------------------------------------------------------

type stubHTTPClient struct {
	doFn func(req *http.Request) (*http.Response, error)
}

var _ HTTPClient = (*stubHTTPClient)(nil)

func (m *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if m.doFn != nil {
		return m.doFn(req)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}
