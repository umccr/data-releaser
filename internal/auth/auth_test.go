package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/model"
)

func TestCreate_Success(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	expiry := time.Now().UTC().Add(7 * 24 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
		ExpiresAt:           &expiry,
	}

	record, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify ULID was generated
	if record.ShareID == "" {
		t.Error("expected non-empty ShareID (ULID)")
	}
	if len(record.ShareID) != 26 {
		t.Errorf("expected 26-char ULID, got %d chars: %s", len(record.ShareID), record.ShareID)
	}

	// Verify fields
	if record.ResearcherAccountID != input.ResearcherAccountID {
		t.Errorf("ResearcherAccountID: expected %q, got %q", input.ResearcherAccountID, record.ResearcherAccountID)
	}
	if record.TargetVpcArn != input.TargetVpcArn {
		t.Errorf("TargetVpcArn: expected %q, got %q", input.TargetVpcArn, record.TargetVpcArn)
	}
	if len(record.LibraryIDs) != 2 {
		t.Errorf("expected 2 library IDs, got %d", len(record.LibraryIDs))
	}
	if len(record.S3Prefixes) != 2 {
		t.Errorf("expected 2 S3 prefixes, got %d", len(record.S3Prefixes))
	}
	if record.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
	if record.ExpiresAt == nil {
		t.Error("expected non-nil ExpiresAt")
	}
	if record.RevokedAt != nil {
		t.Error("expected nil RevokedAt")
	}

	// Verify it was stored in DynamoDB
	if len(mock.Items) != 1 {
		t.Errorf("expected 1 item stored, got %d", len(mock.Items))
	}
}

func TestCreate_Unlimited(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           nil, // unlimited
	}

	record, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if record.ExpiresAt != nil {
		t.Error("expected nil ExpiresAt for unlimited share")
	}
}

func TestCreate_DynamoDBError(t *testing.T) {
	mock := awstest.NewDynamoDB()
	mock.PutItemFn = func(ctx context.Context, params *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, fmt.Errorf("dynamodb unavailable")
	}
	store := NewShareStore(mock, "ShareRecords")

	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	_, err := store.Create(context.Background(), input)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGet_Success(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create a record first
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Retrieve it
	retrieved, err := store.Get(context.Background(), created.ShareID)
	if err != nil {
		t.Fatalf("unexpected error getting: %v", err)
	}

	if retrieved.ShareID != created.ShareID {
		t.Errorf("ShareID: expected %q, got %q", created.ShareID, retrieved.ShareID)
	}
	if retrieved.ResearcherAccountID != created.ResearcherAccountID {
		t.Errorf("ResearcherAccountID: expected %q, got %q", created.ResearcherAccountID, retrieved.ResearcherAccountID)
	}
}

func TestGet_NotFound(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	_, err := store.Get(context.Background(), "nonexistent-id")
	if err != ErrShareNotFound {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}
}

func TestRevoke_Success(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create an active share
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	revoked, err := store.Revoke(context.Background(), created.ShareID)
	if err != nil {
		t.Fatalf("unexpected error revoking: %v", err)
	}

	if revoked.RevokedAt == nil {
		t.Error("expected non-nil RevokedAt after revocation")
	}

	now := time.Now().UTC()
	status := revoked.EffectiveStatus(now)
	if status != model.ShareStatusRevoked {
		t.Errorf("expected status %q, got %q", model.ShareStatusRevoked, status)
	}
}

func TestRevoke_NotFound(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	_, err := store.Revoke(context.Background(), "nonexistent-id")
	if err != ErrShareNotFound {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}
}

func TestRevoke_AlreadyRevoked(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create and then revoke a share
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Revoke it
	_, err = store.Revoke(context.Background(), created.ShareID)
	if err != nil {
		t.Fatalf("unexpected error revoking: %v", err)
	}

	// Mock the GetItem to return the revoked record
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[created.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}

	// Try to revoke again
	_, err = store.Revoke(context.Background(), created.ShareID)
	if err != ErrShareAlreadyRevoked {
		t.Errorf("expected ErrShareAlreadyRevoked, got %v", err)
	}
}

func TestRevoke_Expired(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create a share with an expiry in the past
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &pastExpiry,
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	_, err = store.Revoke(context.Background(), created.ShareID)
	if err != ErrShareExpired {
		t.Errorf("expected ErrShareExpired, got %v", err)
	}
}

func TestExtend_Success(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create an active share with future expiry
	futureExpiry := time.Now().UTC().Add(24 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &futureExpiry,
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Extend by 7 days
	extended, err := store.Extend(context.Background(), created.ShareID, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error extending: %v", err)
	}

	if extended.ExpiresAt == nil {
		t.Fatal("expected non-nil ExpiresAt after extension")
	}

	// The new expiry should be approximately now + 7 days
	expectedExpiry := time.Now().UTC().Add(7 * 24 * time.Hour)
	diff := extended.ExpiresAt.Sub(expectedExpiry)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected expiry around %v, got %v (diff: %v)", expectedExpiry, *extended.ExpiresAt, diff)
	}
}

func TestExtend_NotFound(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	_, err := store.Extend(context.Background(), "nonexistent-id", 7*24*time.Hour)
	if err != ErrShareNotFound {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}
}

func TestExtend_AlreadyRevoked(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create and revoke
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Simulate revoked state
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[created.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}

	_, err = store.Extend(context.Background(), created.ShareID, 7*24*time.Hour)
	if err != ErrShareAlreadyRevoked {
		t.Errorf("expected ErrShareAlreadyRevoked, got %v", err)
	}
}

func TestExtend_Expired(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create with past expiry
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &pastExpiry,
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	_, err = store.Extend(context.Background(), created.ShareID, 7*24*time.Hour)
	if err != ErrShareExpired {
		t.Errorf("expected ErrShareExpired, got %v", err)
	}
}

func TestListByAccount_Success(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create two shares for the same account
	input1 := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}
	input2 := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix2/"},
	}

	_, err := store.Create(context.Background(), input1)
	if err != nil {
		t.Fatalf("unexpected error creating first: %v", err)
	}
	_, err = store.Create(context.Background(), input2)
	if err != nil {
		t.Fatalf("unexpected error creating second: %v", err)
	}

	records, err := store.ListByAccount(context.Background(), "534840902377")
	if err != nil {
		t.Fatalf("unexpected error listing: %v", err)
	}

	if len(records) != 2 {
		t.Errorf("expected 2 records, got %d", len(records))
	}
}

