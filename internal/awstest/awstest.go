// Package awstest provides in-memory test doubles for the AWS client interfaces in
// internal/aws, shared by the tests of every package that talks to AWS.
//
// It is a regular (non-test) package for the same reason internal/testgen is: Go cannot
// import identifiers from another package's _test.go files, and three packages had
// independently grown near-identical DynamoDB and S3 stubs that then drifted apart in
// their default behaviour.
//
// Every operation has an optional function field. When it is nil the double falls back
// to a useful default, so a test only overrides the one seam it cares about.
package awstest

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// DynamoDB is an in-memory DynamoDB double that can back a real auth.ShareStore. It
// stores items keyed by shareId, filters Query by the researcherAccountId GSI key, and
// applies revokedAt/expiresAt updates onto the stored item so state transitions (revoke,
// extend) are observable in follow-up assertions.
type DynamoDB struct {
	// Items holds the persisted records keyed by shareId. Tests read it to assert on
	// what was written, and write to it to set up state a real update cannot reach.
	Items map[string]map[string]types.AttributeValue

	PutItemFn       func(ctx context.Context, params *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	GetItemFn       func(ctx context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	UpdateItemFn    func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	QueryFn         func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
	ScanFn          func(ctx context.Context, params *dynamodb.ScanInput) (*dynamodb.ScanOutput, error)
	DescribeTableFn func(ctx context.Context, params *dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error)
}

var _ internalaws.DynamoDBClient = (*DynamoDB)(nil)

// NewDynamoDB returns an empty in-memory DynamoDB double.
func NewDynamoDB() *DynamoDB {
	return &DynamoDB{Items: make(map[string]map[string]types.AttributeValue)}
}

// shareID extracts the shareId partition key from an item or key map.
func shareID(m map[string]types.AttributeValue) (string, bool) {
	attr, ok := m["shareId"].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return attr.Value, true
}

func (m *DynamoDB) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if m.PutItemFn != nil {
		return m.PutItemFn(ctx, params)
	}
	if id, ok := shareID(params.Item); ok {
		m.Items[id] = params.Item
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (m *DynamoDB) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if m.GetItemFn != nil {
		return m.GetItemFn(ctx, params)
	}
	if id, ok := shareID(params.Key); ok {
		if item, exists := m.Items[id]; exists {
			return &dynamodb.GetItemOutput{Item: item}, nil
		}
	}
	return &dynamodb.GetItemOutput{}, nil
}

// updatableAttrs are the attributes the double applies from an UpdateItem expression.
// They are the only two the ShareStore ever sets.
var updatableAttrs = []string{"revokedAt", "expiresAt"}

func (m *DynamoDB) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if m.UpdateItemFn != nil {
		return m.UpdateItemFn(ctx, params)
	}
	if id, ok := shareID(params.Key); ok {
		if item, exists := m.Items[id]; exists {
			for _, attr := range updatableAttrs {
				if v, ok := params.ExpressionAttributeValues[":"+attr]; ok {
					item[attr] = v
				}
			}
		}
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (m *DynamoDB) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if m.QueryFn != nil {
		return m.QueryFn(ctx, params)
	}
	// Stand in for the GSI2 researcherAccountId lookup: return every item for the
	// requested account. Ordering is unspecified, matching a real index closely enough
	// for callers that sort or filter themselves.
	accountID := ""
	if v, ok := params.ExpressionAttributeValues[":accountId"].(*types.AttributeValueMemberS); ok {
		accountID = v.Value
	}
	var items []map[string]types.AttributeValue
	for _, item := range m.Items {
		if acct, ok := item["researcherAccountId"].(*types.AttributeValueMemberS); ok && acct.Value == accountID {
			items = append(items, item)
		}
	}
	return &dynamodb.QueryOutput{Items: items}, nil
}

func (m *DynamoDB) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if m.ScanFn != nil {
		return m.ScanFn(ctx, params)
	}
	var items []map[string]types.AttributeValue
	for _, item := range m.Items {
		items = append(items, item)
	}
	return &dynamodb.ScanOutput{Items: items}, nil
}

func (m *DynamoDB) DescribeTable(ctx context.Context, params *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if m.DescribeTableFn != nil {
		return m.DescribeTableFn(ctx, params)
	}
	return &dynamodb.DescribeTableOutput{}, nil
}

// S3 is a configurable S3 double. Every operation succeeds with an empty response
// unless overridden, and the mutating calls are recorded so tests can assert on what
// was written or deleted.
type S3 struct {
	PutObjectFn          func(ctx context.Context, params *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	GetObjectFn          func(ctx context.Context, params *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	DeleteObjectFn       func(ctx context.Context, params *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	DeleteObjectsFn      func(ctx context.Context, params *s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error)
	HeadObjectFn         func(ctx context.Context, params *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	HeadBucketFn         func(ctx context.Context, params *s3.HeadBucketInput) (*s3.HeadBucketOutput, error)
	ListObjectsV2Fn      func(ctx context.Context, params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
	GetBucketPolicyFn    func(ctx context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error)
	PutBucketPolicyFn    func(ctx context.Context, params *s3.PutBucketPolicyInput) (*s3.PutBucketPolicyOutput, error)
	DeleteBucketPolicyFn func(ctx context.Context, params *s3.DeleteBucketPolicyInput) (*s3.DeleteBucketPolicyOutput, error)

	// Call recordings, appended before the corresponding hook runs so they capture
	// failed calls too. The batched deletes are recorded whole rather than flattened to
	// their keys, so a test can count the requests a bucket was emptied in as well as
	// the keys it was emptied of.
	PutCalls                []s3.PutObjectInput
	DeleteCalls             []s3.DeleteObjectInput
	DeleteObjectsCalls      []s3.DeleteObjectsInput
	HeadBucketCalls         []s3.HeadBucketInput
	PutBucketPolicyCalls    []s3.PutBucketPolicyInput
	DeleteBucketPolicyCalls []s3.DeleteBucketPolicyInput
}

var _ internalaws.S3Client = (*S3)(nil)

func (m *S3) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.PutCalls = append(m.PutCalls, *params)
	if m.PutObjectFn != nil {
		return m.PutObjectFn(ctx, params)
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *S3) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if m.GetObjectFn != nil {
		return m.GetObjectFn(ctx, params)
	}
	// An empty body rather than a nil one: callers close and read it unconditionally.
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (m *S3) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.DeleteCalls = append(m.DeleteCalls, *params)
	if m.DeleteObjectFn != nil {
		return m.DeleteObjectFn(ctx, params)
	}
	return &s3.DeleteObjectOutput{}, nil
}

// DeleteObjects reports every key in the request as deleted unless overridden, which is
// what the service does for a key that was there and for one that was not alike. Tests
// about a delete that partly failed override the hook and answer with Errors.
func (m *S3) DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	m.DeleteObjectsCalls = append(m.DeleteObjectsCalls, *params)
	if m.DeleteObjectsFn != nil {
		return m.DeleteObjectsFn(ctx, params)
	}
	var deleted []s3types.DeletedObject
	if params.Delete != nil {
		for _, object := range params.Delete.Objects {
			deleted = append(deleted, s3types.DeletedObject{Key: object.Key})
		}
	}
	return &s3.DeleteObjectsOutput{Deleted: deleted}, nil
}

func (m *S3) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.HeadObjectFn != nil {
		return m.HeadObjectFn(ctx, params)
	}
	return &s3.HeadObjectOutput{}, nil
}

func (m *S3) HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	m.HeadBucketCalls = append(m.HeadBucketCalls, *params)
	if m.HeadBucketFn != nil {
		return m.HeadBucketFn(ctx, params)
	}
	return &s3.HeadBucketOutput{}, nil
}

func (m *S3) GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	if m.GetBucketPolicyFn != nil {
		return m.GetBucketPolicyFn(ctx, params)
	}
	return &s3.GetBucketPolicyOutput{}, nil
}

func (m *S3) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.ListObjectsV2Fn != nil {
		return m.ListObjectsV2Fn(ctx, params)
	}
	return &s3.ListObjectsV2Output{}, nil
}

