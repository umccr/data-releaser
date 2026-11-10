// Package aws provides an SDK client factory that resolves credentials via the
// standard AWS credential chain and returns typed service clients for all AWS
// services the Data Releaser uses.
package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ram"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// DefaultRegion is the Data Control account's region.
const DefaultRegion = "ap-southeast-2"

// AthenaClient defines the interface for Athena operations used by the application.
type AthenaClient interface {
	StartQueryExecution(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error)
	GetQueryExecution(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error)
	GetQueryResults(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error)
}

// DynamoDBClient defines the interface for DynamoDB operations used by the application.
type DynamoDBClient interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

// RAMClient defines the interface for RAM operations used by the application.
type RAMClient interface {
	GetResourceShareAssociations(ctx context.Context, params *ram.GetResourceShareAssociationsInput, optFns ...func(*ram.Options)) (*ram.GetResourceShareAssociationsOutput, error)
}

// S3Client defines the interface for S3 operations used by the application.
//
// Both delete operations are here because they are used for different things. A single
// delete removes one document this tool wrote and knows the key of, such as a
// researcher's manifest. The batched one empties a dataset's cache bucket, where the keys
// are whatever the copy delivered and there are thousands of them: a call per object
// would take a request per object and report a failure per object, which for the one
// operation that cannot be undone is both too slow to finish and too noisy to read.
type S3Client interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
	PutBucketPolicy(ctx context.Context, params *s3.PutBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
	DeleteBucketPolicy(ctx context.Context, params *s3.DeleteBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketPolicyOutput, error)
}

// KMSClient defines the interface for KMS operations used by the application.
//
// The read operations report a key's state and its deployed policy; the write
// operations replace that policy, disable the key, and schedule its deletion.
// Scheduling deletion is what renders objects encrypted under a key permanently
// unreadable once the pending window elapses, so it is deliberately the only
// destructive operation exposed here.
//
// Cancelling that deletion and enabling the key are the two halves of taking it back,
// and both are here because either alone leaves a key that decrypts nothing: a cancelled
// deletion returns a key to Disabled rather than to Enabled, which presents to a
// researcher exactly like the purge did.
type KMSClient interface {
	DescribeKey(ctx context.Context, params *kms.DescribeKeyInput, optFns ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
	GetKeyPolicy(ctx context.Context, params *kms.GetKeyPolicyInput, optFns ...func(*kms.Options)) (*kms.GetKeyPolicyOutput, error)
	PutKeyPolicy(ctx context.Context, params *kms.PutKeyPolicyInput, optFns ...func(*kms.Options)) (*kms.PutKeyPolicyOutput, error)
	ScheduleKeyDeletion(ctx context.Context, params *kms.ScheduleKeyDeletionInput, optFns ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error)
	CancelKeyDeletion(ctx context.Context, params *kms.CancelKeyDeletionInput, optFns ...func(*kms.Options)) (*kms.CancelKeyDeletionOutput, error)
	DisableKey(ctx context.Context, params *kms.DisableKeyInput, optFns ...func(*kms.Options)) (*kms.DisableKeyOutput, error)
	EnableKey(ctx context.Context, params *kms.EnableKeyInput, optFns ...func(*kms.Options)) (*kms.EnableKeyOutput, error)
}

// StepFunctionsClient defines the interface for Step Functions operations used by the
// application. An execution is started and then inspected on demand; nothing polls it to
// completion, because a copy run lasts hours to days and its terminal state arrives as an
// event instead.
type StepFunctionsClient interface {
	StartExecution(ctx context.Context, params *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error)
	DescribeExecution(ctx context.Context, params *sfn.DescribeExecutionInput, optFns ...func(*sfn.Options)) (*sfn.DescribeExecutionOutput, error)
}

// LambdaClient defines the interface for Lambda operations used by the application.
type LambdaClient interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
	ListFunctions(ctx context.Context, params *lambda.ListFunctionsInput, optFns ...func(*lambda.Options)) (*lambda.ListFunctionsOutput, error)
}

// APIGatewayClient defines the interface for API Gateway operations used by the application.
type APIGatewayClient interface {
	GetStage(ctx context.Context, params *apigateway.GetStageInput, optFns ...func(*apigateway.Options)) (*apigateway.GetStageOutput, error)
}

// STSClient defines the interface for STS operations used by the application.
type STSClient interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// ClientFactory creates configured AWS SDK clients using credentials resolved
// via the standard AWS credential chain. It wraps config.LoadDefaultConfig and
// supports an optional profile override.
type ClientFactory struct {
	cfg aws.Config
}

// NewClientFactory resolves AWS credentials and returns a ClientFactory.
// If profile is non-empty, credentials are loaded from that named profile.
// If region is empty, DefaultRegion (ap-southeast-2) is used.
// Returns a descriptive error if credential resolution fails.
func NewClientFactory(ctx context.Context, profile string, region string) (*ClientFactory, error) {
	if region == "" {
		region = DefaultRegion
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
	}

	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve AWS credentials: %w", err)
	}

	// Verify credentials can actually be retrieved (LoadDefaultConfig succeeds
	// even if the credential source is broken; retrieving creds surfaces the error).
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve AWS credentials: %w", err)
	}
	if !creds.HasKeys() {
		return nil, fmt.Errorf("failed to resolve AWS credentials: no access key found in credential chain")
	}

	return &ClientFactory{cfg: cfg}, nil
}

// AWSConfig returns the underlying aws.Config for advanced use cases.
func (f *ClientFactory) AWSConfig() aws.Config {
	return f.cfg
}

// AthenaClient returns a configured Athena client.
func (f *ClientFactory) AthenaClient() AthenaClient {
	return athena.NewFromConfig(f.cfg)
}

// DynamoDBClient returns a configured DynamoDB client.
func (f *ClientFactory) DynamoDBClient() DynamoDBClient {
	return dynamodb.NewFromConfig(f.cfg)
}

// RAMClient returns a configured RAM client.
func (f *ClientFactory) RAMClient() RAMClient {
	return ram.NewFromConfig(f.cfg)
}

// S3Client returns a configured S3 client.
func (f *ClientFactory) S3Client() S3Client {
	return s3.NewFromConfig(f.cfg)
}

// KMSClient returns a configured KMS client.
func (f *ClientFactory) KMSClient() KMSClient {
	return kms.NewFromConfig(f.cfg)
}

// StepFunctionsClient returns a configured Step Functions client.
func (f *ClientFactory) StepFunctionsClient() StepFunctionsClient {
	return sfn.NewFromConfig(f.cfg)
}

// LambdaClient returns a configured Lambda client.
func (f *ClientFactory) LambdaClient() LambdaClient {
	return lambda.NewFromConfig(f.cfg)
}

// APIGatewayClient returns a configured API Gateway client.
func (f *ClientFactory) APIGatewayClient() APIGatewayClient {
	return apigateway.NewFromConfig(f.cfg)
}

// STSClient returns a configured STS client.
func (f *ClientFactory) STSClient() STSClient {
	return sts.NewFromConfig(f.cfg)
}
