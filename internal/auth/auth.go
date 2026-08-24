// Package auth implements the Authorization Service client for managing
// Share_Records (create, revoke, extend, list) and the authorization callout handler.
package auth

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/oklog/ulid/v2"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/model"
)

// ShareStore provides CRUD operations for ShareRecords backed by DynamoDB.
type ShareStore struct {
	client    internalaws.DynamoDBClient
	tableName string
}

// NewShareStore creates a new ShareStore with the given DynamoDB client and table name.
func NewShareStore(client internalaws.DynamoDBClient, tableName string) *ShareStore {
	return &ShareStore{
		client:    client,
		tableName: tableName,
	}
}

// GSI index names.
const (
	GSI2IndexName = "GSI2"
)

// ErrShareNotFound indicates the requested share ID does not exist.
var ErrShareNotFound = fmt.Errorf("share not found")

// ErrShareAlreadyRevoked indicates the share has already been revoked.
var ErrShareAlreadyRevoked = fmt.Errorf("share is already revoked")

// ErrShareExpired indicates the share has already expired.
var ErrShareExpired = fmt.Errorf("share has already expired")

// generateULID creates a new ULID using the current time and a random source.
func generateULID() string {
	t := time.Now().UTC()
	entropy := rand.New(rand.NewSource(t.UnixNano()))
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}

// CreateInput contains the parameters for creating a new ShareRecord.
type CreateInput struct {
	ResearcherAccountID string
	TargetVpcArn        string
	LibraryIDs          []string
	S3Prefixes          []string
	// Files records the individual resolved files for manifest generation. Optional;
	// it never affects authorization, which is granted by S3Prefixes.
	Files     []model.ShareFile
	ExpiresAt *time.Time
}

// Create persists a new ShareRecord with a generated ULID, computed expiry, s3Prefixes,
// and status=active. Returns the created ShareRecord.
//
// The target VPC ARN is validated here rather than only at the CLI, because this is the
// single write path every caller shares — the share command, the canary, and the admin
// Lambda's create endpoint. A malformed ARN produces no error downstream: it is stored
// happily and then never matches an authorization callout, so the failure surfaces much
// later as an unexplained deny.
func (s *ShareStore) Create(ctx context.Context, input CreateInput) (*model.ShareRecord, error) {
	if err := model.ValidateTargetVpcArn(input.TargetVpcArn, input.ResearcherAccountID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	shareID := generateULID()

	record := &model.ShareRecord{
		ShareID:             shareID,
		ResearcherAccountID: input.ResearcherAccountID,
		TargetVpcArn:        input.TargetVpcArn,
		LibraryIDs:          input.LibraryIDs,
		S3Prefixes:          input.S3Prefixes,
		Files:               input.Files,
		CreatedAt:           now,
		ExpiresAt:           input.ExpiresAt,
		RevokedAt:           nil,
	}

	item, err := marshalShareRecord(record)
	if err != nil {
		return nil, fmt.Errorf("marshaling share record: %w", err)
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      item,
	})
	if err != nil {
		return nil, fmt.Errorf("persisting share record: %w", err)
	}

	return record, nil
}

// Get retrieves a ShareRecord by its shareId.
func (s *ShareStore) Get(ctx context.Context, shareID string) (*model.ShareRecord, error) {
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"shareId": &types.AttributeValueMemberS{Value: shareID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting share record: %w", err)
	}
	if output.Item == nil {
		return nil, ErrShareNotFound
	}

	return unmarshalShareRecord(output.Item)
}