func (m *S3) PutBucketPolicy(ctx context.Context, params *s3.PutBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	m.PutBucketPolicyCalls = append(m.PutBucketPolicyCalls, *params)
	if m.PutBucketPolicyFn != nil {
		return m.PutBucketPolicyFn(ctx, params)
	}
	return &s3.PutBucketPolicyOutput{}, nil
}

func (m *S3) DeleteBucketPolicy(ctx context.Context, params *s3.DeleteBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.DeleteBucketPolicyOutput, error) {
	m.DeleteBucketPolicyCalls = append(m.DeleteBucketPolicyCalls, *params)
	if m.DeleteBucketPolicyFn != nil {
		return m.DeleteBucketPolicyFn(ctx, params)
	}
	return &s3.DeleteBucketPolicyOutput{}, nil
}

// KMS is a configurable KMS double. DescribeKey reports an enabled key unless
// overridden, because a key that is absent or unusable is the exception rather than the
// case most tests set up. The policy write, disable, and scheduled-deletion calls are
// recorded so a test can assert that access was changed, or that it was left alone.
//
// The two calls that take a scheduled deletion back are recorded separately from each
// other, because the order and the presence of both is the whole of what a restore has
// to get right: cancelling leaves a key disabled, so a run that cancelled and stopped
// there is indistinguishable to a researcher from one that never ran.
type KMS struct {
	DescribeKeyFn      func(ctx context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error)
	GetKeyPolicyFn     func(ctx context.Context, params *kms.GetKeyPolicyInput) (*kms.GetKeyPolicyOutput, error)
	PutKeyPolicyFn     func(ctx context.Context, params *kms.PutKeyPolicyInput) (*kms.PutKeyPolicyOutput, error)
	ScheduleDeletionFn func(ctx context.Context, params *kms.ScheduleKeyDeletionInput) (*kms.ScheduleKeyDeletionOutput, error)
	CancelDeletionFn   func(ctx context.Context, params *kms.CancelKeyDeletionInput) (*kms.CancelKeyDeletionOutput, error)
	DisableKeyFn       func(ctx context.Context, params *kms.DisableKeyInput) (*kms.DisableKeyOutput, error)
	EnableKeyFn        func(ctx context.Context, params *kms.EnableKeyInput) (*kms.EnableKeyOutput, error)

	// Call recordings, appended before the corresponding hook runs so they capture
	// failed calls too.
	PutKeyPolicyCalls     []kms.PutKeyPolicyInput
	ScheduleDeletionCalls []kms.ScheduleKeyDeletionInput
	CancelDeletionCalls   []kms.CancelKeyDeletionInput
	DisableKeyCalls       []kms.DisableKeyInput
	EnableKeyCalls        []kms.EnableKeyInput
}

