package dataset

// These tests cover the ledger read and write path for grants.
//
// What they are mostly about is that a grant is one researcher's access and nothing else.
// Several may stand on one copy at once, so withdrawing one has to leave the others exactly
// as they were; a withdrawal is final, so nothing may talk a revoked grant into standing
// again or move the timestamp saying when it ended; and an expiry that has passed is not a
// withdrawal at all, so it may still be moved forward.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/model"
)

// grantIn builds a grant in the given state against the seeded copy, carrying everything it
// would have accumulated by the time it got there.
func grantIn(state GrantState) *Grant {
	expiresAt := recordClock.Add(90 * 24 * time.Hour)
	g := &Grant{
		GrantID:             seededGrantID,
		CopyID:              seededCopyID,
		DatasetURI:          recordDataset,
		HotCacheBucket:      recordCacheBucket,
		KeyArn:              recordKeyArn,
		ResearcherAccountID: recordResearcher,
		TargetVpcID:         recordTargetVpc,
		State:               state,
		CreatedAt:           recordClock.Add(-30 * 24 * time.Hour),
		ExpiresAt:           &expiresAt,
	}

	if state == GrantStateRevoked {
		revokedAt := recordClock.Add(-time.Hour)
		g.RevokedAt = &revokedAt
	}
	return g
}

// datasetGrant builds a grant against one copy with its own identifier and creation time,
// for the listings.
func datasetGrant(id, copyID string, state GrantState, createdAt time.Time) *Grant {
	g := grantIn(state)
	g.GrantID = id
	g.CopyID = copyID
	g.CreatedAt = createdAt
	return g
}

// seedGrant writes a grant straight into the table, so a test can start from a state
// without walking the lifecycle to get there.
func seedGrant(t *testing.T, table *awstest.DynamoDB, g *Grant) *Grant {
	t.Helper()

	item, err := marshalGrant(g)
	if err != nil {
		t.Fatalf("seeding grant %q: %v", g.GrantID, err)
	}
	table.Items[g.GrantID] = item
	return g
}

// stagedCopy records a copy that delivered its dataset, which is the only thing a grant can
// be made against.
func stagedCopy(t *testing.T, table *awstest.DynamoDB, id string) *Copy {
	t.Helper()

	c := copyIn(CopyStateStaged)
	c.CopyID = id
	return seedCopy(t, table, c)
}

func assertGrantsEqual(t *testing.T, want, got *Grant) {
	t.Helper()

	if got.GrantID != want.GrantID {
		t.Errorf("GrantID: expected %q, got %q", want.GrantID, got.GrantID)
	}
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
	if got.ResearcherAccountID != want.ResearcherAccountID {
		t.Errorf("ResearcherAccountID: expected %q, got %q", want.ResearcherAccountID, got.ResearcherAccountID)
	}
	if got.TargetVpcID != want.TargetVpcID {
		t.Errorf("TargetVpcID: expected %q, got %q", want.TargetVpcID, got.TargetVpcID)
	}
	if got.State != want.State {
		t.Errorf("State: expected %q, got %q", want.State, got.State)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt: expected %s, got %s", want.CreatedAt, got.CreatedAt)
	}
	assertSameInstant(t, "ExpiresAt", want.ExpiresAt, got.ExpiresAt)
	assertSameInstant(t, "RevokedAt", want.RevokedAt, got.RevokedAt)
}

func grantIDs(grants []*Grant) []string {
	ids := make([]string, 0, len(grants))
	for _, g := range grants {
		ids = append(ids, g.GrantID)
	}
	return ids
}

