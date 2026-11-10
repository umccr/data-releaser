package dataset

// These tests cover the ledger read and write path for copies, and above all the refusals.
//
// A copy that reports the wrong outcome is the most expensive mistake in this package: a
// transfer that stopped part way through leaves a bucket that looks exactly like one
// holding the whole dataset, and the only thing standing between that and a researcher is
// the state the copy recorded. So what matters is not only that the store says no to a
// forbidden move, but that saying no leaves the stored record exactly as it was. The store
// defends that twice -- it checks the state it read, then makes that state a condition of
// the write -- and both paths are exercised here.
//
// The table underneath is the shared ledger double, which honours those conditions and
// answers the index queries. Copies, grants and the other mechanism's records all live in
// it together, because that is how they live in the deployed table.

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/model"
)

// The identifiers the single seeded copy and grant most tests work with carry. Each test
// gets its own table, so reusing them costs nothing, and they are distinguishable from the
// minted ones so a failure says which record it was looking at.
const (
	seededCopyID  = "01JCOPYSEEDED0000000000000"
	seededGrantID = "01JGRANTSEEDED000000000000"
)

// newRecordLedger returns the two stores backed by one in-memory table that applies their
// update expressions and honours their conditions, with timestamps and identifiers pinned.
//
// Both stores share the table because that is how they are deployed: a copy and the grants
// against it sit next to each other under the same dataset index key, and several of the
// tests here are about precisely that.
func newRecordLedger(t *testing.T) (*awstest.DynamoDB, *CopyStore, *GrantStore) {
	t.Helper()

	table := awstest.NewLedger()

	copies := NewCopyStore(table, recordTableName)
	copies.now = func() time.Time { return recordClock }
	mintedCopies := 0
	copies.newID = func() string {
		mintedCopies++
		return copyIdentifier(mintedCopies)
	}

	grants := NewGrantStore(table, recordTableName)
	grants.now = func() time.Time { return recordClock }
	mintedGrants := 0
	grants.newID = func() string {
		mintedGrants++
		return grantIdentifier(mintedGrants)
	}

	return table, copies, grants
}

// copyIn builds a copy in the given state, carrying everything it would have accumulated by
// the time it got there. The accumulated fields are the point: a refused change has to be
// seen to have left all of them alone, not only the state.
func copyIn(state CopyState) *Copy {
	c := &Copy{
		CopyID:           seededCopyID,
		DatasetURI:       recordDataset,
		HotCacheBucket:   recordCacheBucket,
		KeyArn:           recordKeyArn,
		State:            state,
		CreatedAt:        recordClock.Add(-30 * 24 * time.Hour),
		CopyExecutionArn: recordCopyExecution,
	}

	switch state {
	case CopyStateCopying:
		// The transfer has not reported, so there are no numbers yet.
		return c
	case CopyStateFailed:
		// Whatever it managed before it failed is still recorded.
		c.ObjectCount, c.TotalBytes = 3, 4096
		c.CopySummaryLocation = recordCopySummary
		return c
	}

	c.ObjectCount, c.TotalBytes = 12, 1<<30
	c.CopySummaryLocation = recordCopySummary
	if state == CopyStatePurged || state == CopyStateDeleted {
		purgedAt := recordClock.Add(-time.Hour)
		deletionDate := purgedAt.Add(30 * 24 * time.Hour)
		c.PurgedAt, c.KeyDeletionDate = &purgedAt, &deletionDate
	}
	// A deleted copy is one that was purged and then had its objects removed, so it
	// carries both timestamps: the counts and the summary describe a transfer that
	// happened, and deleting what it delivered does not unsay that.
	if state == CopyStateDeleted {
		deletedAt := recordClock.Add(-time.Minute)
		c.DeletedAt = &deletedAt
	}
	return c
}

// datasetCopy builds a copy of the shared dataset with its own identifier and creation
// time, for the listings.
func datasetCopy(id string, state CopyState, createdAt time.Time) *Copy {
	c := copyIn(state)
	c.CopyID = id
	c.CreatedAt = createdAt
	return c
}

// seedCopy writes a copy straight into the table, so a test can start from any state
// without walking the lifecycle to get there, and can put a record into a state no legal
// change would produce.
func seedCopy(t *testing.T, table *awstest.DynamoDB, c *Copy) *Copy {
	t.Helper()

	item, err := marshalCopy(c)
	if err != nil {
		t.Fatalf("seeding copy %q: %v", c.CopyID, err)
	}
	table.Items[c.CopyID] = item
	return c
}

func storedRecordItem(t *testing.T, table *awstest.DynamoDB, id string) map[string]types.AttributeValue {
	t.Helper()

	item, ok := table.Items[id]
	if !ok {
		t.Fatalf("record %q is no longer in the table", id)
	}
	// Entries are replaced rather than mutated by the table, so a shallow copy is a stable
	// snapshot of what the record held before a change was attempted.
	return maps.Clone(item)
}

// assertRecordItemUnchanged fails if anything about the stored item moved. It compares the
// whole item rather than the state alone, because a change refused after it had already
// written a timestamp is still a change.
func assertRecordItemUnchanged(
	t *testing.T,
	table *awstest.DynamoDB,
	id string,
	before map[string]types.AttributeValue,
) {
	t.Helper()

	after := storedRecordItem(t, table, id)
	if len(before) != len(after) {
		t.Errorf("the refused change altered the stored record:\nbefore %v\n after %v", before, after)
		return
	}
	for name, want := range before {
		got, present := after[name]
		if !present {
			t.Errorf("the refused change removed %q from the stored record", name)
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("the refused change altered %q: was %s, now %s", name, attributeText(want), attributeText(got))
		}
	}
}

