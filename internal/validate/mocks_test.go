package validate

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ram"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// This file collects configurable mocks for the AWS-client interfaces the validate
// package depends on and nothing else does. The DynamoDB and S3 doubles, which several
// packages need, live in internal/awstest instead.
//
// Each mock exposes function fields so individual tests can inject success or failure
// behaviour without a live AWS account. Defaults return successful, empty responses so a
// test only overrides the seam it cares about.

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