// A grant carries the copy's dataset, bucket and key alongside its own fields, so that it
// renders its own policies and prints its own row without reading anything else.
func TestCreateGrantsAccessToAStagedCopy(t *testing.T) {
	table, _, grants := newRecordLedger(t)
	c := stagedCopy(t, table, copyIdentifier(1))

	expiresAt := recordClock.Add(90 * 24 * time.Hour)
	granted, err := grants.Create(context.Background(), c, GrantRequest{
		ResearcherAccountID: recordResearcher,
		TargetVpcID:         recordTargetVpc,
		ExpiresAt:           &expiresAt,
	})
	if err != nil {
		t.Fatalf("granting access to a staged copy: %v", err)
	}

	if granted.GrantID != grantIdentifier(1) {
		t.Errorf("expected the minted identifier %q, got %q", grantIdentifier(1), granted.GrantID)
	}
	if granted.State != GrantStateGranted {
		t.Errorf("expected state %q, got %q", GrantStateGranted, granted.State)
	}
	if !granted.CreatedAt.Equal(recordClock) {
		t.Errorf("CreatedAt: expected %s, got %s", recordClock, granted.CreatedAt)
	}
	if !granted.IsActive(recordClock) {
		t.Error("expected the grant to give access")
	}
	if granted.CopyID != c.CopyID {
		t.Errorf("expected the grant to name copy %q, got %q", c.CopyID, granted.CopyID)
	}
	if granted.DatasetURI != c.DatasetURI || granted.HotCacheBucket != c.HotCacheBucket || granted.KeyArn != c.KeyArn {
		t.Errorf("expected the copy's dataset, bucket and key to be carried onto the grant, got %+v", granted)
	}

	stored, err := grants.Get(context.Background(), granted.GrantID)
	if err != nil {
		t.Fatalf("reading back the grant just made: %v", err)
	}
	assertGrantsEqual(t, granted, stored)
}

// The bytes either arrived or they did not. A bucket holding part of a dataset looks
// exactly like one holding all of it, so this refusal is the whole of what keeps a
// truncated dataset away from a researcher.
func TestCreateRefusesACopyThatDidNotDeliver(t *testing.T) {
	for _, from := range CopyStates() {
		if from == CopyStateStaged {
			continue
		}
		t.Run(string(from), func(t *testing.T) {
			table, _, grants := newRecordLedger(t)
			c := seedCopy(t, table, copyIn(from))
			before := len(table.Items)

			_, err := grants.Create(context.Background(), c, GrantRequest{
				ResearcherAccountID: recordResearcher,
				TargetVpcID:         recordTargetVpc,
			})

			var refused *NotGrantableError
			if !errors.As(err, &refused) {
				t.Fatalf("expected the grant to be refused, got %v", err)
			}
			if refused.Current != from {
				t.Errorf("expected the refusal to report state %q, got %q", from, refused.Current)
			}
			if !strings.Contains(err.Error(), string(from)) {
				t.Errorf("the refusal should name the state the copy is in, got: %v", err)
			}
			if len(table.Items) != before {
				t.Errorf("a refused grant wrote %d item(s)", len(table.Items)-before)
			}
		})
	}
}

// A grant naming no account or no VPC would be recorded happily and then render a policy
// granting nothing, or worse one whose condition is absent.
func TestCreateRefusesAnIncompleteGrant(t *testing.T) {
	incompleteCopy := func(mutate func(*Copy)) *Copy {
		c := copyIn(CopyStateStaged)
		mutate(c)
		return c
	}

	tests := []struct {
		name    string
		copy    *Copy
		request GrantRequest
	}{
		{
			name:    "no researcher account",
			copy:    copyIn(CopyStateStaged),
			request: GrantRequest{TargetVpcID: recordTargetVpc},
		},
		{
			name:    "no target VPC",
			copy:    copyIn(CopyStateStaged),
			request: GrantRequest{ResearcherAccountID: recordResearcher},
		},
		{
			name:    "blank target VPC",
			copy:    copyIn(CopyStateStaged),
			request: GrantRequest{ResearcherAccountID: recordResearcher, TargetVpcID: " "},
		},
		{
			// The dataset URI is the key the grant is found under, so a grant without one
			// would be written and then found by nothing.
			name:    "a copy with no dataset",
			copy:    incompleteCopy(func(c *Copy) { c.DatasetURI = "" }),
			request: GrantRequest{ResearcherAccountID: recordResearcher, TargetVpcID: recordTargetVpc},
		},
		{
			name:    "a copy with no bucket",
			copy:    incompleteCopy(func(c *Copy) { c.HotCacheBucket = "" }),
			request: GrantRequest{ResearcherAccountID: recordResearcher, TargetVpcID: recordTargetVpc},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, _, grants := newRecordLedger(t)

			if _, err := grants.Create(context.Background(), tt.copy, tt.request); err == nil {
				t.Fatal("expected the grant to be refused")
			}
			if len(table.Items) != 0 {
				t.Errorf("a refused grant wrote %d item(s)", len(table.Items))
			}
		})
	}
}

