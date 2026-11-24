package dataset

// These tests cover the ledger representation of a copy, and above all what keeps it apart
// from everything else in a table it shares three ways: with the other mechanism, with the
// grants written against it, and with the undivided records written before copies and
// grants were separate things.

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/model"
)

// The fixtures every test of the two new record kinds is built from. They describe one
// dataset copied into one bucket and granted to one researcher, which is the shape almost
// every case starts from.
const (
	recordTableName     = "ShareRecords"
	recordDataset       = "s3://ceph-source/datasets/cohort-a/"
	recordCacheBucket   = "hot-cache-cohort-a"
	recordKeyArn        = "arn:aws:kms:ap-southeast-2:999988887777:key/1a2b3c4d-5e6f-7081-92a3-b4c5d6e7f809"
	recordResearcher    = "123456789012"
	recordTargetVpc     = "vpc-0a1b2c3d4e5f60718"
	recordCopyExecution = "arn:aws:states:ap-southeast-2:999988887777:execution:copy:cohort-a"
	recordCopySummary   = "s3://hot-cache-cohort-a/.copy-control/summary.csv"
)

// recordClock is the instant every write in these tests is timestamped with, so a written
// record can be compared against an expected one rather than against whatever the clock
// happened to say.
var recordClock = time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)

// copyIdentifier and grantIdentifier render stand-in identifiers of the same shape and
// length as a real one, distinguishable at a glance in a failure message.
func copyIdentifier(n int) string {
	return paddedIdentifier("01JCOPY", n)
}

func grantIdentifier(n int) string {
	return paddedIdentifier("01JGRANT", n)
}

// paddedIdentifier renders an identifier of the length a ULID has, so nothing under test
// can pass only because the identifiers in a test were shorter than the real ones.
func paddedIdentifier(prefix string, n int) string {
	digits := make([]byte, 26-len(prefix))
	for i := len(digits) - 1; i >= 0; i, n = i-1, n/10 {
		digits[i] = byte('0' + n%10)
	}
	return prefix + string(digits)
}

func attributeString(t *testing.T, item map[string]types.AttributeValue, name string) string {
	t.Helper()

	attr, ok := item[name].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatalf("expected a string attribute %q, got %#v", name, item[name])
	}
	return attr.Value
}

// otherMechanismItem builds the item shape the htsget mechanism writes: the same partition
// key and per-researcher index keys, its own mechanism marker, and none of the fields
// either of this mechanism's records has.
func otherMechanismItem(shareID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		recordIDAttribute:          &types.AttributeValueMemberS{Value: shareID},
		model.MechanismAttribute:   &types.AttributeValueMemberS{Value: string(model.MechanismHtsget)},
		researcherAccountAttribute: &types.AttributeValueMemberS{Value: recordResearcher},
		createdAtAttribute:         &types.AttributeValueMemberS{Value: "2026-07-10T03:00:00Z"},
		"targetVpcArn":             &types.AttributeValueMemberS{Value: "arn:aws:ec2:ap-southeast-2:123456789012:vpc/vpc-123"},
		"libraryIds":               &types.AttributeValueMemberSS{Value: []string{"L2400001"}},
	}
}

// undividedItem builds the shape of a record written before a copy and a grant were
// separate: this mechanism's marker, but no kind at all. Neither store may act on one,
// since it describes a transfer and a grant at once and no field says which half is which.
func undividedItem(shareID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		recordIDAttribute:          &types.AttributeValueMemberS{Value: shareID},
		model.MechanismAttribute:   &types.AttributeValueMemberS{Value: string(model.MechanismS3)},
		datasetURIAttribute:        &types.AttributeValueMemberS{Value: recordDataset},
		researcherAccountAttribute: &types.AttributeValueMemberS{Value: recordResearcher},
		createdAtAttribute:         &types.AttributeValueMemberS{Value: "2026-07-10T03:00:00Z"},
		stateAttribute:             &types.AttributeValueMemberS{Value: "released"},
	}
}