// attributeText renders an attribute for comparison and for a failure message. Every
// attribute either kind of record writes is a string or a number, and printing the values
// rather than the pointers is what makes a mismatch readable.
func attributeText(value types.AttributeValue) string {
	switch typed := value.(type) {
	case *types.AttributeValueMemberS:
		return "S:" + typed.Value
	case *types.AttributeValueMemberN:
		return "N:" + typed.Value
	case *types.AttributeValueMemberSS:
		return "SS:" + strings.Join(typed.Value, ",")
	default:
		return "?"
	}
}

// assertRefusedWithoutAWrite fails unless a refusal both left the stored record alone and
// was made before anything was attempted against the table.
func assertRefusedWithoutAWrite(
	t *testing.T,
	table *awstest.DynamoDB,
	id string,
	before map[string]types.AttributeValue,
	attempts *int,
) {
	t.Helper()

	if *attempts != 0 {
		t.Errorf("a refused change attempted %d write(s)", *attempts)
	}
	assertRecordItemUnchanged(t, table, id, before)
}

// countRecordWrites makes the table count the writes attempted against it, so a refusal can
// be shown to have been made before anything was attempted rather than after it was
// rejected.
func countRecordWrites(t *testing.T, table *awstest.DynamoDB) *int {
	t.Helper()

	attempts := 0
	write := table.UpdateItemFn
	table.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		attempts++
		return write(ctx, params)
	}
	return &attempts
}

func assertCopiesEqual(t *testing.T, want, got *Copy) {
	t.Helper()

	if got.CopyID != want.CopyID {
		t.Errorf("CopyID: expected %q, got %q", want.CopyID, got.CopyID)
	}
	if got.DatasetURI != want.DatasetURI {
		t.Errorf("DatasetURI: expected %q, got %q", want.DatasetURI, got.DatasetURI)
	}
	if got.HotCacheBucket != want.HotCacheBucket || got.KeyArn != want.KeyArn {
		t.Errorf("bucket and key: expected %q under %q, got %q under %q",
			want.HotCacheBucket, want.KeyArn, got.HotCacheBucket, got.KeyArn)
	}
	if got.State != want.State {
		t.Errorf("State: expected %q, got %q", want.State, got.State)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt: expected %s, got %s", want.CreatedAt, got.CreatedAt)
	}
	if got.CopyExecutionArn != want.CopyExecutionArn {
		t.Errorf("CopyExecutionArn: expected %q, got %q", want.CopyExecutionArn, got.CopyExecutionArn)
	}
	if got.ObjectCount != want.ObjectCount || got.TotalBytes != want.TotalBytes {
		t.Errorf("transferred: expected %d objects and %d bytes, got %d and %d",
			want.ObjectCount, want.TotalBytes, got.ObjectCount, got.TotalBytes)
	}
	if got.CopySummaryLocation != want.CopySummaryLocation {
		t.Errorf("CopySummaryLocation: expected %q, got %q", want.CopySummaryLocation, got.CopySummaryLocation)
	}
	assertSameInstant(t, "PurgedAt", want.PurgedAt, got.PurgedAt)
	assertSameInstant(t, "KeyDeletionDate", want.KeyDeletionDate, got.KeyDeletionDate)
}

// assertSameInstant compares two optional timestamps, treating absence as meaningful: an
// expiry that came back as a date, or a revocation that came back as nothing, is the kind
// of difference that changes what the record means.
func assertSameInstant(t *testing.T, field string, want, got *time.Time) {
	t.Helper()

	switch {
	case want == nil && got == nil:
	case want == nil:
		t.Errorf("%s: expected none, got %s", field, got)
	case got == nil:
		t.Errorf("%s: expected %s, got none", field, want)
	case !got.Equal(*want):
		t.Errorf("%s: expected %s, got %s", field, want, got)
	}
}

func copyIDs(copies []*Copy) []string {
	ids := make([]string, 0, len(copies))
	for _, c := range copies {
		ids = append(ids, c.CopyID)
	}
	return ids
}

func TestCreateRecordsACopyingCopy(t *testing.T) {
	_, copies, _ := newRecordLedger(t)

	c, err := copies.Create(context.Background(), CreateCopyInput{
		DatasetURI:       recordDataset,
		HotCacheBucket:   recordCacheBucket,
		KeyArn:           recordKeyArn,
		CopyExecutionArn: recordCopyExecution,
	})
	if err != nil {
		t.Fatalf("creating a copy: %v", err)
	}

	if c.State != CopyStateCopying {
		t.Errorf("expected a new copy to be copying, got %q", c.State)
	}
	if c.IsGrantable() {
		t.Error("a transfer that has not reported must not be grantable")
	}
	if !c.CreatedAt.Equal(recordClock) {
		t.Errorf("CreatedAt: expected %s, got %s", recordClock, c.CreatedAt)
	}
	if c.CopyID != copyIdentifier(1) {
		t.Errorf("expected the minted identifier %q, got %q", copyIdentifier(1), c.CopyID)
	}
	// What the transfer delivered is not known until it reports, so none of it may be
	// filled in yet.
	if c.ObjectCount != 0 || c.TotalBytes != 0 || c.CopySummaryLocation != "" {
		t.Errorf("a copy describes nothing transferred until it reports, got %+v", c)
	}

	stored, err := copies.Get(context.Background(), c.CopyID)
	if err != nil {
		t.Fatalf("reading back the copy just created: %v", err)
	}
	assertCopiesEqual(t, c, stored)
}