// One dataset released to three researchers is three grants against one copy, not one
// record describing three. Withdrawing one has to leave the other two exactly as they were,
// and nothing is transferred a second time for any of them.
func TestSeveralGrantsCoexistOnOneCopy(t *testing.T) {
	ctx := context.Background()
	table, _, grants := newRecordLedger(t)
	c := stagedCopy(t, table, copyIdentifier(1))

	held := make([]*Grant, 0, 3)
	for _, account := range []string{"123456789012", "777788889999", "999988887777"} {
		granted, err := grants.Create(ctx, c, GrantRequest{
			ResearcherAccountID: account,
			TargetVpcID:         recordTargetVpc,
		})
		if err != nil {
			t.Fatalf("granting access to %q: %v", account, err)
		}
		held = append(held, granted)
	}

	found, err := grants.GrantsOfCopy(ctx, c)
	if err != nil {
		t.Fatalf("finding the grants against a copy: %v", err)
	}
	// Made in the same instant, so the ordering falls back to the identifier, which a ULID
	// makes agree with the order they were made in.
	want := []string{held[2].GrantID, held[1].GrantID, held[0].GrantID}
	if got := grantIDs(found); !slices.Equal(got, want) {
		t.Fatalf("expected %v newest first, got %v", want, got)
	}

	revoked, err := grants.MarkRevoked(ctx, held[1].GrantID)
	if err != nil {
		t.Fatalf("withdrawing one of the grants: %v", err)
	}
	if revoked.IsActive(recordClock) {
		t.Error("a revoked grant gives nobody access")
	}

	for _, untouched := range []*Grant{held[0], held[2]} {
		stored, err := grants.Get(ctx, untouched.GrantID)
		if err != nil {
			t.Fatalf("reading back grant %q: %v", untouched.GrantID, err)
		}
		if !stored.IsActive(recordClock) {
			t.Errorf("withdrawing one grant took %q with it: %+v", untouched.GrantID, stored)
		}
		assertGrantsEqual(t, untouched, stored)
	}

	// The copy itself is untouched by any of it: a grant is a policy statement, and
	// withdrawing one transfers and destroys nothing.
	stored, err := NewCopyStore(table, recordTableName).Get(ctx, c.CopyID)
	if err != nil {
		t.Fatalf("reading back the copy: %v", err)
	}
	assertCopiesEqual(t, c, stored)
}

func TestGrantGetReportsWhatItCannotActOn(t *testing.T) {
	ctx := context.Background()

	t.Run("no such grant", func(t *testing.T) {
		_, _, grants := newRecordLedger(t)

		_, err := grants.Get(ctx, seededGrantID)
		if !errors.Is(err, ErrGrantNotFound) {
			t.Fatalf("expected ErrGrantNotFound, got %v", err)
		}
	})

	t.Run("a record of the other mechanism", func(t *testing.T) {
		table, _, grants := newRecordLedger(t)
		const shareID = "01JSHARE00000000000000000A"
		table.Items[shareID] = otherMechanismItem(shareID)

		_, err := grants.Get(ctx, shareID)
		if !errors.Is(err, ErrGrantNotFound) {
			t.Fatalf("expected ErrGrantNotFound, got %v", err)
		}
		if !strings.Contains(err.Error(), string(model.MechanismHtsget)) {
			t.Errorf("the refusal should name the mechanism the record belongs to, got: %v", err)
		}
	})

	// The operand of extend and revoke is the grant, not the copy it reads from, so a copy
	// identifier typed here has to say what it is rather than reading as a typo.
	t.Run("a copy identifier", func(t *testing.T) {
		table, _, grants := newRecordLedger(t)
		c := seedCopy(t, table, copyIn(CopyStateStaged))

		_, err := grants.Get(ctx, c.CopyID)
		if !errors.Is(err, ErrGrantNotFound) {
			t.Fatalf("expected ErrGrantNotFound, got %v", err)
		}
		if !strings.Contains(err.Error(), string(kindCopy)) {
			t.Errorf("the refusal should say the identifier names a copy, got: %v", err)
		}
	})
}