func TestMarshalCopyUsesTheSharedTablesAttributes(t *testing.T) {
	c := &Copy{
		CopyID:         copyIdentifier(1),
		DatasetURI:     recordDataset,
		HotCacheBucket: recordCacheBucket,
		KeyArn:         recordKeyArn,
		State:          CopyStateStaged,
		CreatedAt:      time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}

	item, err := marshalCopy(c)
	if err != nil {
		t.Fatalf("marshaling a copy: %v", err)
	}

	// The partition key is the one the table already has, carrying the copy id.
	if got := attributeString(t, item, recordIDAttribute); got != c.CopyID {
		t.Errorf("%s: expected %q, got %q", recordIDAttribute, c.CopyID, got)
	}
	if got := attributeString(t, item, model.MechanismAttribute); got != string(model.MechanismS3) {
		t.Errorf("mechanism: expected %q, got %q", model.MechanismS3, got)
	}
	// Without this a grant of the same dataset, sitting in the same index under the same
	// key, would read back as a transfer it never performed.
	if got := attributeString(t, item, kindAttribute); got != string(kindCopy) {
		t.Errorf("kind: expected %q, got %q", kindCopy, got)
	}

	// Both keys of the index the copies of one dataset are found through.
	if got := attributeString(t, item, datasetURIAttribute); got != recordDataset {
		t.Errorf("%s: expected %q, got %q", datasetURIAttribute, recordDataset, got)
	}
	if got := attributeString(t, item, createdAtAttribute); got != "2026-03-04T05:06:07Z" {
		t.Errorf("%s: expected an RFC3339 UTC timestamp, got %q", createdAtAttribute, got)
	}

	// A copy has no researcher, so it belongs in neither the per-researcher index nor the
	// VPC index the other mechanism's authorization callout queries.
	for _, attribute := range []string{researcherAccountAttribute, targetVpcAttribute, "targetVpcArn"} {
		if _, present := item[attribute]; present {
			t.Errorf("a copy names no researcher, so %q must be absent", attribute)
		}
	}
}

// Nothing the transfer has not reported yet may be written as a zero, because a stored
// zero reads as a transfer that delivered nothing rather than one that has not said.
func TestMarshalCopyOmitsWhatTheTransferHasNotReported(t *testing.T) {
	item, err := marshalCopy(&Copy{
		CopyID:         copyIdentifier(1),
		DatasetURI:     recordDataset,
		HotCacheBucket: recordCacheBucket,
		KeyArn:         recordKeyArn,
		State:          CopyStateCopying,
		CreatedAt:      recordClock,
	})
	if err != nil {
		t.Fatalf("marshaling a copy: %v", err)
	}

	for _, attribute := range []string{
		objectCountAttribute,
		totalBytesAttribute,
		copySummaryLocationAttribute,
		"copyExecutionArn",
		purgedAtAttribute,
		keyDeletionDateAttribute,
		deletedAtAttribute,
	} {
		if _, present := item[attribute]; present {
			t.Errorf("expected %q to be omitted from a copy still running", attribute)
		}
	}
}

func TestCopyItemRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	purgedAt := createdAt.Add(72 * time.Hour)
	deletionDate := createdAt.Add(30 * 24 * time.Hour)
	deletedAt := deletionDate.Add(time.Hour)

	tests := []struct {
		name string
		copy Copy
	}{
		{
			name: "a transfer under way, which has reported nothing",
			copy: Copy{
				CopyID:           copyIdentifier(1),
				DatasetURI:       recordDataset,
				HotCacheBucket:   recordCacheBucket,
				KeyArn:           recordKeyArn,
				State:            CopyStateCopying,
				CreatedAt:        createdAt,
				CopyExecutionArn: recordCopyExecution,
			},
		},
		{
			// The summary is written outside the dataset's own bucket and may be missing
			// when the copy could not write one; the absent attribute must not read back
			// as a location.
			name: "a staged copy with no summary of its own",
			copy: Copy{
				CopyID:         copyIdentifier(2),
				DatasetURI:     recordDataset,
				HotCacheBucket: recordCacheBucket,
				KeyArn:         recordKeyArn,
				State:          CopyStateStaged,
				CreatedAt:      createdAt,
				ObjectCount:    42,
				TotalBytes:     1 << 40,
			},
		},
		{
			name: "a failed transfer, carrying what it managed",
			copy: Copy{
				CopyID:              copyIdentifier(3),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				State:               CopyStateFailed,
				CreatedAt:           createdAt,
				CopyExecutionArn:    recordCopyExecution,
				ObjectCount:         3,
				TotalBytes:          4096,
				CopySummaryLocation: recordCopySummary,
			},
		},
		{
			name: "a purged copy keeps every timestamp it accumulated",
			copy: Copy{
				CopyID:              copyIdentifier(4),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				State:               CopyStatePurged,
				CreatedAt:           createdAt,
				CopyExecutionArn:    recordCopyExecution,
				ObjectCount:         12,
				TotalBytes:          1 << 30,
				CopySummaryLocation: recordCopySummary,
				PurgedAt:            &purgedAt,
				KeyDeletionDate:     &deletionDate,
			},
		},
		{
			// Deleting the objects adds a timestamp and takes nothing away: what the
			// transfer delivered and when its key was scheduled are still the record of
			// what happened, and the counts still describe objects that were there.
			name: "a copy whose objects have been deleted keeps the rest of its history",
			copy: Copy{
				CopyID:              copyIdentifier(5),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				State:               CopyStateDeleted,
				CreatedAt:           createdAt,
				CopyExecutionArn:    recordCopyExecution,
				ObjectCount:         12,
				TotalBytes:          1 << 30,
				CopySummaryLocation: recordCopySummary,
				PurgedAt:            &purgedAt,
				KeyDeletionDate:     &deletionDate,
				DeletedAt:           &deletedAt,
			},
		},
		{
			// The shape taking a purge back leaves behind: staged again, and carrying no
			// date its destruction becomes irreversible. Those cleared attributes have to
			// read back as absent rather than as a zero time, which would say the copy
			// was purged at the beginning of the epoch and is already unreadable.
			name: "a copy staged again after its purge was taken back",
			copy: Copy{
				CopyID:              copyIdentifier(7),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				State:               CopyStateStaged,
				CreatedAt:           createdAt,
				CopyExecutionArn:    recordCopyExecution,
				ObjectCount:         12,
				TotalBytes:          1 << 30,
				CopySummaryLocation: recordCopySummary,
			},
		},
		{
			// The other way in: a transfer that failed is deleted without ever having
			// been purged, so it carries a deletion and no key deletion date at all.
			name: "a failed copy deleted without ever having been purged",
			copy: Copy{
				CopyID:              copyIdentifier(6),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				State:               CopyStateDeleted,
				CreatedAt:           createdAt,
				CopyExecutionArn:    recordCopyExecution,
				ObjectCount:         3,
				TotalBytes:          4096,
				CopySummaryLocation: recordCopySummary,
				DeletedAt:           &deletedAt,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, err := marshalCopy(&tt.copy)
			if err != nil {
				t.Fatalf("marshaling: %v", err)
			}
			restored, err := unmarshalCopy(item)
			if err != nil {
				t.Fatalf("unmarshaling: %v", err)
			}
			if !reflect.DeepEqual(*restored, tt.copy) {
				t.Errorf("round trip changed the copy:\n got %+v\nwant %+v", *restored, tt.copy)
			}
		})
	}
}

func TestMarshalCopyRefusals(t *testing.T) {
	tests := []struct {
		name string
		copy Copy
	}{
		{
			// Would otherwise surface as a complaint about a missing attribute named for
			// the other mechanism.
			name: "no identifier",
			copy: Copy{DatasetURI: recordDataset, State: CopyStateCopying},
		},
		{
			name: "unknown state",
			copy: Copy{CopyID: copyIdentifier(1), State: CopyState("archived")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := marshalCopy(&tt.copy); err == nil {
				t.Fatal("expected the copy to be refused")
			}
		})
	}
}

// The shared table holds three other shapes of record, and every one of them read as a
// copy would report a transfer that never happened.
func TestUnmarshalCopyRejectsEverythingThatIsNotACopy(t *testing.T) {
	grantItem, err := marshalGrant(grantIn(GrantStateGranted))
	if err != nil {
		t.Fatalf("marshaling a grant: %v", err)
	}

	tests := []struct {
		name string
		item map[string]types.AttributeValue
		want error
	}{
		{
			name: "an htsget share",
			item: otherMechanismItem("01JSHARE00000000000000000A"),
			want: errOtherMechanism,
		},
		{
			// Written before the table was shared, so it carries no mechanism at all.
			name: "a record predating the shared table",
			item: withoutAttribute(otherMechanismItem("01JSHARE00000000000000000A"), model.MechanismAttribute),
			want: errOtherMechanism,
		},
		{
			name: "a grant of this mechanism",
			item: grantItem,
			want: errOtherKind,
		},
		{
			name: "a record written before copies and grants were separate",
			item: undividedItem("01JRELEASE0000000000000000"),
			want: errOtherKind,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := unmarshalCopy(tt.item); !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
		})
	}
}

func withoutAttribute(item map[string]types.AttributeValue, name string) map[string]types.AttributeValue {
	delete(item, name)
	return item
}