// A copy missing any of these would be recorded happily and then found by nothing: the
// dataset URI is the key its own dataset's copies are found through, and the bucket and key
// are what a grant against it renders its policies from.
func TestCreateRefusesAnIncompleteCopy(t *testing.T) {
	tests := []struct {
		name  string
		input CreateCopyInput
	}{
		{
			name:  "no dataset URI",
			input: CreateCopyInput{HotCacheBucket: recordCacheBucket, KeyArn: recordKeyArn},
		},
		{
			name:  "no hot cache bucket",
			input: CreateCopyInput{DatasetURI: recordDataset, KeyArn: recordKeyArn},
		},
		{
			name:  "no encryption key",
			input: CreateCopyInput{DatasetURI: recordDataset, HotCacheBucket: recordCacheBucket},
		},
		{
			name: "blank dataset URI",
			input: CreateCopyInput{
				DatasetURI:     "   ",
				HotCacheBucket: recordCacheBucket,
				KeyArn:         recordKeyArn,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, copies, _ := newRecordLedger(t)

			if _, err := copies.Create(context.Background(), tt.input); err == nil {
				t.Fatal("expected the copy to be refused")
			}
			if len(table.Items) != 0 {
				t.Errorf("a refused create wrote %d items", len(table.Items))
			}
		})
	}
}

// The identifier a transfer runs under is decided before there is anything to record --
// it names the execution and the folder its artefacts go in -- so the record has to be
// writable under it rather than under one the store mints.
func TestRecordKeepsTheIdentifierTheTransferAlreadyRunsUnder(t *testing.T) {
	_, copies, _ := newRecordLedger(t)

	submitted := &Copy{
		CopyID:           "01JSUBMITTED00000000000000",
		DatasetURI:       recordDataset,
		HotCacheBucket:   recordCacheBucket,
		KeyArn:           recordKeyArn,
		State:            CopyStateCopying,
		CreatedAt:        recordClock,
		CopyExecutionArn: recordCopyExecution,
	}
	if err := copies.Record(context.Background(), submitted); err != nil {
		t.Fatalf("recording a submitted copy: %v", err)
	}

	stored, err := copies.Get(context.Background(), submitted.CopyID)
	if err != nil {
		t.Fatalf("reading back the recorded copy: %v", err)
	}
	assertCopiesEqual(t, submitted, stored)
}

// The write is conditioned on the identifier being unused, so it can never overwrite an
// existing record of any kind.
func TestRecordRefusesAnIdentifierAlreadyInUse(t *testing.T) {
	table, copies, _ := newRecordLedger(t)
	table.PutItemFn = func(_ context.Context, _ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, &types.ConditionalCheckFailedException{}
	}

	_, err := copies.Create(context.Background(), CreateCopyInput{
		DatasetURI:     recordDataset,
		HotCacheBucket: recordCacheBucket,
		KeyArn:         recordKeyArn,
	})
	if err == nil {
		t.Fatal("expected the create to be refused")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("the refusal should say the identifier is taken, got: %v", err)
	}
}

func TestCopyGetReportsWhatItCannotActOn(t *testing.T) {
	ctx := context.Background()

	t.Run("no such copy", func(t *testing.T) {
		_, copies, _ := newRecordLedger(t)

		_, err := copies.Get(ctx, seededCopyID)
		if !errors.Is(err, ErrCopyNotFound) {
			t.Fatalf("expected ErrCopyNotFound, got %v", err)
		}
	})

	t.Run("a record of the other mechanism", func(t *testing.T) {
		table, copies, _ := newRecordLedger(t)
		const shareID = "01JSHARE00000000000000000A"
		table.Items[shareID] = otherMechanismItem(shareID)

		_, err := copies.Get(ctx, shareID)
		if !errors.Is(err, ErrCopyNotFound) {
			t.Fatalf("expected ErrCopyNotFound, got %v", err)
		}
		if !strings.Contains(err.Error(), string(model.MechanismHtsget)) {
			t.Errorf("the refusal should name the mechanism the record belongs to, got: %v", err)
		}
	})

	// A grant identifier where a copy was wanted. The two are told apart only by the kind,
	// so the refusal has to say which one it found or an operator has nothing to go on.
	t.Run("a grant identifier", func(t *testing.T) {
		table, copies, _ := newRecordLedger(t)
		grant := seedGrant(t, table, grantIn(GrantStateGranted))

		_, err := copies.Get(ctx, grant.GrantID)
		if !errors.Is(err, ErrCopyNotFound) {
			t.Fatalf("expected ErrCopyNotFound, got %v", err)
		}
		if !strings.Contains(err.Error(), string(kindGrant)) {
			t.Errorf("the refusal should say the identifier names a grant, got: %v", err)
		}
	})
}