// Extending is refused only by the withdrawal that ended the grant, and by there being no
// expiry to move.
func TestGrantExtendRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("a revoked grant", func(t *testing.T) {
		table, _, grants := newRecordLedger(t)
		g := seedGrant(t, table, grantIn(GrantStateRevoked))
		before := storedRecordItem(t, table, g.GrantID)
		attempts := countRecordWrites(t, table)

		_, err := grants.Extend(ctx, g.GrantID, 30*24*time.Hour)

		var refused *GrantNotExtendableError
		if !errors.As(err, &refused) {
			t.Fatalf("expected the extension to be refused, got %v", err)
		}
		if refused.Current != GrantStateRevoked {
			t.Errorf("expected the refusal to report state %q, got %q", GrantStateRevoked, refused.Current)
		}
		assertRefusedWithoutAWrite(t, table, g.GrantID, before, attempts)
	})

	// Nothing to move: an unlimited grant carries no time condition in its deployed
	// policies, so withdrawing its access is a revocation rather than an expiry.
	t.Run("an unlimited grant", func(t *testing.T) {
		table, _, grants := newRecordLedger(t)
		unlimited := grantIn(GrantStateGranted)
		unlimited.ExpiresAt = nil
		g := seedGrant(t, table, unlimited)
		before := storedRecordItem(t, table, g.GrantID)
		attempts := countRecordWrites(t, table)

		_, err := grants.Extend(ctx, g.GrantID, 30*24*time.Hour)
		if !errors.Is(err, ErrUnlimitedGrant) {
			t.Fatalf("expected ErrUnlimitedGrant, got %v", err)
		}
		assertRefusedWithoutAWrite(t, table, g.GrantID, before, attempts)
	})

	t.Run("a duration that moves nothing forward", func(t *testing.T) {
		table, _, grants := newRecordLedger(t)
		g := seedGrant(t, table, grantIn(GrantStateGranted))
		before := storedRecordItem(t, table, g.GrantID)
		attempts := countRecordWrites(t, table)

		for _, duration := range []time.Duration{0, -24 * time.Hour} {
			if _, err := grants.Extend(ctx, g.GrantID, duration); err == nil {
				t.Fatalf("expected a duration of %s to be refused", duration)
			}
		}
		assertRefusedWithoutAWrite(t, table, g.GrantID, before, attempts)
	})

	t.Run("no such grant", func(t *testing.T) {
		_, _, grants := newRecordLedger(t)

		_, err := grants.Extend(ctx, seededGrantID, 30*24*time.Hour)
		if !errors.Is(err, ErrGrantNotFound) {
			t.Fatalf("expected ErrGrantNotFound, got %v", err)
		}
	})
}

// A grant whose expiry has passed keeps its statements and its recorded expiry, so moving
// the expiry forward is all it takes to restore access without anything being transferred
// again. The new expiry runs from now, not from the old one, or an extension of a
// long-lapsed grant would land in the past.
func TestGrantExtendMovesTheExpiryForwardFromNow(t *testing.T) {
	table, _, grants := newRecordLedger(t)

	lapsed := grantIn(GrantStateGranted)
	lapsedAt := recordClock.Add(-60 * 24 * time.Hour)
	lapsed.ExpiresAt = &lapsedAt
	g := seedGrant(t, table, lapsed)

	if g.EffectiveState(recordClock) != GrantStateExpired {
		t.Fatalf("this test starts from a lapsed grant, got %q", g.EffectiveState(recordClock))
	}

	extended, err := grants.Extend(context.Background(), g.GrantID, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("extending a grant whose expiry has passed: %v", err)
	}

	want := recordClock.Add(30 * 24 * time.Hour)
	if extended.ExpiresAt == nil || !extended.ExpiresAt.Equal(want) {
		t.Fatalf("expected the expiry to move to %s, got %v", want, extended.ExpiresAt)
	}
	if extended.EffectiveState(recordClock) != GrantStateGranted {
		t.Errorf("expected the extended grant to give access again, got %q",
			extended.EffectiveState(recordClock))
	}
	if extended.State != GrantStateGranted {
		t.Errorf("extending must not move the stored state, got %q", extended.State)
	}

	stored, err := grants.Get(context.Background(), g.GrantID)
	if err != nil {
		t.Fatalf("reading back the extended grant: %v", err)
	}
	assertGrantsEqual(t, extended, stored)
}