// Revoke sets the revokedAt timestamp on a ShareRecord after verifying it is active.
// Returns ErrShareNotFound if the share doesn't exist, ErrShareAlreadyRevoked if already
// revoked, or ErrShareExpired if the share has expired.
func (s *ShareStore) Revoke(ctx context.Context, shareID string) (*model.ShareRecord, error) {
	record, err := s.Get(ctx, shareID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	status := record.EffectiveStatus(now)
	switch status {
	case model.ShareStatusRevoked:
		return nil, ErrShareAlreadyRevoked
	case model.ShareStatusExpired:
		return nil, ErrShareExpired
	}

	revokedAt := now.Format(time.RFC3339)
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"shareId": &types.AttributeValueMemberS{Value: shareID},
		},
		UpdateExpression: aws.String("SET revokedAt = :revokedAt"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":revokedAt": &types.AttributeValueMemberS{Value: revokedAt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("revoking share record: %w", err)
	}

	record.RevokedAt = &now
	return record, nil
}

// Extend updates the expiresAt timestamp to now + duration after verifying the share is active.
// Returns ErrShareNotFound if the share doesn't exist, ErrShareAlreadyRevoked if revoked,
// or ErrShareExpired if the share has expired.
func (s *ShareStore) Extend(ctx context.Context, shareID string, duration time.Duration) (*model.ShareRecord, error) {
	record, err := s.Get(ctx, shareID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	status := record.EffectiveStatus(now)
	switch status {
	case model.ShareStatusRevoked:
		return nil, ErrShareAlreadyRevoked
	case model.ShareStatusExpired:
		return nil, ErrShareExpired
	}

	newExpiry := now.Add(duration)
	expiresAtStr := newExpiry.Format(time.RFC3339)
	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"shareId": &types.AttributeValueMemberS{Value: shareID},
		},
		UpdateExpression: aws.String("SET expiresAt = :expiresAt"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":expiresAt": &types.AttributeValueMemberS{Value: expiresAtStr},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("extending share record: %w", err)
	}

	record.ExpiresAt = &newExpiry
	return record, nil
}

// ListByAccount queries ShareRecords for a specific researcherAccountId using GSI2,
// ordered by createdAt descending.
func (s *ShareStore) ListByAccount(ctx context.Context, researcherAccountID string) ([]*model.ShareRecord, error) {
	output, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(GSI2IndexName),
		KeyConditionExpression: aws.String("researcherAccountId = :accountId"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":accountId": &types.AttributeValueMemberS{Value: researcherAccountID},
		},
		ScanIndexForward: aws.Bool(false), // descending order by createdAt (SK)
	})
	if err != nil {
		return nil, fmt.Errorf("querying shares by account: %w", err)
	}

	return unmarshalShareRecords(output.Items)
}

// ListAll scans all ShareRecords and returns them ordered by createdAt descending.
func (s *ShareStore) ListAll(ctx context.Context) ([]*model.ShareRecord, error) {
	output, err := s.client.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		return nil, fmt.Errorf("scanning shares: %w", err)
	}

	records, err := unmarshalShareRecords(output.Items)
	if err != nil {
		return nil, err
	}

	// Sort by createdAt descending, matching the order ListByAccount gets from the index.
	slices.SortFunc(records, func(a, b *model.ShareRecord) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})

	return records, nil
}

// FindMatchingActiveShare queries existing active shares for the given researcherAccountID
// and compares library ID sets (order-independent) for an exact match. If a match is found,
// it returns the existing ShareRecord. If no match exists, it returns nil.
// Requirements: 12.1, 12.2, 12.3, 12.4
func (s *ShareStore) FindMatchingActiveShare(ctx context.Context, researcherAccountID string, libraryIDs []string) (*model.ShareRecord, error) {
	shares, err := s.ListByAccount(ctx, researcherAccountID)
	if err != nil {
		return nil, fmt.Errorf("querying existing shares for idempotency check: %w", err)
	}

	now := time.Now().UTC()
	for _, share := range shares {
		if share.EffectiveStatus(now) != model.ShareStatusActive {
			continue
		}
		if libraryIDSetsEqual(share.LibraryIDs, libraryIDs) {
			return share, nil
		}
	}

	return nil, nil
}

// CreateIdempotent creates a new ShareRecord, but first checks if an identical active share
// already exists for the same researcher account with the same set of library IDs. If a
// match is found, it returns the existing share without creating a duplicate.
// The returned bool indicates whether an existing share was reused (true) or a new one was
// created (false).
// Requirements: 12.1, 12.2, 12.3, 12.4
func (s *ShareStore) CreateIdempotent(ctx context.Context, input CreateInput) (*model.ShareRecord, bool, error) {
	existing, err := s.FindMatchingActiveShare(ctx, input.ResearcherAccountID, input.LibraryIDs)
	if err != nil {
		return nil, false, err
	}

	if existing != nil {
		return existing, true, nil
	}

	record, err := s.Create(ctx, input)
	if err != nil {
		return nil, false, err
	}

	return record, false, nil
}