// The heart of it: every change the lifecycle forbids is refused, reports the state the
// copy is actually in, and leaves the stored record exactly as it was.
func TestRefusedCopyTransitionsLeaveTheRecordUnchanged(t *testing.T) {
	ctx := context.Background()

	land := func(store *CopyStore, id string) (*Copy, error) {
		return store.MarkStaged(ctx, id, CopyOutcome{ObjectCount: 99, SummaryLocation: recordCopySummary})
	}
	fail := func(store *CopyStore, id string) (*Copy, error) {
		return store.MarkFailed(ctx, id, CopyOutcome{})
	}
	remove := func(store *CopyStore, id string) (*Copy, error) {
		return store.MarkDeleted(ctx, id)
	}
	restore := func(store *CopyStore, id string) (*Copy, error) {
		return store.MarkRestored(ctx, id)
	}

	tests := []struct {
		name      string
		from      CopyState
		requested CopyState
		change    func(store *CopyStore, copyID string) (*Copy, error)
	}{
		{
			// A transfer reports once. A second report of a different outcome would
			// rewrite what the first said actually arrived.
			name:      "landing a transfer that already reported",
			from:      CopyStateStaged,
			requested: CopyStateStaged,
			change:    land,
		},
		{
			// The dangerous one: a bucket holding part of a dataset looks exactly like one
			// holding all of it, so a failed transfer must never be talked into landing.
			name:      "landing a transfer that failed",
			from:      CopyStateFailed,
			requested: CopyStateStaged,
			change:    land,
		},
		{
			name:      "landing a transfer whose key is already scheduled for destruction",
			from:      CopyStatePurged,
			requested: CopyStateStaged,
			change:    land,
		},
		{
			name:      "failing a transfer that already delivered",
			from:      CopyStateStaged,
			requested: CopyStateFailed,
			change:    fail,
		},
		{
			name:      "failing a transfer that already failed",
			from:      CopyStateFailed,
			requested: CopyStateFailed,
			change:    fail,
		},
		{
			name:      "failing a purged copy",
			from:      CopyStatePurged,
			requested: CopyStateFailed,
			change:    fail,
		},
		{
			name:      "purging a copy that is already purged",
			from:      CopyStatePurged,
			requested: CopyStatePurged,
			change: func(store *CopyStore, id string) (*Copy, error) {
				return store.MarkPurged(ctx, id, recordClock.Add(30*24*time.Hour))
			},
		},
		{
			// The transfer is still writing objects into the bucket, so deleting what is
			// there deletes half a copy nobody asked to stop.
			name:      "deleting the objects of a transfer still under way",
			from:      CopyStateCopying,
			requested: CopyStateDeleted,
			change:    remove,
		},
		{
			// The dangerous one on this side: a staged copy is what a grant is made
			// against, so its objects are the ones a researcher may be reading right now.
			// Stopping that is a purge, and this refusal is what forces the order.
			name:      "deleting the objects of a copy that could still be read",
			from:      CopyStateStaged,
			requested: CopyStateDeleted,
			change:    remove,
		},
		{
			name:      "deleting objects that are already gone",
			from:      CopyStateDeleted,
			requested: CopyStateDeleted,
			change:    remove,
		},
		{
			name:      "landing a transfer whose objects have been deleted",
			from:      CopyStateDeleted,
			requested: CopyStateStaged,
			change:    land,
		},
		{
			name:      "purging a copy whose objects have been deleted",
			from:      CopyStateDeleted,
			requested: CopyStatePurged,
			change: func(store *CopyStore, id string) (*Copy, error) {
				return store.MarkPurged(ctx, id, recordClock.Add(30*24*time.Hour))
			},
		},
		{
			// Taking a purge back is the one move into staged that is not a transfer
			// reporting, and the lifecycle would let a transfer still running arrive
			// there. Refusing it here is what keeps this from becoming a way to say a
			// copy delivered without any copy having delivered.
			name:      "taking back the purge of a transfer still under way",
			from:      CopyStateCopying,
			requested: CopyStateStaged,
			change:    restore,
		},
		{
			// The dangerous one, again: the bucket holds part of a dataset, and nothing
			// was ever purged of it to take back.
			name:      "taking back the purge of a transfer that failed",
			from:      CopyStateFailed,
			requested: CopyStateStaged,
			change:    restore,
		},
		{
			name:      "taking back a purge of a copy that was never purged",
			from:      CopyStateStaged,
			requested: CopyStateStaged,
			change:    restore,
		},
		{
			// A cancelled key deletion gives back the ability to decrypt objects that are
			// no longer in the bucket, which is nothing at all.
			name:      "taking back a purge whose objects have since been deleted",
			from:      CopyStateDeleted,
			requested: CopyStateStaged,
			change:    restore,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, copies, _ := newRecordLedger(t)
			c := seedCopy(t, table, copyIn(tt.from))
			before := storedRecordItem(t, table, c.CopyID)
			attempts := countRecordWrites(t, table)

			_, err := tt.change(copies, c.CopyID)

			var refused *ForbiddenCopyTransitionError
			if !errors.As(err, &refused) {
				t.Fatalf("expected the change to be refused, got %v", err)
			}
			if refused.Current != tt.from {
				t.Errorf("expected the refusal to report state %q, got %q", tt.from, refused.Current)
			}
			if refused.Requested != tt.requested {
				t.Errorf("expected the refusal to name %q as requested, got %q", tt.requested, refused.Requested)
			}
			if slices.Contains(refused.PermittedFrom, tt.from) {
				t.Errorf("the refusal lists %q among the states the change is permitted from: %v",
					tt.from, refused.PermittedFrom)
			}
			// The operator has to be told what the copy is, or the refusal says only that
			// something was wrong.
			if !strings.Contains(err.Error(), string(tt.from)) {
				t.Errorf("the refusal should name the current state, got: %v", err)
			}

			// The lifecycle is checked against the state that was read, so a forbidden
			// change is refused before anything is attempted against the table.
			assertRefusedWithoutAWrite(t, table, c.CopyID, before, attempts)
		})
	}
}