func TestListByAccount_NoResults(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	records, err := store.ListByAccount(context.Background(), "nonexistent-account")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
}

func TestListAll_Success(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create shares for different accounts
	input1 := CreateInput{
		ResearcherAccountID: "111111111111",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:111111111111:vpc/vpc-0000000000000001",
		LibraryIDs:          []string{"L001"},
		S3Prefixes:          []string{"s3://bucket/p1/"},
	}
	input2 := CreateInput{
		ResearcherAccountID: "222222222222",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:222222222222:vpc/vpc-0000000000000002",
		LibraryIDs:          []string{"L002"},
		S3Prefixes:          []string{"s3://bucket/p2/"},
	}

	_, err := store.Create(context.Background(), input1)
	if err != nil {
		t.Fatalf("unexpected error creating first: %v", err)
	}

	// Small delay to ensure different createdAt times
	time.Sleep(10 * time.Millisecond)

	_, err = store.Create(context.Background(), input2)
	if err != nil {
		t.Fatalf("unexpected error creating second: %v", err)
	}

	records, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(records) != 2 {
		t.Errorf("expected 2 records, got %d", len(records))
	}

	// Verify descending order by createdAt
	if len(records) == 2 && records[0].CreatedAt.Before(records[1].CreatedAt) {
		t.Error("expected records ordered by createdAt descending")
	}
}

func TestListAll_Empty(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	records, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
}

func TestMarshalUnmarshal_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second) // truncate for RFC3339 round-trip
	expiry := now.Add(7 * 24 * time.Hour)

	original := &model.ShareRecord{
		ShareID:             "01HYPE7XXXXXXXXXXXXXXXXXXX",
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
		CreatedAt:           now,
		ExpiresAt:           &expiry,
		RevokedAt:           nil,
	}

	item, err := marshalShareRecord(original)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	restored, err := unmarshalShareRecord(item)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if restored.ShareID != original.ShareID {
		t.Errorf("ShareID: expected %q, got %q", original.ShareID, restored.ShareID)
	}
	if restored.ResearcherAccountID != original.ResearcherAccountID {
		t.Errorf("ResearcherAccountID: expected %q, got %q", original.ResearcherAccountID, restored.ResearcherAccountID)
	}
	if restored.TargetVpcArn != original.TargetVpcArn {
		t.Errorf("TargetVpcArn: expected %q, got %q", original.TargetVpcArn, restored.TargetVpcArn)
	}
	if !restored.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt: expected %v, got %v", original.CreatedAt, restored.CreatedAt)
	}
	if restored.ExpiresAt == nil || !restored.ExpiresAt.Equal(*original.ExpiresAt) {
		t.Errorf("ExpiresAt: expected %v, got %v", original.ExpiresAt, restored.ExpiresAt)
	}
	if restored.RevokedAt != nil {
		t.Error("expected nil RevokedAt")
	}
}