var _ internalaws.KMSClient = (*KMS)(nil)

// defaultPendingWindowDays matches the KMS default when a request omits the window.
const defaultPendingWindowDays = 30

func (m *KMS) DescribeKey(ctx context.Context, params *kms.DescribeKeyInput, optFns ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
	if m.DescribeKeyFn != nil {
		return m.DescribeKeyFn(ctx, params)
	}
	return &kms.DescribeKeyOutput{
		KeyMetadata: &kmstypes.KeyMetadata{
			KeyId:    params.KeyId,
			Enabled:  true,
			KeyState: kmstypes.KeyStateEnabled,
		},
	}, nil
}

func (m *KMS) GetKeyPolicy(ctx context.Context, params *kms.GetKeyPolicyInput, optFns ...func(*kms.Options)) (*kms.GetKeyPolicyOutput, error) {
	if m.GetKeyPolicyFn != nil {
		return m.GetKeyPolicyFn(ctx, params)
	}
	return &kms.GetKeyPolicyOutput{}, nil
}

func (m *KMS) PutKeyPolicy(ctx context.Context, params *kms.PutKeyPolicyInput, optFns ...func(*kms.Options)) (*kms.PutKeyPolicyOutput, error) {
	m.PutKeyPolicyCalls = append(m.PutKeyPolicyCalls, *params)
	if m.PutKeyPolicyFn != nil {
		return m.PutKeyPolicyFn(ctx, params)
	}
	return &kms.PutKeyPolicyOutput{}, nil
}