// A transfer reports through a callback that may well deliver the same result twice, so a
// repeated outcome has to read as nothing to do rather than as a failure.
func TestARepeatedCopyOutcomeReportsAsAlreadyInThatState(t *testing.T) {
	table, copies, _ := newRecordLedger(t)
	c := seedCopy(t, table, copyIn(CopyStateStaged))

	_, err := copies.MarkStaged(context.Background(), c.CopyID, CopyOutcome{ObjectCount: 12})

	var refused *ForbiddenCopyTransitionError
	if !errors.As(err, &refused) {
		t.Fatalf("expected the repeated report to be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), "already in state") {
		t.Errorf("a repeated report should read as nothing to do, got: %v", err)
	}
}

// The second defence: the state the change was checked against is a condition of the write,
// so a copy that moved on in between keeps whatever happened to it instead of being
// overwritten -- and the refusal reports the state it actually found.
func TestACopyChangeRefusedAtTheWriteReportsWhatItFoundInstead(t *testing.T) {
	ctx := context.Background()
	table, copies, _ := newRecordLedger(t)
	c := seedCopy(t, table, copyIn(CopyStateCopying))

	// Both seeded copies carry the same identifier, so the second stands for the same copy
	// having been purged between the read and the write. The write itself is left to the
	// table, so the refusal comes from the condition the store attached.
	write := table.UpdateItemFn
	table.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		seedCopy(t, table, copyIn(CopyStatePurged))
		return write(ctx, params)
	}

	_, err := copies.MarkStaged(ctx, c.CopyID, CopyOutcome{ObjectCount: 12, TotalBytes: 1 << 30})

	var refused *ForbiddenCopyTransitionError
	if !errors.As(err, &refused) {
		t.Fatalf("expected the report to be refused, got %v", err)
	}
	if refused.Current != CopyStatePurged {
		t.Errorf("expected the refusal to report the state it found, %q, got %q",
			CopyStatePurged, refused.Current)
	}

	// The purge has to survive: overwriting it would mark a copy grantable whose key is
	// already scheduled for destruction.
	stored, err := copies.Get(ctx, c.CopyID)
	if err != nil {
		t.Fatalf("reading back the copy: %v", err)
	}
	if stored.State != CopyStatePurged || stored.PurgedAt == nil {
		t.Errorf("expected the purge to stand, got %+v", stored)
	}
}

func TestMarkStagedRecordsWhatTheTransferDelivered(t *testing.T) {
	table, copies, _ := newRecordLedger(t)
	c := seedCopy(t, table, copyIn(CopyStateCopying))

	outcome := CopyOutcome{ObjectCount: 12, TotalBytes: 1 << 30, SummaryLocation: recordCopySummary}
	staged, err := copies.MarkStaged(context.Background(), c.CopyID, outcome)
	if err != nil {
		t.Fatalf("recording a completed transfer: %v", err)
	}

	if staged.State != CopyStateStaged {
		t.Errorf("expected state %q, got %q", CopyStateStaged, staged.State)
	}
	if !staged.IsGrantable() {
		t.Error("a staged copy is what a grant is made against")
	}
	if staged.ObjectCount != outcome.ObjectCount || staged.TotalBytes != outcome.TotalBytes {
		t.Errorf("expected %d objects and %d bytes, got %d and %d",
			outcome.ObjectCount, outcome.TotalBytes, staged.ObjectCount, staged.TotalBytes)
	}
	// The copy service's execution history does not persist indefinitely, so the ledger is
	// the only durable account of which objects staged.
	if staged.CopySummaryLocation != outcome.SummaryLocation {
		t.Errorf("expected the summary at %q, got %q", outcome.SummaryLocation, staged.CopySummaryLocation)
	}

	stored, err := copies.Get(context.Background(), c.CopyID)
	if err != nil {
		t.Fatalf("reading back the staged copy: %v", err)
	}
	assertCopiesEqual(t, staged, stored)
}

// A failed transfer has to be distinguishable from one that delivered, because that is the
// whole of what stops the dataset being granted, and what it managed before failing is
// worth keeping so the failure can be investigated.
func TestMarkFailedIsDistinguishableFromStaged(t *testing.T) {
	table, copies, _ := newRecordLedger(t)
	c := seedCopy(t, table, copyIn(CopyStateCopying))

	failed, err := copies.MarkFailed(context.Background(), c.CopyID, CopyOutcome{
		ObjectCount:     3,
		TotalBytes:      4096,
		SummaryLocation: recordCopySummary,
	})
	if err != nil {
		t.Fatalf("recording a failed transfer: %v", err)
	}

	if failed.State != CopyStateFailed {
		t.Fatalf("expected state %q, got %q", CopyStateFailed, failed.State)
	}
	if failed.IsGrantable() {
		t.Error("a failed transfer must not be grantable")
	}
	if failed.ObjectCount != 3 || failed.CopySummaryLocation != recordCopySummary {
		t.Errorf("expected what the transfer managed before failing to be recorded, got %+v", failed)
	}
}

