package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/model"
)

// datasetItem builds the item shape the other release mechanism writes into the shared
// table: the same partition key and per-researcher index keys, the mechanism attribute,
// and none of the fields an htsget share has.
func datasetItem(releaseID, accountID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"shareId":             &types.AttributeValueMemberS{Value: releaseID},
		"mechanism":           &types.AttributeValueMemberS{Value: string(model.MechanismS3)},
		"researcherAccountId": &types.AttributeValueMemberS{Value: accountID},
		"createdAt":           &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		"datasetUri":          &types.AttributeValueMemberS{Value: "s3://source/dataset/"},
		"state":               &types.AttributeValueMemberS{Value: "released"},
	}
}

func storeShare(t *testing.T, store *ShareStore, accountID string) *model.ShareRecord {
	t.Helper()
	record, err := store.Create(context.Background(), CreateInput{
		ResearcherAccountID: accountID,
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:" + accountID + ":vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/demo/L2400001/"},
	})
	if err != nil {
		t.Fatalf("creating share: %v", err)
	}
	return record
}

func TestCreateMarksTheMechanism(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	record := storeShare(t, store, "777788889999")

	attr, ok := mock.Items[record.ShareID][model.MechanismAttribute].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatalf("expected a %s attribute on the stored item", model.MechanismAttribute)
	}
	if attr.Value != string(model.MechanismHtsget) {
		t.Errorf("expected mechanism %q, got %q", model.MechanismHtsget, attr.Value)
	}
}

// An item written before the table was shared carries no mechanism attribute. It is
// still a live share and has to keep reading as one, because nothing rewrites it.
func TestRecordWithoutMechanismReadsAsHtsget(t *testing.T) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")

	record := storeShare(t, store, "777788889999")
	delete(mock.Items[record.ShareID], model.MechanismAttribute)

	got, err := store.Get(context.Background(), record.ShareID)
	if err != nil {
		t.Fatalf("getting a record with no mechanism attribute: %v", err)
	}
	if got.ShareID != record.ShareID {
		t.Errorf("expected share %q, got %q", record.ShareID, got.ShareID)
	}
	if len(got.LibraryIDs) != 1 {
		t.Errorf("expected the library IDs to survive, got %v", got.LibraryIDs)
	}

	listed, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatalf("listing with a record that has no mechanism attribute: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 share listed, got %d", len(listed))
	}
}

// Both reads of the shared table ask the table for htsget records only, so the other
// mechanism's items are not paid for and not returned.
func TestReadsAreScopedToHtsgetRecords(t *testing.T) {
	assertScoped := func(t *testing.T, filter *string, names map[string]string, values map[string]types.AttributeValue) {
		t.Helper()
		if filter == nil {
			t.Fatal("expected a filter expression scoping the read to htsget records")
		}
		// Absence has to pass the filter: pre-existing items carry no attribute.
		if !strings.Contains(*filter, "attribute_not_exists") {
			t.Errorf("filter should admit items with no mechanism attribute, got %q", *filter)
		}
		alias, ok := names["#mechanism"]
		if !ok || alias != model.MechanismAttribute {
			t.Errorf("expected #mechanism to alias %q, got %v", model.MechanismAttribute, names)
		}
		want, ok := values[":htsgetMechanism"].(*types.AttributeValueMemberS)
		if !ok || want.Value != string(model.MechanismHtsget) {
			t.Errorf("expected the filter to compare against %q, got %v", model.MechanismHtsget, values)
		}
	}

	t.Run("list all", func(t *testing.T) {
		mock := awstest.NewDynamoDB()
		var seen *dynamodb.ScanInput
		mock.ScanFn = func(_ context.Context, params *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
			seen = params
			return &dynamodb.ScanOutput{}, nil
		}
		if _, err := NewShareStore(mock, "ShareRecords").ListAll(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertScoped(t, seen.FilterExpression, seen.ExpressionAttributeNames, seen.ExpressionAttributeValues)
	})

	t.Run("list by account", func(t *testing.T) {
		mock := awstest.NewDynamoDB()
		var seen *dynamodb.QueryInput
		mock.QueryFn = func(_ context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			seen = params
			return &dynamodb.QueryOutput{}, nil
		}
		if _, err := NewShareStore(mock, "ShareRecords").ListByAccount(context.Background(), "777788889999"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertScoped(t, seen.FilterExpression, seen.ExpressionAttributeNames, seen.ExpressionAttributeValues)
		if seen.ExpressionAttributeValues[":accountId"] == nil {
			t.Error("the account key condition value was dropped by the filter merge")
		}
	})
}

// The filter is the table's job, but a store or emulator that ignores it must not be able
// to feed a release to the htsget unmarshaller: it would not fail, it would report a
// share granting nothing to nobody.
func TestDatasetItemsNeverReachHtsgetRecords(t *testing.T) {
	const account = "777788889999"

	newStore := func(t *testing.T) (*awstest.DynamoDB, *ShareStore, string) {
		t.Helper()
		mock := awstest.NewDynamoDB()
		store := NewShareStore(mock, "ShareRecords")
		share := storeShare(t, store, account)
		mock.Items["01JRELEASEIDENTIFIER000000"] = datasetItem("01JRELEASEIDENTIFIER000000", account)
		return mock, store, share.ShareID
	}

	t.Run("list all", func(t *testing.T) {
		_, store, shareID := newStore(t)
		records, err := store.ListAll(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(records) != 1 || records[0].ShareID != shareID {
			t.Fatalf("expected only share %q, got %+v", shareID, records)
		}
	})

	t.Run("list by account", func(t *testing.T) {
		_, store, shareID := newStore(t)
		records, err := store.ListByAccount(context.Background(), account)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(records) != 1 || records[0].ShareID != shareID {
			t.Fatalf("expected only share %q, got %+v", shareID, records)
		}
	})

	// A release identifier is not a share identifier, so acting on one here is a
	// mistake, and reporting it as absent is what the operator can do something with.
	t.Run("get", func(t *testing.T) {
		_, store, _ := newStore(t)
		_, err := store.Get(context.Background(), "01JRELEASEIDENTIFIER000000")
		if !errors.Is(err, ErrShareNotFound) {
			t.Fatalf("expected ErrShareNotFound, got %v", err)
		}
	})

	// The idempotency check compares library ID sets, and a release has none. Matching
	// one would hand an operator someone else's record as an existing share.
	t.Run("idempotency check", func(t *testing.T) {
		_, store, _ := newStore(t)
		match, err := store.FindMatchingActiveShare(context.Background(), account, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if match != nil {
			t.Fatalf("expected no match, got share %q", match.ShareID)
		}
	})
}