func TestMarshalUnmarshal_RoundTrip_WithRevokedAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	revokedAt := now.Add(-30 * time.Minute)

	original := &model.ShareRecord{
		ShareID:             "01HYPE7XXXXXXXXXXXXXXXXXXX",
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		CreatedAt:           now.Add(-time.Hour),
		RevokedAt:           &revokedAt,
	}

	item, err := marshalShareRecord(original)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	restored, err := unmarshalShareRecord(item)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if restored.RevokedAt == nil {
		t.Fatal("expected non-nil RevokedAt")
	}
	if !restored.RevokedAt.Equal(*original.RevokedAt) {
		t.Errorf("RevokedAt: expected %v, got %v", *original.RevokedAt, *restored.RevokedAt)
	}
}

func TestRevoke_AlreadyRevoked_StateUnchanged(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create and revoke a share
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Simulate revoked state in DynamoDB
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[created.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}

	// Track whether UpdateItem is called
	updateCalled := false
	mock.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		updateCalled = true
		return &dynamodb.UpdateItemOutput{}, nil
	}

	// Attempt revoke on already-revoked share
	_, err = store.Revoke(context.Background(), created.ShareID)
	if err != ErrShareAlreadyRevoked {
		t.Errorf("expected ErrShareAlreadyRevoked, got %v", err)
	}

	if updateCalled {
		t.Error("expected UpdateItem NOT to be called on already-revoked share")
	}
}

func TestRevoke_Expired_StateUnchanged(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create a share with past expiry
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &pastExpiry,
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Track whether UpdateItem is called
	updateCalled := false
	mock.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		updateCalled = true
		return &dynamodb.UpdateItemOutput{}, nil
	}

	// Attempt revoke on expired share
	_, err = store.Revoke(context.Background(), created.ShareID)
	if err != ErrShareExpired {
		t.Errorf("expected ErrShareExpired, got %v", err)
	}

	if updateCalled {
		t.Error("expected UpdateItem NOT to be called on expired share")
	}
}

func TestExtend_Revoked_StateUnchanged(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create and simulate revoked state
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[created.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}

	// Track whether UpdateItem is called
	updateCalled := false
	mock.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		updateCalled = true
		return &dynamodb.UpdateItemOutput{}, nil
	}

	// Attempt extend on revoked share
	_, err = store.Extend(context.Background(), created.ShareID, 7*24*time.Hour)
	if err != ErrShareAlreadyRevoked {
		t.Errorf("expected ErrShareAlreadyRevoked, got %v", err)
	}

	if updateCalled {
		t.Error("expected UpdateItem NOT to be called on revoked share")
	}
}

func TestExtend_Expired_StateUnchanged(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create with past expiry
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &pastExpiry,
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Track whether UpdateItem is called
	updateCalled := false
	mock.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		updateCalled = true
		return &dynamodb.UpdateItemOutput{}, nil
	}

	// Attempt extend on expired share
	_, err = store.Extend(context.Background(), created.ShareID, 7*24*time.Hour)
	if err != ErrShareExpired {
		t.Errorf("expected ErrShareExpired, got %v", err)
	}

	if updateCalled {
		t.Error("expected UpdateItem NOT to be called on expired share")
	}
}

func TestGenerateULID(t *testing.T) {
	id1 := generateULID()
	id2 := generateULID()

	if id1 == "" {
		t.Error("expected non-empty ULID")
	}
	if len(id1) != 26 {
		t.Errorf("expected 26-char ULID, got %d chars: %s", len(id1), id1)
	}
	if id1 == id2 {
		t.Error("expected two distinct ULIDs")
	}
}