// The key is destroyed per dataset rather than per copy, so a transfer still running, or
// one that failed, is as purgeable as one that delivered.
//
// The two states it does not apply from are the two that come after it: a purged copy is
// already there, and one whose objects have since been deleted has moved past it for
// good.
func TestMarkPurgedAppliesFromEveryStateACopyCanBeIn(t *testing.T) {
	deletionDate := recordClock.Add(30 * 24 * time.Hour)

	for _, from := range CopyStates() {
		if from == CopyStatePurged || from == CopyStateDeleted {
			continue
		}
		t.Run(string(from), func(t *testing.T) {
			table, copies, _ := newRecordLedger(t)
			c := seedCopy(t, table, copyIn(from))

			purged, err := copies.MarkPurged(context.Background(), c.CopyID, deletionDate)
			if err != nil {
				t.Fatalf("purging a copy in state %q: %v", from, err)
			}

			if purged.State != CopyStatePurged {
				t.Errorf("expected state %q, got %q", CopyStatePurged, purged.State)
			}
			if purged.PurgedAt == nil || !purged.PurgedAt.Equal(recordClock) {
				t.Errorf("expected the purge timestamped %s, got %v", recordClock, purged.PurgedAt)
			}
			// The date the destruction stops being reversible is the one thing an operator
			// may still act on.
			if purged.KeyDeletionDate == nil || !purged.KeyDeletionDate.Equal(deletionDate) {
				t.Errorf("expected the key deletion date %s, got %v", deletionDate, purged.KeyDeletionDate)
			}
			if purged.IsGrantable() {
				t.Error("a purged copy must not be grantable")
			}
			if !purged.IsDestroyed(deletionDate) {
				t.Error("expected the copy to be unreadable for good once the deletion date arrives")
			}
		})
	}
}

// A purge is the one thing here with a way back, and taking it back has to leave a record
// that can be granted without qualification: the deletion of the key was cancelled inside
// its window, so the objects the copy staged decrypt again.
//
// The dates the purge wrote go with it. A copy carrying the state that says it can be
// granted and the date its destruction becomes irreversible would say two things at once,
// and a listing would have to pick one of them to believe. Everything the transfer itself
// recorded stays: the objects never moved, so nothing about what arrived has changed.
func TestMarkRestoredTakesBackAPurgeAndKeepsWhatTheTransferDelivered(t *testing.T) {
	table, copies, _ := newRecordLedger(t)
	before := copyIn(CopyStatePurged)
	c := seedCopy(t, table, before)

	restored, err := copies.MarkRestored(context.Background(), c.CopyID)
	if err != nil {
		t.Fatalf("taking back the purge of a purged copy: %v", err)
	}

	if restored.State != CopyStateStaged {
		t.Errorf("expected state %q, got %q", CopyStateStaged, restored.State)
	}
	if !restored.IsGrantable() {
		t.Error("a copy whose key decrypts again is one a grant can be made against")
	}
	if restored.PurgedAt != nil || restored.KeyDeletionDate != nil {
		t.Errorf("the restored copy is still counting down to its own destruction: %+v", restored)
	}
	// Nothing it can be measured against makes it destroyed any more, however far ahead
	// the date is read from.
	if restored.IsDestroyed(recordClock.Add(365 * 24 * time.Hour)) {
		t.Error("a copy whose key deletion was cancelled is not destroyed by any date")
	}

	if restored.CopyExecutionArn != before.CopyExecutionArn ||
		restored.ObjectCount != before.ObjectCount ||
		restored.TotalBytes != before.TotalBytes ||
		restored.CopySummaryLocation != before.CopySummaryLocation {
		t.Errorf("taking the purge back rewrote what the transfer delivered: %+v", restored)
	}

	// Cleared out of the stored item rather than merely absent from what came back: an
	// attribute left behind would read back as a purge on the next read of the record.
	item := storedRecordItem(t, table, c.CopyID)
	for _, attribute := range []string{purgedAtAttribute, keyDeletionDateAttribute} {
		if _, present := item[attribute]; present {
			t.Errorf("the stored record of a copy that can be granted still carries %q", attribute)
		}
	}

	stored, err := copies.Get(context.Background(), c.CopyID)
	if err != nil {
		t.Fatalf("reading back the restored copy: %v", err)
	}
	assertCopiesEqual(t, restored, stored)
}

// A purge and the taking back of it are a round trip, and what comes back has to be the
// record that went in. Nothing was transferred to get here -- the objects sat in the bucket
// throughout -- so a copy that gained or lost anything on the way would be describing a
// transfer that never happened. It can then be purged again, since taking one back is not a
// door that closes behind it.
func TestACopyPurgedAndStagedAgainIsTheRecordItWas(t *testing.T) {
	ctx := context.Background()
	table, copies, _ := newRecordLedger(t)
	staged := seedCopy(t, table, copyIn(CopyStateStaged))
	before := storedRecordItem(t, table, staged.CopyID)

	purged, err := copies.MarkPurged(ctx, staged.CopyID, recordClock.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("purging: %v", err)
	}
	if purged.PurgedAt == nil || purged.KeyDeletionDate == nil {
		t.Fatalf("the purge recorded no dates for the restore to clear: %+v", purged)
	}

	restored, err := copies.MarkRestored(ctx, staged.CopyID)
	if err != nil {
		t.Fatalf("taking the purge back: %v", err)
	}
	assertCopiesEqual(t, staged, restored)

	after := storedRecordItem(t, table, staged.CopyID)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the round trip altered the stored record:\nbefore %v\n after %v", before, after)
	}

	if _, err := copies.MarkPurged(ctx, staged.CopyID, recordClock.Add(7*24*time.Hour)); err != nil {
		t.Errorf("purging a copy whose earlier purge was taken back: %v", err)
	}
}

