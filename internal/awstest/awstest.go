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

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

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
	PutObjectFn       func(ctx context.Context, params *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	DeleteObjectFn    func(ctx context.Context, params *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	HeadObjectFn      func(ctx context.Context, params *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	HeadBucketFn      func(ctx context.Context, params *s3.HeadBucketInput) (*s3.HeadBucketOutput, error)
	GetBucketPolicyFn func(ctx context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error)

	// Call recordings, appended before the corresponding hook runs so they capture
	// failed calls too.
	PutCalls        []s3.PutObjectInput
	DeleteCalls     []s3.DeleteObjectInput
	HeadBucketCalls []s3.HeadBucketInput
}

var _ internalaws.S3Client = (*S3)(nil)

func (m *S3) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.PutCalls = append(m.PutCalls, *params)
	if m.PutObjectFn != nil {
		return m.PutObjectFn(ctx, params)
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *S3) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.DeleteCalls = append(m.DeleteCalls, *params)
	if m.DeleteObjectFn != nil {
		return m.DeleteObjectFn(ctx, params)
	}
	return &s3.DeleteObjectOutput{}, nil
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