// libraryIDSetsEqual compares two slices of library IDs as sets (order-independent).
// Returns true if both contain exactly the same elements. Both sides are sorted copies:
// the stored side arrives from a DynamoDB string set, whose order is unspecified.
func libraryIDSetsEqual(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// dynamoShareRecord is the DynamoDB-specific representation of a ShareRecord.
// It uses string-based timestamps for DynamoDB storage.
type dynamoShareRecord struct {
	ShareID             string   `dynamodbav:"shareId"`
	ResearcherAccountID string   `dynamodbav:"researcherAccountId"`
	TargetVpcArn        string   `dynamodbav:"targetVpcArn"`
	LibraryIDs          []string `dynamodbav:"libraryIds,stringset"`
	S3Prefixes          []string `dynamodbav:"s3Prefixes,stringset"`
	// Files is a list rather than a set: entries are structs, and their order is the
	// resolution order, which the manifest reproduces.
	Files     []dynamoShareFile `dynamodbav:"files,omitempty"`
	CreatedAt string            `dynamodbav:"createdAt"`
	ExpiresAt *string           `dynamodbav:"expiresAt,omitempty"`
	RevokedAt *string           `dynamodbav:"revokedAt,omitempty"`
}

// dynamoShareFile is the DynamoDB representation of a model.ShareFile.
type dynamoShareFile struct {
	LibraryID string `dynamodbav:"libraryId"`
	HtsgetID  string `dynamodbav:"htsgetId"`
	Format    string `dynamodbav:"format"`
}

// marshalShareRecord converts a model.ShareRecord to a DynamoDB attribute map.
func marshalShareRecord(record *model.ShareRecord) (map[string]types.AttributeValue, error) {
	ddbRecord := dynamoShareRecord{
		ShareID:             record.ShareID,
		ResearcherAccountID: record.ResearcherAccountID,
		TargetVpcArn:        record.TargetVpcArn,
		LibraryIDs:          record.LibraryIDs,
		S3Prefixes:          record.S3Prefixes,
		Files:               toDynamoShareFiles(record.Files),
		CreatedAt:           record.CreatedAt.Format(time.RFC3339),
	}

	if record.ExpiresAt != nil {
		s := record.ExpiresAt.Format(time.RFC3339)
		ddbRecord.ExpiresAt = &s
	}
	if record.RevokedAt != nil {
		s := record.RevokedAt.Format(time.RFC3339)
		ddbRecord.RevokedAt = &s
	}

	return attributevalue.MarshalMap(ddbRecord)
}

// unmarshalShareRecord converts a DynamoDB attribute map to a model.ShareRecord.
func unmarshalShareRecord(item map[string]types.AttributeValue) (*model.ShareRecord, error) {
	var ddbRecord dynamoShareRecord
	if err := attributevalue.UnmarshalMap(item, &ddbRecord); err != nil {
		return nil, fmt.Errorf("unmarshaling share record: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339, ddbRecord.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing createdAt: %w", err)
	}

	record := &model.ShareRecord{
		ShareID:             ddbRecord.ShareID,
		ResearcherAccountID: ddbRecord.ResearcherAccountID,
		TargetVpcArn:        ddbRecord.TargetVpcArn,
		LibraryIDs:          ddbRecord.LibraryIDs,
		S3Prefixes:          ddbRecord.S3Prefixes,
		Files:               fromDynamoShareFiles(ddbRecord.Files),
		CreatedAt:           createdAt,
	}

	if ddbRecord.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *ddbRecord.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("parsing expiresAt: %w", err)
		}
		record.ExpiresAt = &t
	}

	if ddbRecord.RevokedAt != nil {
		t, err := time.Parse(time.RFC3339, *ddbRecord.RevokedAt)
		if err != nil {
			return nil, fmt.Errorf("parsing revokedAt: %w", err)
		}
		record.RevokedAt = &t
	}

	return record, nil
}

// mapFiles converts between the model and DynamoDB share-file representations.
// Nil or empty in, nil out — so a record with no files omits the attribute entirely
// (which is also how records written before the attribute existed read back).
func mapFiles[T, U any](in []T, convert func(T) U) []U {
	if len(in) == 0 {
		return nil
	}
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, convert(v))
	}
	return out
}

func toDynamoShareFiles(files []model.ShareFile) []dynamoShareFile {
	return mapFiles(files, func(f model.ShareFile) dynamoShareFile {
		return dynamoShareFile{LibraryID: f.LibraryID, HtsgetID: f.HtsgetID, Format: f.Format}
	})
}

func fromDynamoShareFiles(files []dynamoShareFile) []model.ShareFile {
	return mapFiles(files, func(f dynamoShareFile) model.ShareFile {
		return model.ShareFile{LibraryID: f.LibraryID, HtsgetID: f.HtsgetID, Format: f.Format}
	})
}

// unmarshalShareRecords converts a slice of DynamoDB items to ShareRecords.
func unmarshalShareRecords(items []map[string]types.AttributeValue) ([]*model.ShareRecord, error) {
	records := make([]*model.ShareRecord, 0, len(items))
	for _, item := range items {
		record, err := unmarshalShareRecord(item)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}