// Deleting the objects applies from the two states where nobody is reading them, and from
// nowhere else. The two are reached quite differently -- one dataset was purged and one
// never delivered -- so both are exercised, and both have to keep the whole of the history
// that got them there: the deletion adds a timestamp and unsays nothing.
func TestMarkDeletedAppliesWhereNobodyIsReadingTheObjects(t *testing.T) {
	for _, from := range []CopyState{CopyStatePurged, CopyStateFailed} {
		t.Run(string(from), func(t *testing.T) {
			table, copies, _ := newRecordLedger(t)
			before := copyIn(from)
			c := seedCopy(t, table, before)

			deleted, err := copies.MarkDeleted(context.Background(), c.CopyID)
			if err != nil {
				t.Fatalf("deleting the objects of a copy in state %q: %v", from, err)
			}

			if deleted.State != CopyStateDeleted {
				t.Errorf("expected state %q, got %q", CopyStateDeleted, deleted.State)
			}
			// Timestamped by the store's own clock, the way a purge is: the caller has
			// already deleted the objects and has no better moment to offer than this.
			if deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(recordClock) {
				t.Errorf("expected the deletion timestamped %s, got %v", recordClock, deleted.DeletedAt)
			}
			if deleted.IsGrantable() || deleted.IsDeletable() {
				t.Error("a copy whose objects are gone can be neither granted nor deleted again")
			}

			// What the transfer delivered, and the destruction of the key protecting it,
			// are the record of what happened and survive the objects themselves.
			if deleted.ObjectCount != before.ObjectCount || deleted.TotalBytes != before.TotalBytes {
				t.Errorf("the deletion rewrote what the transfer delivered: %+v", deleted)
			}
			if from == CopyStatePurged {
				if deleted.PurgedAt == nil || deleted.KeyDeletionDate == nil {
					t.Errorf("the deletion lost the purge that preceded it: %+v", deleted)
				}
			}

			stored, err := copies.Get(context.Background(), c.CopyID)
			if err != nil {
				t.Fatalf("reading back the deleted copy: %v", err)
			}
			assertCopiesEqual(t, deleted, stored)
		})
	}
}

// A copy written and then read back through the store has to come out as it went in,
// including the shape most easily lost: one that delivered without writing a summary of its
// own, whose absent attribute must not read back as a location.
func TestACopySurvivesTheWriteAndReadPath(t *testing.T) {
	table, copies, _ := newRecordLedger(t)

	created, err := copies.Create(context.Background(), CreateCopyInput{
		DatasetURI:       recordDataset,
		HotCacheBucket:   recordCacheBucket,
		KeyArn:           recordKeyArn,
		CopyExecutionArn: recordCopyExecution,
	})
	if err != nil {
		t.Fatalf("creating a copy: %v", err)
	}

	staged, err := copies.MarkStaged(context.Background(), created.CopyID, CopyOutcome{
		ObjectCount: 4096,
		TotalBytes:  1 << 42,
	})
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}
	if staged.CopySummaryLocation != "" {
		t.Errorf("expected no summary, got %q", staged.CopySummaryLocation)
	}
	if staged.CopyExecutionArn != recordCopyExecution {
		t.Errorf("reporting lost the execution the transfer ran under, got %+v", staged)
	}

	stored, err := copies.Get(context.Background(), created.CopyID)
	if err != nil {
		t.Fatalf("reading back the copy: %v", err)
	}
	assertCopiesEqual(t, staged, stored)

	item := storedRecordItem(t, table, created.CopyID)
	// A copy names no researcher at any point in its life, so it never joins the index
	// that answers what an account can read, and it has not been purged.
	for _, attribute := range []string{researcherAccountAttribute, purgedAtAttribute, keyDeletionDateAttribute} {
		if _, present := item[attribute]; present {
			t.Errorf("a staged copy must store no %q", attribute)
		}
	}
}

// The copies of one dataset are found through an index the grants of that dataset are in
// too, under the same key. A grant answering this question would report a transfer that
// some other record performed.
func TestCopiesOfDatasetIgnoresTheGrantsOfTheSameDataset(t *testing.T) {
	table, copies, _ := newRecordLedger(t)

	newest := seedCopy(t, table, datasetCopy(copyIdentifier(1), CopyStateStaged, recordClock.Add(-time.Hour)))
	oldest := seedCopy(t, table, datasetCopy(copyIdentifier(2), CopyStatePurged, recordClock.Add(-90*24*time.Hour)))
	seedGrant(t, table, grantIn(GrantStateGranted))
	table.Items["01JSHARE00000000000000000A"] = otherMechanismItem("01JSHARE00000000000000000A")

	found, err := copies.CopiesOfDataset(context.Background(), recordDataset)
	if err != nil {
		t.Fatalf("finding the copies of a dataset: %v", err)
	}

	// Newest first, and both of them: staging has to see a copy that already covers the
	// dataset rather than starting a second billed transfer, and purge has to name every
	// copy the destruction of the key will take with it.
	want := []string{newest.CopyID, oldest.CopyID}
	if got := copyIDs(found); !slices.Equal(got, want) {
		t.Errorf("expected %v newest first, got %v", want, got)
	}
}

func TestCopiesOfDatasetUsesTheDatasetIndex(t *testing.T) {
	table, copies, _ := newRecordLedger(t)

	var seen *dynamodb.QueryInput
	table.QueryFn = func(_ context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		seen = params
		return &dynamodb.QueryOutput{}, nil
	}

	if _, err := copies.CopiesOfDataset(context.Background(), recordDataset); err != nil {
		t.Fatalf("finding the copies of a dataset: %v", err)
	}

	if aws.ToString(seen.IndexName) != DatasetIndexName {
		t.Errorf("expected the dataset index %q, got %q", DatasetIndexName, aws.ToString(seen.IndexName))
	}
	if aws.ToBool(seen.ScanIndexForward) {
		t.Error("expected the newest copy first")
	}
	assertAsksForKind(t, seen.FilterExpression, seen.ExpressionAttributeNames, seen.ExpressionAttributeValues, kindCopy)
}