// ScheduleKeyDeletion reports a deletion date the requested number of days ahead, so a
// caller that records when deletion becomes irreversible has something plausible to
// record. Tests asserting on an exact date should override the hook.
func (m *KMS) ScheduleKeyDeletion(ctx context.Context, params *kms.ScheduleKeyDeletionInput, optFns ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error) {
	m.ScheduleDeletionCalls = append(m.ScheduleDeletionCalls, *params)
	if m.ScheduleDeletionFn != nil {
		return m.ScheduleDeletionFn(ctx, params)
	}
	window := int32(defaultPendingWindowDays)
	if params.PendingWindowInDays != nil {
		window = *params.PendingWindowInDays
	}
	deletionDate := time.Now().UTC().AddDate(0, 0, int(window))
	return &kms.ScheduleKeyDeletionOutput{
		KeyId:               params.KeyId,
		DeletionDate:        &deletionDate,
		KeyState:            kmstypes.KeyStatePendingDeletion,
		PendingWindowInDays: &window,
	}, nil
}

// CancelKeyDeletion echoes the key it was given and reports nothing else, which is all
// the service returns: a cancellation leaves the key disabled, and what state it is
// actually in afterwards is only ever learned by describing it again. A test that cares
// says so through DescribeKeyFn.
func (m *KMS) CancelKeyDeletion(ctx context.Context, params *kms.CancelKeyDeletionInput, optFns ...func(*kms.Options)) (*kms.CancelKeyDeletionOutput, error) {
	m.CancelDeletionCalls = append(m.CancelDeletionCalls, *params)
	if m.CancelDeletionFn != nil {
		return m.CancelDeletionFn(ctx, params)
	}
	return &kms.CancelKeyDeletionOutput{KeyId: params.KeyId}, nil
}

func (m *KMS) DisableKey(ctx context.Context, params *kms.DisableKeyInput, optFns ...func(*kms.Options)) (*kms.DisableKeyOutput, error) {
	m.DisableKeyCalls = append(m.DisableKeyCalls, *params)
	if m.DisableKeyFn != nil {
		return m.DisableKeyFn(ctx, params)
	}
	return &kms.DisableKeyOutput{}, nil
}

func (m *KMS) EnableKey(ctx context.Context, params *kms.EnableKeyInput, optFns ...func(*kms.Options)) (*kms.EnableKeyOutput, error) {
	m.EnableKeyCalls = append(m.EnableKeyCalls, *params)
	if m.EnableKeyFn != nil {
		return m.EnableKeyFn(ctx, params)
	}
	return &kms.EnableKeyOutput{}, nil
}

// StepFunctions is a configurable Step Functions double. StartExecution succeeds and
// returns an execution ARN derived from the requested name, so a caller that stores the
// execution reference stores something distinguishable per call.
type StepFunctions struct {
	StartExecutionFn    func(ctx context.Context, params *sfn.StartExecutionInput) (*sfn.StartExecutionOutput, error)
	DescribeExecutionFn func(ctx context.Context, params *sfn.DescribeExecutionInput) (*sfn.DescribeExecutionOutput, error)

	// StartCalls records every start attempt, appended before the hook runs, so a test
	// can assert that no copy was started as easily as that one was.
	StartCalls []sfn.StartExecutionInput
}

var _ internalaws.StepFunctionsClient = (*StepFunctions)(nil)

func (m *StepFunctions) StartExecution(ctx context.Context, params *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error) {
	m.StartCalls = append(m.StartCalls, *params)
	if m.StartExecutionFn != nil {
		return m.StartExecutionFn(ctx, params)
	}
	name := "execution"
	if params.Name != nil {
		name = *params.Name
	}
	startDate := time.Now().UTC()
	return &sfn.StartExecutionOutput{
		ExecutionArn: aws.String("arn:aws:states:ap-southeast-2:000000000000:execution:copy:" + name),
		StartDate:    &startDate,
	}, nil
}

func (m *StepFunctions) DescribeExecution(ctx context.Context, params *sfn.DescribeExecutionInput, optFns ...func(*sfn.Options)) (*sfn.DescribeExecutionOutput, error) {
	if m.DescribeExecutionFn != nil {
		return m.DescribeExecutionFn(ctx, params)
	}
	return &sfn.DescribeExecutionOutput{ExecutionArn: params.ExecutionArn}, nil
}
