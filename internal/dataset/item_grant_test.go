package dataset

// These tests cover the ledger representation of a grant: the indexes it belongs to, which
// are not the ones a copy belongs to, and the two timestamps that carry meaning by being
// absent.

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/model"
)

func TestMarshalGrantUsesTheSharedTablesAttributes(t *testing.T) {
	createdAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	g := &Grant{
		GrantID:             grantIdentifier(1),
		CopyID:              copyIdentifier(1),
		DatasetURI:          recordDataset,
		HotCacheBucket:      recordCacheBucket,
		KeyArn:              recordKeyArn,
		ResearcherAccountID: recordResearcher,
		TargetVpcID:         recordTargetVpc,
		State:               GrantStateGranted,
		CreatedAt:           createdAt,
	}

	item, err := marshalGrant(g)
	if err != nil {
		t.Fatalf("marshaling a grant: %v", err)
	}

	if got := attributeString(t, item, recordIDAttribute); got != g.GrantID {
		t.Errorf("%s: expected %q, got %q", recordIDAttribute, g.GrantID, got)
	}
	if got := attributeString(t, item, model.MechanismAttribute); got != string(model.MechanismS3) {
		t.Errorf("mechanism: expected %q, got %q", model.MechanismS3, got)
	}
	if got := attributeString(t, item, kindAttribute); got != string(kindGrant) {
		t.Errorf("kind: expected %q, got %q", kindGrant, got)
	}

	// The copy is not indexed, so it is carried plainly and read back by whoever needs to
	// know which transfer the bytes came from.
	if got := attributeString(t, item, copyIDAttribute); got != g.CopyID {
		t.Errorf("%s: expected %q, got %q", copyIDAttribute, g.CopyID, got)
	}

	// Both keys of the per-researcher index, so one query per account answers for both
	// mechanisms, and both keys of the dataset index, which a grant shares with the copy
	// it reads from.
	if got := attributeString(t, item, researcherAccountAttribute); got != recordResearcher {
		t.Errorf("%s: expected %q, got %q", researcherAccountAttribute, recordResearcher, got)
	}
	if got := attributeString(t, item, datasetURIAttribute); got != recordDataset {
		t.Errorf("%s: expected %q, got %q", datasetURIAttribute, recordDataset, got)
	}
	if got := attributeString(t, item, createdAtAttribute); got != "2026-03-04T05:06:07Z" {
		t.Errorf("%s: expected an RFC3339 UTC timestamp, got %q", createdAtAttribute, got)
	}

	// The VPC index is the other mechanism's authorization path, and nothing of ours has
	// any business appearing in it.
	if _, present := item["targetVpcArn"]; present {
		t.Error("a grant must not populate the VPC index the authorization callout queries")
	}
}

// An unlimited grant carries no expiry at all rather than a far-future one, and a grant in
// force has not been revoked. Both read back from the absence of the attribute, so neither
// may be written empty.
func TestMarshalGrantOmitsTheTimestampsThatMeanSomethingByBeingAbsent(t *testing.T) {
	item, err := marshalGrant(&Grant{
		GrantID:             grantIdentifier(1),
		CopyID:              copyIdentifier(1),
		DatasetURI:          recordDataset,
		HotCacheBucket:      recordCacheBucket,
		KeyArn:              recordKeyArn,
		ResearcherAccountID: recordResearcher,
		TargetVpcID:         recordTargetVpc,
		State:               GrantStateGranted,
		CreatedAt:           recordClock,
	})
	if err != nil {
		t.Fatalf("marshaling a grant: %v", err)
	}

	for _, attribute := range []string{expiresAtAttribute, revokedAtAttribute} {
		if _, present := item[attribute]; present {
			t.Errorf("expected %q to be omitted from an unlimited grant still in force", attribute)
		}
	}
}

func TestGrantItemRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	expiresAt := createdAt.Add(30 * 24 * time.Hour)
	revokedAt := createdAt.Add(48 * time.Hour)

	tests := []struct {
		name  string
		grant Grant
	}{
		{
			// The shape most easily lost: an absent expiry must not read back as a date,
			// or an unlimited grant would silently stop working.
			name: "an unlimited grant",
			grant: Grant{
				GrantID:             grantIdentifier(1),
				CopyID:              copyIdentifier(1),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				ResearcherAccountID: recordResearcher,
				TargetVpcID:         recordTargetVpc,
				State:               GrantStateGranted,
				CreatedAt:           createdAt,
			},
		},
		{
			name: "an expiring grant",
			grant: Grant{
				GrantID:             grantIdentifier(2),
				CopyID:              copyIdentifier(1),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				ResearcherAccountID: recordResearcher,
				TargetVpcID:         recordTargetVpc,
				State:               GrantStateGranted,
				CreatedAt:           createdAt,
				ExpiresAt:           &expiresAt,
			},
		},
		{
			// A withdrawn grant keeps the expiry it had: both are part of the account of
			// what the researcher held and when.
			name: "a revoked grant keeps the expiry it had",
			grant: Grant{
				GrantID:             grantIdentifier(3),
				CopyID:              copyIdentifier(1),
				DatasetURI:          recordDataset,
				HotCacheBucket:      recordCacheBucket,
				KeyArn:              recordKeyArn,
				ResearcherAccountID: recordResearcher,
				TargetVpcID:         recordTargetVpc,
				State:               GrantStateRevoked,
				CreatedAt:           createdAt,
				ExpiresAt:           &expiresAt,
				RevokedAt:           &revokedAt,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, err := marshalGrant(&tt.grant)
			if err != nil {
				t.Fatalf("marshaling: %v", err)
			}
			restored, err := unmarshalGrant(item)
			if err != nil {
				t.Fatalf("unmarshaling: %v", err)
			}
			if !reflect.DeepEqual(*restored, tt.grant) {
				t.Errorf("round trip changed the grant:\n got %+v\nwant %+v", *restored, tt.grant)
			}
		})
	}
}

func TestMarshalGrantRefusals(t *testing.T) {
	tests := []struct {
		name  string
		grant Grant
	}{
		{
			name:  "no identifier",
			grant: Grant{CopyID: copyIdentifier(1), State: GrantStateGranted},
		},
		{
			// Storing the derived state would discard the recorded expiry, which is the
			// whole of what makes an expired grant extendable rather than dead.
			name:  "the derived state",
			grant: Grant{GrantID: grantIdentifier(1), State: GrantStateExpired},
		},
		{
			name:  "unknown state",
			grant: Grant{GrantID: grantIdentifier(1), State: GrantState("archived")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := marshalGrant(&tt.grant); err == nil {
				t.Fatal("expected the grant to be refused")
			}
		})
	}
}

func TestUnmarshalGrantRejectsEverythingThatIsNotAGrant(t *testing.T) {
	copyItem, err := marshalCopy(copyIn(CopyStateStaged))
	if err != nil {
		t.Fatalf("marshaling a copy: %v", err)
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
			name: "a copy of this mechanism",
			item: copyItem,
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
			if _, err := unmarshalGrant(tt.item); !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
		})
	}
}

// A copy and a grant of the same dataset are as alike as two records in this table ever
// get: the same partition key attribute, the same dataset, the same bucket and key, and
// adjacent positions in the same index. Only the kind tells them apart, and each has to
// refuse the other by name so that a mistaken identifier says which record it found.
func TestACopyAndAGrantOfOneDatasetAreNotConfusedForOneAnother(t *testing.T) {
	c := copyIn(CopyStateStaged)
	g := grantIn(GrantStateGranted)
	if g.DatasetURI != c.DatasetURI {
		t.Fatalf("this test is only meaningful when both describe one dataset, got %q and %q",
			c.DatasetURI, g.DatasetURI)
	}

	copyItem, err := marshalCopy(c)
	if err != nil {
		t.Fatalf("marshaling the copy: %v", err)
	}
	grantItem, err := marshalGrant(g)
	if err != nil {
		t.Fatalf("marshaling the grant: %v", err)
	}

	if attributeString(t, copyItem, datasetURIAttribute) != attributeString(t, grantItem, datasetURIAttribute) {
		t.Fatal("both records have to share the dataset index key for this to be worth testing")
	}

	restoredCopy, err := unmarshalCopy(copyItem)
	if err != nil {
		t.Fatalf("reading the copy back: %v", err)
	}
	if restoredCopy.CopyID != c.CopyID {
		t.Errorf("expected copy %q, got %q", c.CopyID, restoredCopy.CopyID)
	}
	restoredGrant, err := unmarshalGrant(grantItem)
	if err != nil {
		t.Fatalf("reading the grant back: %v", err)
	}
	if restoredGrant.GrantID != g.GrantID {
		t.Errorf("expected grant %q, got %q", g.GrantID, restoredGrant.GrantID)
	}

	// The refusal names what it found, so an operator who typed one identifier where the
	// other was wanted is told which command to reach for.
	_, err = unmarshalCopy(grantItem)
	if !errors.Is(err, errOtherKind) {
		t.Fatalf("expected a grant read as a copy to be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), string(kindGrant)) {
		t.Errorf("the refusal should name what it found, got: %v", err)
	}
	_, err = unmarshalGrant(copyItem)
	if !errors.Is(err, errOtherKind) {
		t.Fatalf("expected a copy read as a grant to be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), string(kindCopy)) {
		t.Errorf("the refusal should name what it found, got: %v", err)
	}
}