// Every page is drained rather than the first one read. The callers act on what comes back,
// and a partial answer looks exactly like a dataset with fewer copies than it has -- which
// would start a second billed transfer, or purge a key while leaving a copy unaccounted
// for. The walk over the pages is shared with the grant store, so this covers both.
func TestCopiesOfDatasetDrainsEveryPage(t *testing.T) {
	table, copies, _ := newRecordLedger(t)

	newest := datasetCopy(copyIdentifier(1), CopyStateStaged, recordClock.Add(-time.Hour))
	oldest := datasetCopy(copyIdentifier(2), CopyStatePurged, recordClock.Add(-90*24*time.Hour))

	pages := 0
	table.QueryFn = func(_ context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		pages++
		if params.ExclusiveStartKey == nil {
			return &dynamodb.QueryOutput{
				Items:            []map[string]types.AttributeValue{copyItemOf(t, newest)},
				LastEvaluatedKey: itemKey(newest.CopyID),
			}, nil
		}
		return &dynamodb.QueryOutput{
			Items: []map[string]types.AttributeValue{copyItemOf(t, oldest)},
		}, nil
	}

	found, err := copies.CopiesOfDataset(context.Background(), recordDataset)
	if err != nil {
		t.Fatalf("finding the copies of a dataset: %v", err)
	}

	if pages != 2 {
		t.Fatalf("expected both pages to be read, got %d query(s)", pages)
	}
	want := []string{newest.CopyID, oldest.CopyID}
	if got := copyIDs(found); !slices.Equal(got, want) {
		t.Errorf("expected %v in the order the index gave them, got %v", want, got)
	}
}

func copyItemOf(t *testing.T, c *Copy) map[string]types.AttributeValue {
	t.Helper()

	item, err := marshalCopy(c)
	if err != nil {
		t.Fatalf("marshaling copy %q: %v", c.CopyID, err)
	}
	return item
}

func TestCopiesOfDatasetNeedsADataset(t *testing.T) {
	_, copies, _ := newRecordLedger(t)

	if _, err := copies.CopiesOfDataset(context.Background(), "  "); err == nil {
		t.Fatal("expected a blank dataset URI to be refused")
	}
}

// A listing that hid the failed and the purged would answer "what can be granted?" rather
// than "what has been transferred?", which is the question an operator is usually asking.
func TestCopyListAllReportsEveryCopyNewestFirst(t *testing.T) {
	table, copies, _ := newRecordLedger(t)

	newest := seedCopy(t, table, datasetCopy(copyIdentifier(1), CopyStateStaged, recordClock.Add(-time.Hour)))
	middle := seedCopy(t, table, datasetCopy(copyIdentifier(2), CopyStateFailed, recordClock.Add(-2*time.Hour)))
	oldest := seedCopy(t, table, datasetCopy(copyIdentifier(3), CopyStatePurged, recordClock.Add(-3*time.Hour)))
	seedGrant(t, table, grantIn(GrantStateGranted))
	table.Items["01JSHARE00000000000000000A"] = otherMechanismItem("01JSHARE00000000000000000A")
	table.Items["01JRELEASE0000000000000000"] = undividedItem("01JRELEASE0000000000000000")

	var seen *dynamodb.ScanInput
	scan := table.ScanFn
	table.ScanFn = func(ctx context.Context, params *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		seen = params
		return scan(ctx, params)
	}

	found, err := copies.ListAll(context.Background())
	if err != nil {
		t.Fatalf("listing every copy: %v", err)
	}

	want := []string{newest.CopyID, middle.CopyID, oldest.CopyID}
	if got := copyIDs(found); !slices.Equal(got, want) {
		t.Fatalf("expected %v newest first, got %v", want, got)
	}
	assertAsksForKind(t, seen.FilterExpression, seen.ExpressionAttributeNames, seen.ExpressionAttributeValues, kindCopy)
}

// assertAsksForKind checks a read of the shared table is scoped to one kind of this
// mechanism's records, so neither the other mechanism's items nor the other kind are paid
// for or returned.
func assertAsksForKind(
	t *testing.T,
	filter *string,
	names map[string]string,
	values map[string]types.AttributeValue,
	want itemKind,
) {
	t.Helper()

	if filter == nil {
		t.Fatal("expected a filter scoping the read to one kind of record")
	}
	if got, ok := names["#mechanism"]; !ok || got != model.MechanismAttribute {
		t.Errorf("expected #mechanism to alias %q, got %v", model.MechanismAttribute, names)
	}
	if got, ok := names["#kind"]; !ok || got != kindAttribute {
		t.Errorf("expected #kind to alias %q, got %v", kindAttribute, names)
	}
	if got := attributeText(values[":releaseMechanism"]); got != "S:"+string(model.MechanismS3) {
		t.Errorf("expected the filter to compare against mechanism %q, got %v", model.MechanismS3, got)
	}
	if got := attributeText(values[":kind"]); got != "S:"+string(want) {
		t.Errorf("expected the filter to compare against kind %q, got %v", want, got)
	}
	// An absent attribute must not qualify: every copy and every grant writes both
	// markers, and a record carrying neither is one this store cannot act on.
	if strings.Contains(*filter, "attribute_not_exists") {
		t.Errorf("absence must not qualify a record for this read: %q", *filter)
	}
}