func TestFindMatchingActiveShare_ExactMatch(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create an active share
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Search with same library IDs in different order
	match, err := store.FindMatchingActiveShare(context.Background(), "534840902377", []string{"L2400002", "L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if match == nil {
		t.Fatal("expected a match, got nil")
	}
	if match.ShareID != created.ShareID {
		t.Errorf("expected shareID %q, got %q", created.ShareID, match.ShareID)
	}
}

func TestFindMatchingActiveShare_NoMatch_DifferentLibraries(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create an active share
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}

	_, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Search with partially overlapping (but not exact) library IDs
	match, err := store.FindMatchingActiveShare(context.Background(), "534840902377", []string{"L2400001", "L2400003"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if match != nil {
		t.Errorf("expected nil (no match), got share %q", match.ShareID)
	}
}

func TestFindMatchingActiveShare_SkipsRevoked(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create a share and then revoke it
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}

	created, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Mark as revoked in mock
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[created.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}

	// Search should not find the revoked share
	match, err := store.FindMatchingActiveShare(context.Background(), "534840902377", []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if match != nil {
		t.Errorf("expected nil (revoked share should be skipped), got share %q", match.ShareID)
	}
}

func TestFindMatchingActiveShare_SkipsExpired(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create a share with past expiry
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour)
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &pastExpiry,
	}

	_, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating: %v", err)
	}

	// Search should not find the expired share
	match, err := store.FindMatchingActiveShare(context.Background(), "534840902377", []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if match != nil {
		t.Errorf("expected nil (expired share should be skipped), got share %q", match.ShareID)
	}
}

func TestFindMatchingActiveShare_NoShares(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	match, err := store.FindMatchingActiveShare(context.Background(), "534840902377", []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if match != nil {
		t.Errorf("expected nil, got share %q", match.ShareID)
	}
}

func TestCreateIdempotent_NewShare(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}

	record, reused, err := store.CreateIdempotent(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reused {
		t.Error("expected reused=false for new share")
	}
	if record == nil {
		t.Fatal("expected non-nil record")
	}
	if record.ShareID == "" {
		t.Error("expected non-empty ShareID")
	}
}

func TestCreateIdempotent_ExistingMatch(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}

	// First creation
	first, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating first: %v", err)
	}

	// Idempotent creation with same libraries (different order)
	input2 := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400002", "L2400001"}, // different order
		S3Prefixes:          []string{"s3://bucket/prefix2/", "s3://bucket/prefix1/"},
	}

	record, reused, err := store.CreateIdempotent(context.Background(), input2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !reused {
		t.Error("expected reused=true for matching share")
	}
	if record.ShareID != first.ShareID {
		t.Errorf("expected same shareID %q, got %q", first.ShareID, record.ShareID)
	}
}

func TestCreateIdempotent_PartialOverlap_CreatesNew(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	// Create a share with L1, L2
	input := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}

	first, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error creating first: %v", err)
	}

	// Idempotent creation with partial overlap (L1, L3) — should create new share
	input2 := CreateInput{
		ResearcherAccountID: "534840902377",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400003"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix3/"},
	}

	record, reused, err := store.CreateIdempotent(context.Background(), input2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reused {
		t.Error("expected reused=false for partial overlap")
	}
	if record.ShareID == first.ShareID {
		t.Error("expected different shareID for new share")
	}
}

func TestLibraryIDSetsEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        []string
		b        []string
		expected bool
	}{
		{
			name:     "identical sets same order",
			a:        []string{"L1", "L2", "L3"},
			b:        []string{"L1", "L2", "L3"},
			expected: true,
		},
		{
			name:     "identical sets different order",
			a:        []string{"L3", "L1", "L2"},
			b:        []string{"L2", "L3", "L1"},
			expected: true,
		},
		{
			name:     "different lengths",
			a:        []string{"L1", "L2"},
			b:        []string{"L1", "L2", "L3"},
			expected: false,
		},
		{
			name:     "same length different elements",
			a:        []string{"L1", "L2"},
			b:        []string{"L1", "L3"},
			expected: false,
		},
		{
			name:     "both empty",
			a:        []string{},
			b:        []string{},
			expected: true,
		},
		{
			name:     "one empty one not",
			a:        []string{},
			b:        []string{"L1"},
			expected: false,
		},
		{
			name:     "single element match",
			a:        []string{"L1"},
			b:        []string{"L1"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := libraryIDSetsEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("libraryIDSetsEqual(%v, %v) = %v, want %v", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}