// Withdrawal takes out this grant's statements and nothing else: the objects, the key, and
// the account of what the researcher held all survive it.
func TestRevokingAGrantWithdrawsAccessAndKeepsEverythingElse(t *testing.T) {
	table, _, grants := newRecordLedger(t)
	g := seedGrant(t, table, grantIn(GrantStateGranted))

	revoked, err := grants.MarkRevoked(context.Background(), g.GrantID)
	if err != nil {
		t.Fatalf("recording the withdrawal: %v", err)
	}

	if revoked.State != GrantStateRevoked {
		t.Errorf("expected state %q, got %q", GrantStateRevoked, revoked.State)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(recordClock) {
		t.Errorf("expected the withdrawal timestamped %s, got %v", recordClock, revoked.RevokedAt)
	}
	if revoked.IsActive(recordClock) || revoked.IsDeployed() || revoked.IsExtendable() {
		t.Errorf("a revoked grant gives no access, deploys nothing and extends nowhere, got %+v", revoked)
	}
	// The expiry it had is part of the account of what the researcher held and when.
	assertSameInstant(t, "ExpiresAt", g.ExpiresAt, revoked.ExpiresAt)
	if revoked.HotCacheBucket != recordCacheBucket || revoked.KeyArn != recordKeyArn {
		t.Errorf("withdrawal must leave the copy and its key alone, got %+v", revoked)
	}
}

// There is deliberately no way out of revoked, so a second withdrawal is a repeat of an
// outcome the ledger already holds. It must not move the timestamp saying when the access
// ended, which is the part of the record that says what actually happened.
func TestRevokingAGrantTwiceKeepsTheFirstWithdrawal(t *testing.T) {
	table, _, grants := newRecordLedger(t)
	g := seedGrant(t, table, grantIn(GrantStateRevoked))
	before := storedRecordItem(t, table, g.GrantID)
	attempts := countRecordWrites(t, table)

	_, err := grants.MarkRevoked(context.Background(), g.GrantID)

	var refused *AlreadyRevokedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected the second withdrawal to be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), string(GrantStateRevoked)) {
		t.Errorf("the refusal should name the state the grant is in, got: %v", err)
	}
	assertRefusedWithoutAWrite(t, table, g.GrantID, before, attempts)
}

// The second defence: the state the change was checked against is a condition of the write,
// so a grant withdrawn in between keeps that outcome rather than quietly regaining an
// expiry.
func TestAGrantChangeRefusedAtTheWriteReportsWhatItFoundInstead(t *testing.T) {
	ctx := context.Background()
	table, _, grants := newRecordLedger(t)
	g := seedGrant(t, table, grantIn(GrantStateGranted))

	write := table.UpdateItemFn
	table.UpdateItemFn = func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		seedGrant(t, table, grantIn(GrantStateRevoked))
		return write(ctx, params)
	}

	_, err := grants.Extend(ctx, g.GrantID, 30*24*time.Hour)

	var refused *GrantNotExtendableError
	if !errors.As(err, &refused) {
		t.Fatalf("expected the extension to be refused, got %v", err)
	}
	if refused.Current != GrantStateRevoked {
		t.Errorf("expected the refusal to report the state it found, %q, got %q",
			GrantStateRevoked, refused.Current)
	}

	stored, err := grants.Get(ctx, g.GrantID)
	if err != nil {
		t.Fatalf("reading back the grant: %v", err)
	}
	if stored.State != GrantStateRevoked || stored.RevokedAt == nil {
		t.Errorf("expected the withdrawal to stand, got %+v", stored)
	}
}

// The grants of a copy are found through the dataset index, which holds the copy itself and
// the grants against every other copy of the same dataset. Only this copy's grants may come
// back.
func TestGrantsOfCopyIgnoresTheCopyAndTheGrantsOfOtherCopies(t *testing.T) {
	table, _, grants := newRecordLedger(t)

	wanted := stagedCopy(t, table, copyIdentifier(1))
	stagedCopy(t, table, copyIdentifier(2))

	newest := seedGrant(t, table, datasetGrant(grantIdentifier(1), wanted.CopyID, GrantStateGranted, recordClock.Add(-time.Hour)))
	oldest := seedGrant(t, table, datasetGrant(grantIdentifier(2), wanted.CopyID, GrantStateRevoked, recordClock.Add(-3*time.Hour)))
	seedGrant(t, table, datasetGrant(grantIdentifier(3), copyIdentifier(2), GrantStateGranted, recordClock.Add(-2*time.Hour)))

	found, err := grants.GrantsOfCopy(context.Background(), wanted)
	if err != nil {
		t.Fatalf("finding the grants against a copy: %v", err)
	}

	// The withdrawn one is included: reconciliation asks this to find statements that
	// should no longer be deployed, and a listing that hid them would not report any.
	want := []string{newest.GrantID, oldest.GrantID}
	if got := grantIDs(found); !slices.Equal(got, want) {
		t.Errorf("expected %v newest first, got %v", want, got)
	}
}

func TestGrantsOfCopyNeedsACopyItCanFind(t *testing.T) {
	ctx := context.Background()
	_, _, grants := newRecordLedger(t)

	if _, err := grants.GrantsOfCopy(ctx, nil); err == nil {
		t.Error("expected a missing copy to be refused")
	}
	// Without the dataset there is no index to look in, and without the identifier nothing
	// to narrow it to.
	if _, err := grants.GrantsOfCopy(ctx, &Copy{CopyID: copyIdentifier(1)}); err == nil {
		t.Error("expected a copy with no dataset to be refused")
	}
	if _, err := grants.GrantsOfCopy(ctx, &Copy{DatasetURI: recordDataset}); err == nil {
		t.Error("expected a copy with no identifier to be refused")
	}
}

// Whoever asks who holds a dataset should not have to know how many times it was
// transferred, so this spans every copy of it -- and none of those copies themselves.
func TestGrantsOfDatasetSpansEveryCopyOfIt(t *testing.T) {
	table, _, grants := newRecordLedger(t)

	first := stagedCopy(t, table, copyIdentifier(1))
	second := stagedCopy(t, table, copyIdentifier(2))

	newest := seedGrant(t, table, datasetGrant(grantIdentifier(1), second.CopyID, GrantStateGranted, recordClock.Add(-time.Hour)))
	oldest := seedGrant(t, table, datasetGrant(grantIdentifier(2), first.CopyID, GrantStateGranted, recordClock.Add(-2*time.Hour)))
	table.Items["01JSHARE00000000000000000A"] = otherMechanismItem("01JSHARE00000000000000000A")
	table.Items["01JRELEASE0000000000000000"] = undividedItem("01JRELEASE0000000000000000")

	found, err := grants.GrantsOfDataset(context.Background(), recordDataset)
	if err != nil {
		t.Fatalf("finding the grants of a dataset: %v", err)
	}

	want := []string{newest.GrantID, oldest.GrantID}
	if got := grantIDs(found); !slices.Equal(got, want) {
		t.Errorf("expected %v newest first, got %v", want, got)
	}
}

func TestGrantsOfDatasetNeedsADataset(t *testing.T) {
	_, _, grants := newRecordLedger(t)

	if _, err := grants.GrantsOfDataset(context.Background(), "  "); err == nil {
		t.Fatal("expected a blank dataset URI to be refused")
	}
}

// The per-researcher index covers both mechanisms, so a query over it has to say which
// records it wants -- and drop anything that arrives anyway, since the other mechanism's
// record read as a grant would report access to a dataset it says nothing about.
func TestGrantListByAccountAsksTheTableForGrantsOnly(t *testing.T) {
	table, _, grants := newRecordLedger(t)

	held := seedGrant(t, table, datasetGrant(grantIdentifier(1), copyIdentifier(1), GrantStateGranted, recordClock.Add(-time.Hour)))
	table.Items["01JSHARE00000000000000000A"] = otherMechanismItem("01JSHARE00000000000000000A")
	table.Items["01JRELEASE0000000000000000"] = undividedItem("01JRELEASE0000000000000000")

	var seen *dynamodb.QueryInput
	query := table.QueryFn
	table.QueryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		seen = params
		return query(ctx, params)
	}

	found, err := grants.ListByAccount(context.Background(), recordResearcher)
	if err != nil {
		t.Fatalf("listing the grants of an account: %v", err)
	}

	if got := grantIDs(found); !slices.Equal(got, []string{held.GrantID}) {
		t.Errorf("expected only the grant, got %v", got)
	}
	if aws.ToString(seen.IndexName) != ResearcherIndexName {
		t.Errorf("expected the per-researcher index %q, got %q", ResearcherIndexName, aws.ToString(seen.IndexName))
	}
	if aws.ToBool(seen.ScanIndexForward) {
		t.Error("expected the newest grant first")
	}
	assertAsksForKind(t, seen.FilterExpression, seen.ExpressionAttributeNames, seen.ExpressionAttributeValues, kindGrant)
	// The kind filter is merged into the query's own expression values, and must not
	// displace the key it is keyed on.
	if got := attributeText(seen.ExpressionAttributeValues[":researcherAccountId"]); got != "S:"+recordResearcher {
		t.Errorf("expected the query to be keyed on account %q, got %v", recordResearcher, got)
	}
}

func TestGrantListByAccountNeedsAnAccount(t *testing.T) {
	_, _, grants := newRecordLedger(t)

	if _, err := grants.ListByAccount(context.Background(), ""); err == nil {
		t.Fatal("expected a blank account to be refused")
	}
}

// A listing that hid the withdrawn and the lapsed would answer "who has access?" rather
// than "what has been granted?", which is the question an operator is usually asking.
func TestGrantListAllReportsEveryGrantNewestFirst(t *testing.T) {
	table, _, grants := newRecordLedger(t)

	lapsed := datasetGrant(grantIdentifier(1), copyIdentifier(1), GrantStateGranted, recordClock.Add(-3*time.Hour))
	lapsedAt := recordClock.Add(-time.Hour)
	lapsed.ExpiresAt = &lapsedAt
	seedGrant(t, table, lapsed)

	revoked := seedGrant(t, table, datasetGrant(grantIdentifier(2), copyIdentifier(1), GrantStateRevoked, recordClock.Add(-2*time.Hour)))
	newest := seedGrant(t, table, datasetGrant(grantIdentifier(3), copyIdentifier(2), GrantStateGranted, recordClock.Add(-time.Hour)))
	seedCopy(t, table, copyIn(CopyStateStaged))
	table.Items["01JSHARE00000000000000000A"] = otherMechanismItem("01JSHARE00000000000000000A")

	var seen *dynamodb.ScanInput
	scan := table.ScanFn
	table.ScanFn = func(ctx context.Context, params *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		seen = params
		return scan(ctx, params)
	}

	found, err := grants.ListAll(context.Background())
	if err != nil {
		t.Fatalf("listing every grant: %v", err)
	}

	want := []string{newest.GrantID, revoked.GrantID, lapsed.GrantID}
	if got := grantIDs(found); !slices.Equal(got, want) {
		t.Fatalf("expected %v newest first, got %v", want, got)
	}
	if state := found[2].EffectiveState(recordClock); state != GrantStateExpired {
		t.Errorf("expected the lapsed grant to report as %q, got %q", GrantStateExpired, state)
	}
	assertAsksForKind(t, seen.FilterExpression, seen.ExpressionAttributeNames, seen.ExpressionAttributeValues, kindGrant)
}

// A grant written and then read back through the store has to come out as it went in,
// including the shape most easily lost: one with no expiry, whose absent attribute must not
// read back as a date.
func TestAGrantSurvivesTheWriteAndReadPath(t *testing.T) {
	ctx := context.Background()
	table, _, grants := newRecordLedger(t)
	c := stagedCopy(t, table, copyIdentifier(1))

	granted, err := grants.Create(ctx, c, GrantRequest{
		ResearcherAccountID: recordResearcher,
		TargetVpcID:         recordTargetVpc,
	})
	if err != nil {
		t.Fatalf("granting access: %v", err)
	}
	if !granted.IsUnlimited() {
		t.Errorf("expected an unlimited grant, got expiry %s", granted.ExpiresAt)
	}

	stored, err := grants.Get(ctx, granted.GrantID)
	if err != nil {
		t.Fatalf("reading back the grant: %v", err)
	}
	assertGrantsEqual(t, granted, stored)

	item := storedRecordItem(t, table, granted.GrantID)
	if _, present := item[expiresAtAttribute]; present {
		t.Error("an unlimited grant must store no expiry")
	}
	if _, present := item[revokedAtAttribute]; present {
		t.Error("a grant in force must store no withdrawal")
	}
}
