package cmd

// These tests cover the commands that act on a release already in place: moving a grant's
// expiry, taking it back, destroying the copy it reads from, taking that destruction back
// while its window is still open, and deleting what that copy left behind in the bucket.
//
// All of them are about refusals as much as about what they do, and for the same reason as
// the grant itself: the deployed documents are what a read is actually checked against, so
// a command that refuses has to be seen to have left both of them alone. Two of them carry
// a further weight now that a copy may hold several grants at once -- moving or withdrawing
// one has to leave the others deployed exactly as they were, because the documents are
// rewritten whole every time. The purge tests carry a weight of their own, since a
// scheduled key deletion is the one thing here that cannot be taken back once its window
// elapses, so what an abandoned confirmation leaves behind is worth stating precisely. The
// delete tests carry it twice over: that deletion has no window at all, and it is refused
// wherever anybody could still be reading, so most of what they assert is what a refusal
// left exactly as it was. The restore tests read the purge ones backwards, and what they
// weigh is the two things a restore has to get right at once: the key has to come back able
// to decrypt, and nobody's access may come back with it.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/umccr/data-releaser/internal/dataset"
)

// extendTheGrant runs one extension against the given grant.
func extendTheGrant(t *testing.T, doubles *releaseDoubles, id string, duration time.Duration) (string, error) {
	t.Helper()

	cmd, out, _ := testCommand("")
	err := newTestReleaser(t, doubles).extend(context.Background(), cmd, extendS3Request{
		grantID:  id,
		duration: duration,
	})
	return out.String(), err
}

// TestExtendingAGrantWhoseExpiryHasPassed checks the case the command exists for as much
// as any other: a grant that has lapsed still carries its granting statements and the
// objects are still in the bucket, so moving the expiry forward restores access without
// the dataset being copied again.
//
// The duration runs from now rather than from the expiry it replaces, which is what makes
// that possible at all -- extending from a date already behind us would land the new expiry
// in the past too.
func TestExtendingAGrantWhoseExpiryHasPassed(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)

	lapsed := releaseClock.Add(-24 * time.Hour)
	grant := doubles.seedGrant(t, staged, grantSeed{expiresAt: &lapsed})

	out, err := extendTheGrant(t, doubles, grant.GrantID, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("extending a lapsed grant: %v\n%s", err, out)
	}

	recorded := doubles.attr(t, grant.GrantID, "expiresAt")
	moved, parseErr := time.Parse(time.RFC3339, recorded)
	if parseErr != nil {
		t.Fatalf("recorded expiry %q is not a timestamp: %v", recorded, parseErr)
	}
	if !moved.After(releaseClock) {
		t.Errorf("recorded expiry %s did not move past the present", recorded)
	}

	// The deployed condition and the recorded expiry have to be the same moment: the
	// documents are rendered from the ledger as it now stands, not from a second reading
	// of a clock.
	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	if !strings.Contains(written[0], recorded) {
		t.Errorf("the deployed time condition does not carry the recorded expiry %s:\n%s",
			recorded, written[0])
	}
	if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateGranted) {
		t.Errorf("recorded state = %q; an extension does not move a grant out of granted", got)
	}
}

// TestExtendingOneGrantLeavesTheOthersDeployed checks the consequence of the documents
// being rendered whole.
//
// Two researchers read one copy. Moving one researcher's expiry rewrites both documents,
// and the other researcher's statements have to go back in unchanged -- otherwise an
// extension would silently withdraw somebody nobody mentioned.
func TestExtendingOneGrantLeavesTheOthersDeployed(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)

	grants := doubles.seedGrants(t, staged,
		grantSeed{},
		grantSeed{account: secondResearcher, vpc: secondVpc},
	)
	moved, untouched := grants[0], grants[1]

	out, err := extendTheGrant(t, doubles, moved.GrantID, 60*24*time.Hour)
	if err != nil {
		t.Fatalf("extending one of two grants: %v\n%s", err, out)
	}

	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	for _, want := range []string{releaseResearcher, secondResearcher, untouched.GrantID} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the rewritten bucket policy lost %q:\n%s", want, written[0])
		}
	}

	if got := doubles.attr(t, untouched.GrantID, "expiresAt"); got != untouched.ExpiresAt.Format(time.RFC3339) {
		t.Errorf("the other researcher's expiry moved to %q; only the named grant is extended", got)
	}
	if !strings.Contains(out, "2 grant(s) on this copy") {
		t.Errorf("expected the report to say both grants are still deployed, got:\n%s", out)
	}
}

// TestExtendingIsRefusedForAGrantWithNothingToMove checks the two cases an extension
// cannot reach, and that a refusal leaves the records and both documents exactly as they
// were.
//
// The two are refused for different reasons and the report says which: a withdrawn grant
// has no granting statements left for a time condition to bound, and one whose copy is
// being destroyed is protected by a key whose deletion is already scheduled, so no expiry
// could reach the data anyway.
func TestExtendingIsRefusedForAGrantWithNothingToMove(t *testing.T) {
	cases := map[string]struct {
		reason string
		seed   func(t *testing.T, doubles *releaseDoubles) *dataset.Grant
	}{
		"withdrawn": {
			reason: "no time condition left to move",
			seed: func(t *testing.T, doubles *releaseDoubles) *dataset.Grant {
				staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
				return doubles.seedGrant(t, staged, grantSeed{revoked: true})
			},
		},
		"copy being destroyed": {
			reason: "deletion of the key",
			seed: func(t *testing.T, doubles *releaseDoubles) *dataset.Grant {
				// Granted first and purged afterwards, because that is the only order the
				// ledger allows: a copy has to have staged before anything is granted
				// against it.
				staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
				grant := doubles.seedGrant(t, staged, grantSeed{})
				if _, err := doubles.copyStore().MarkPurged(
					context.Background(), staged.CopyID, releaseClock.Add(30*24*time.Hour)); err != nil {
					t.Fatalf("purging the copy: %v", err)
				}
				return grant
			},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			doubles := newReleaseDoubles()
			grant := c.seed(t, doubles)

			_, err := extendTheGrant(t, doubles, grant.GrantID, 30*24*time.Hour)
			if err == nil {
				t.Fatalf("extending a grant with %s must be refused", name)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("expected the refusal to explain why there is no expiry to move, got %v", err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "nothing was written") {
				t.Errorf("expected the refusal to say nothing was written, got %v", err)
			}

			if got := doubles.attr(t, grant.GrantID, "expiresAt"); got != grant.ExpiresAt.Format(time.RFC3339) {
				t.Errorf("recorded expiry = %q; a refusal must leave it as it was", got)
			}
			if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
				t.Error("a refused extension must write neither document")
			}
		})
	}
}

// TestExtendingAnUnlimitedGrantChangesNothing checks that a grant made without an expiry is
// reported as having none and left exactly as it was.
//
// It succeeds rather than failing, because the state the operator was reaching for --
// access that continues -- already holds. What it must not do is write anything: a grant
// carrying no time condition and one carrying a distant date are different grants, and
// quietly turning the first into the second would narrow an agreement nobody asked to
// narrow.
func TestExtendingAnUnlimitedGrantChangesNothing(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	grant := doubles.seedGrant(t, staged, grantSeed{unlimited: true})

	out, err := extendTheGrant(t, doubles, grant.GrantID, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("extending an unlimited grant should report rather than fail: %v", err)
	}
	if !strings.Contains(out, "No expiry applies") {
		t.Errorf("expected the report to say no expiry applies, got:\n%s", out)
	}
	if got := doubles.attr(t, grant.GrantID, "expiresAt"); got != "" {
		t.Errorf("an unlimited grant gained an expiry of %q", got)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
		t.Error("an unlimited grant must have neither document rewritten")
	}
}

// TestExtendingACopyIdentifierIsRefused checks the mistake the split makes possible: the
// operand of an extension is the grant, and the copy it reads from is a different record
// with an identifier of the same shape.
//
// Naming which kind was found is the difference between an operator retyping the
// identifier and one reaching for the listing to find the grant they meant.
func TestExtendingACopyIdentifierIsRefused(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.seedGrant(t, staged, grantSeed{})

	_, err := extendTheGrant(t, doubles, copyID(1), 30*24*time.Hour)
	if err == nil {
		t.Fatal("extending a copy identifier must be refused")
	}
	if !strings.Contains(err.Error(), "not a grant") {
		t.Errorf("expected the refusal to say the identifier names a copy, got %v", err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 {
		t.Error("a refused extension must write no document")
	}
}

// revokeTheGrant runs one withdrawal against the given grant.
func revokeTheGrant(t *testing.T, doubles *releaseDoubles, id string) (string, error) {
	t.Helper()

	cmd, out, _ := testCommand("")
	err := newTestReleaser(t, doubles).revoke(context.Background(), cmd, id)
	return out.String(), err
}

// TestRevokingWithdrawsAccessAndLeavesTheDataInPlace covers a successful withdrawal: both
// documents rewritten without the researcher, the withdrawal recorded, and the researcher's
// own description of the access removed because they have none left.
//
// Nothing is destroyed, and the report has to say so: the difference between this and a
// purge is the whole reason the two are separate commands.
func TestRevokingWithdrawsAccessAndLeavesTheDataInPlace(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	grant := doubles.seedGrant(t, staged, grantSeed{})

	out, err := revokeTheGrant(t, doubles, grant.GrantID)
	if err != nil {
		t.Fatalf("withdrawing: %v\n%s", err, out)
	}

	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	if strings.Contains(written[0], releaseResearcher) {
		t.Errorf("the rewritten bucket policy still names the researcher:\n%s", written[0])
	}
	if len(doubles.keys.ScheduleDeletionCalls) != 0 || len(doubles.objects.DeleteCalls) == 0 {
		t.Error("a withdrawal destroys nothing and removes the researcher's manifest")
	}

	if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
		t.Errorf("recorded state = %q, want revoked", got)
	}
	if doubles.attr(t, grant.GrantID, "revokedAt") == "" {
		t.Error("the withdrawal recorded no timestamp")
	}
	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStateStaged) {
		t.Errorf("the copy moved to %q; a withdrawal takes nothing away from what was transferred", got)
	}
	if !strings.Contains(out, "untouched") {
		t.Errorf("expected the report to say the objects and key are untouched, got:\n%s", out)
	}

	// The account holds nothing else, so its document is removed rather than left
	// describing access that has ended.
	if removed := manifestDeletes(doubles); len(removed) != 1 {
		t.Errorf("expected the researcher's manifest to be removed, got %v", removed)
	}
}

// TestRevokingOneGrantLeavesTheOtherResearcherReading is the case the split exists for on
// this side.
//
// Two researchers read one copy and one of them is withdrawn. The documents are rewritten
// whole, so the other researcher's statements have to be written back exactly as they were:
// their access is untouched, their record is untouched, and their description of it is not
// removed.
func TestRevokingOneGrantLeavesTheOtherResearcherReading(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)

	grants := doubles.seedGrants(t, staged,
		grantSeed{},
		grantSeed{account: secondResearcher, vpc: secondVpc},
	)
	withdrawn, remaining := grants[0], grants[1]

	out, err := revokeTheGrant(t, doubles, withdrawn.GrantID)
	if err != nil {
		t.Fatalf("withdrawing one of two grants: %v\n%s", err, out)
	}

	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	if strings.Contains(written[0], withdrawn.GrantID) || strings.Contains(written[0], releaseResearcher) {
		t.Errorf("the withdrawn researcher is still in the bucket policy:\n%s", written[0])
	}
	for _, want := range []string{remaining.GrantID, secondResearcher, secondVpc} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the remaining researcher lost %q from the bucket policy:\n%s", want, written[0])
		}
	}

	if got := doubles.attr(t, remaining.GrantID, "state"); got != string(dataset.GrantStateGranted) {
		t.Errorf("the remaining grant is %q; withdrawing one must not touch the other", got)
	}
	if !strings.Contains(out, "1 other grant(s)") {
		t.Errorf("expected the report to say what survived the withdrawal, got:\n%s", out)
	}

	// Only the withdrawn researcher's description goes; the other still holds access to
	// describe.
	removed := manifestDeletes(doubles)
	if len(removed) != 1 {
		t.Fatalf("expected exactly one manifest removed, got %v", removed)
	}
	if !strings.Contains(removed[0], releaseResearcher) {
		t.Errorf("the wrong researcher's manifest was removed: %q", removed[0])
	}
}

// TestRevokingIsRefusedWhereThereIsNoAccessToWithdraw checks that only a deployed grant can
// be withdrawn. A grant already withdrawn has no statements left to take out, so the
// refusal reports when its access ended rather than writing the same documents again.
func TestRevokingIsRefusedWhereThereIsNoAccessToWithdraw(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	grant := doubles.seedGrant(t, staged, grantSeed{revoked: true})

	_, err := revokeTheGrant(t, doubles, grant.GrantID)
	if err == nil {
		t.Fatal("withdrawing a grant that was already withdrawn must be refused")
	}

	var notRevocable *grantNotRevocableError
	if !errors.As(err, &notRevocable) {
		t.Fatalf("expected the refusal to name the grant it found, got %v", err)
	}
	if !strings.Contains(err.Error(), string(dataset.GrantStateRevoked)) {
		t.Errorf("expected the refusal to report the state it found, got %v", err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 {
		t.Error("a refused withdrawal must write no document")
	}
	if got := doubles.attr(t, grant.GrantID, "revokedAt"); got != grant.RevokedAt.Format(time.RFC3339) {
		t.Errorf("the recorded withdrawal moved to %q; a refusal must leave it alone", got)
	}
}

// TestRevokingACopyIdentifierIsRefused is the counterpart of the same mistake on the
// extension: what is withdrawn is one researcher's access, not the copy they read.
func TestRevokingACopyIdentifierIsRefused(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.seedGrant(t, staged, grantSeed{})

	_, err := revokeTheGrant(t, doubles, copyID(1))
	if err == nil {
		t.Fatal("withdrawing a copy identifier must be refused")
	}
	if !strings.Contains(err.Error(), "not a grant") {
		t.Errorf("expected the refusal to say the identifier names a copy, got %v", err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 {
		t.Error("a refused withdrawal must write no document")
	}
}

// TestRevokingAnExpiredGrantIsARealWithdrawal checks that a lapsed grant is still worth
// withdrawing. Its granting statements are still deployed -- expiry is a condition inside
// them, not their removal -- so taking them out is a real change, and refusing it would
// leave a grant standing that an operator believed they had removed.
func TestRevokingAnExpiredGrantIsARealWithdrawal(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)

	lapsed := releaseClock.Add(-24 * time.Hour)
	grant := doubles.seedGrant(t, staged, grantSeed{expiresAt: &lapsed})

	if _, err := revokeTheGrant(t, doubles, grant.GrantID); err != nil {
		t.Fatalf("withdrawing a lapsed grant: %v", err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 1 {
		t.Error("a lapsed grant still carries its granting statements, so they have to be written out")
	}
	if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
		t.Errorf("recorded state = %q, want revoked", got)
	}
}

// TestRevokingReportsAWithdrawalThatCouldNotBeRecorded checks the half-finished case. The
// reads have stopped, which is what was asked for, but the record still reads as deployed
// -- so a listing would report access that does not exist and reconciliation would put the
// statements back. That is worth a loud failure rather than a warning.
func TestRevokingReportsAWithdrawalThatCouldNotBeRecorded(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	grant := doubles.seedGrant(t, staged, grantSeed{})
	doubles.ledger.UpdateItemFn = failingUpdate

	_, err := revokeTheGrant(t, doubles, grant.GrantID)
	if err == nil {
		t.Fatal("a withdrawal that could not be recorded must be reported as a failure")
	}

	var unrecorded *unrecordedRevocationError
	if !errors.As(err, &unrecorded) {
		t.Fatalf("expected the failure to name the withdrawal left unrecorded, got %v", err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 1 {
		t.Error("the reads have stopped: the report is about the record, not the documents")
	}
}

// purgeTheDataset runs one purge, answering the confirmation with the given input.
func purgeTheDataset(t *testing.T, doubles *releaseDoubles, confirmation string) (string, error) {
	t.Helper()

	purger, err := newPurger(releaseConfig(), doubles.clients())
	if err != nil {
		t.Fatalf("assembling the purge command: %v", err)
	}
	purger.now = func() time.Time { return releaseClock }

	cmd, out, _ := testCommand(confirmation)
	return outputOf(out, purger.purge(context.Background(), cmd, purgeRequest{
		datasetURI:        releaseDataset,
		pendingWindowDays: defaultPendingWindowDays,
	}))
}

// TestPurgeDeclinedSchedulesNothingAndListsEveryAffectedRecord checks what an abandoned
// confirmation leaves behind, and what the operator was shown before they abandoned it.
//
// Every copy of the dataset is listed and so is every grant against them, not only the ones
// giving access, because the key is per dataset: an operator reasoning about one record is
// about to destroy all of them, and the ones they were not thinking about are exactly the
// ones worth showing. Declining then has to leave all of them untouched, and exit non-zero
// so that a script can tell a copy that was destroyed from one that was left alone.
func TestPurgeDeclinedSchedulesNothingAndListsEveryAffectedRecord(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.seedCopy(t, copyID(2), dataset.CopyStateFailed)
	grants := doubles.seedGrants(t, staged,
		grantSeed{},
		grantSeed{account: secondResearcher, vpc: secondVpc},
	)

	out, err := purgeTheDataset(t, doubles, "y\n")
	if err == nil {
		t.Fatal("an abandoned purge must exit non-zero so a script can tell it apart from one that ran")
	}

	var declined *purgeDeclinedError
	if !errors.As(err, &declined) {
		t.Fatalf("expected the abandoned purge to be reported as such, got %v", err)
	}

	for _, id := range []string{copyID(1), copyID(2), grants[0].GrantID, grants[1].GrantID} {
		if !strings.Contains(out, id) {
			t.Errorf("expected record %s to be listed before confirmation, got:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "irreversible") {
		t.Errorf("expected the operator to be told the operation is irreversible, got:\n%s", out)
	}

	if len(doubles.keys.ScheduleDeletionCalls) != 0 {
		t.Error("an abandoned purge must schedule no key deletion")
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 {
		t.Error("an abandoned purge must write no policy")
	}
	for _, id := range []string{copyID(1), copyID(2)} {
		if got := doubles.attr(t, id, "state"); got == string(dataset.CopyStatePurged) {
			t.Errorf("copy %s was recorded as purged after the confirmation was abandoned", id)
		}
	}
	for _, grant := range grants {
		if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateGranted) {
			t.Errorf("grant %s moved to %q after the confirmation was abandoned", grant.GrantID, got)
		}
	}
}

// TestPurgeWithdrawsAccessBeforeSchedulingTheKey covers a purge carried through, and above
// all its order.
//
// A scheduled deletion can be cancelled while its window runs, and cancelling it makes the
// key able to decrypt again. A grant left standing in the policies would come back to life
// with it, so every grant has to be out of the documents before the key is scheduled --
// every grant, not one, since a copy may carry several. Every copy of the dataset is then
// recorded as purged, whatever state each was in, because they all die with the key.
func TestPurgeWithdrawsAccessBeforeSchedulingTheKey(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.seedCopy(t, copyID(2), dataset.CopyStateFailed)
	grants := doubles.seedGrants(t, staged,
		grantSeed{},
		grantSeed{account: secondResearcher, vpc: secondVpc},
	)

	out, err := purgeTheDataset(t, doubles, releaseDataset+"\n")
	if err != nil {
		t.Fatalf("purging: %v\n%s", err, out)
	}

	if len(doubles.keys.ScheduleDeletionCalls) != 1 {
		t.Fatalf("expected one key deletion scheduled, got %d", len(doubles.keys.ScheduleDeletionCalls))
	}

	// One pair of writes takes both researchers out at once: the documents are shared by
	// every grant against the copy and are only ever written whole.
	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected the bucket policy rewritten once, got %d", len(written))
	}
	for _, gone := range []string{releaseResearcher, secondResearcher} {
		if strings.Contains(written[0], gone) {
			t.Errorf("account %s is still in the bucket policy after a purge:\n%s", gone, written[0])
		}
	}

	for _, id := range []string{copyID(1), copyID(2)} {
		if got := doubles.attr(t, id, "state"); got != string(dataset.CopyStatePurged) {
			t.Errorf("copy %s state = %q, want purged", id, got)
		}
		if doubles.attr(t, id, "keyDeletionDate") == "" {
			t.Errorf("copy %s recorded no date its destruction becomes irreversible", id)
		}
	}
	for _, grant := range grants {
		if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
			t.Errorf("grant %s state = %q, want revoked", grant.GrantID, got)
		}
		if doubles.attr(t, grant.GrantID, "revokedAt") == "" {
			t.Errorf("grant %s recorded no time its access was withdrawn", grant.GrantID)
		}
	}

	// Neither researcher held anything else, so both descriptions are removed rather than
	// left pointing at data on its way out.
	if removed := manifestDeletes(doubles); len(removed) != 2 {
		t.Errorf("expected both researchers' manifests to be removed, got %v", removed)
	}
}

// TestPurgeOfADatasetAlreadyBeingDestroyedSchedulesNothingFurther checks a repeated purge.
// The key is already counting down, so nothing is scheduled a second time and the date
// reported is the one already in force.
func TestPurgeOfADatasetAlreadyBeingDestroyedSchedulesNothingFurther(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStatePurged)
	doubles.keys.DescribeKeyFn = keyInState(kmstypes.KeyStatePendingDeletion)

	out, err := purgeTheDataset(t, doubles, "")
	if err != nil {
		t.Fatalf("a repeated purge should report and do nothing: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already purged") {
		t.Errorf("expected the report to say the dataset is already purged, got:\n%s", out)
	}
	if len(doubles.keys.ScheduleDeletionCalls) != 0 {
		t.Error("a repeated purge must schedule nothing")
	}
}

// TestPurgeFinishesAJobBegunOutsideThisTool checks the case where the key is already being
// destroyed but the ledger has not caught up: the copy still reads as staged and the grant
// against it still reads as giving access.
//
// Nothing is scheduled again -- the key is already on its way out -- but the bucket policy
// is still closed and the records are brought into line, and the date recorded is the one
// the key is actually counting down to rather than the window that was asked for.
func TestPurgeFinishesAJobBegunOutsideThisTool(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	grant := doubles.seedGrant(t, staged, grantSeed{})
	doubles.keys.DescribeKeyFn = keyInState(kmstypes.KeyStatePendingDeletion)

	out, err := purgeTheDataset(t, doubles, releaseDataset+"\n")
	if err != nil {
		t.Fatalf("purging: %v\n%s", err, out)
	}

	if len(doubles.keys.ScheduleDeletionCalls) != 0 {
		t.Error("a key already being destroyed must not be scheduled again")
	}
	if len(doubles.keys.PutKeyPolicyCalls) != 0 {
		t.Error("a key pending deletion accepts no policy")
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 1 {
		t.Errorf("the bucket policy should still be closed, got %d writes",
			len(doubles.objects.PutBucketPolicyCalls))
	}

	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStatePurged) {
		t.Errorf("recorded copy state = %q, want purged", got)
	}
	if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
		t.Errorf("recorded grant state = %q, want revoked", got)
	}
	want := releaseClock.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	if got := doubles.attr(t, copyID(1), "keyDeletionDate"); got != want {
		t.Errorf("recorded deletion date = %q, want the date the key already reports (%s)", got, want)
	}
}

// TestPurgeOfADatasetNeverCopiedHereIsRefused checks that a dataset with nothing here to
// destroy is refused rather than reported as destroyed, and that the refusal points at the
// likeliest cause: purging is keyed on the dataset, so a URI spelled differently names a
// different one.
func TestPurgeOfADatasetNeverCopiedHereIsRefused(t *testing.T) {
	doubles := newReleaseDoubles()

	_, err := purgeTheDataset(t, doubles, "")
	if err == nil {
		t.Fatal("purging a dataset that was never copied here must be refused")
	}

	var nothing *nothingToPurgeError
	if !errors.As(err, &nothing) {
		t.Fatalf("expected the refusal to say there is nothing here to destroy, got %v", err)
	}
	if len(doubles.keys.ScheduleDeletionCalls) != 0 {
		t.Error("nothing may be scheduled for a dataset with no copy here")
	}
}

// TestPurgeWindowIsBoundedWhereTheConfirmationQuotesIt checks the flag reading, because the
// confirmation quotes the window back: an operator has to be told the same number the
// schedule will actually carry.
func TestPurgeWindowIsBoundedWhereTheConfirmationQuotesIt(t *testing.T) {
	cmd := purgeS3Cmd

	// The command is the process-wide one cobra assembled, so its flags are put back the
	// way they were found rather than left carrying this test's values.
	t.Cleanup(func() {
		_ = cmd.Flags().Set("pending-window-days", fmt.Sprint(defaultPendingWindowDays))
		_ = cmd.Flags().Set("dataset-uri", "")
	})

	for _, window := range []string{"0", "6", "31", "365"} {
		if err := cmd.Flags().Set("pending-window-days", window); err != nil {
			t.Fatalf("setting the window to %s: %v", window, err)
		}
		if err := cmd.Flags().Set("dataset-uri", releaseDataset); err != nil {
			t.Fatalf("setting the dataset: %v", err)
		}
		if _, err := readPurgeRequest(cmd); err == nil {
			t.Errorf("a pending window of %s days must be refused: KMS accepts no such value", window)
		}
	}

	for _, window := range []string{"7", "30"} {
		if err := cmd.Flags().Set("pending-window-days", window); err != nil {
			t.Fatalf("setting the window to %s: %v", window, err)
		}
		if _, err := readPurgeRequest(cmd); err != nil {
			t.Errorf("a pending window of %s days is one KMS accepts, got %v", window, err)
		}
	}
}

// deleteTheObjects runs one deletion, answering the confirmation with the given input.
func deleteTheObjects(t *testing.T, doubles *releaseDoubles, confirmation string) (string, error) {
	t.Helper()

	cmd, out, _ := testCommand(confirmation)
	return outputOf(out, newDeleter(releaseConfig(), doubles.clients()).
		delete(context.Background(), cmd, deleteRequest{datasetURI: releaseDataset}))
}

// TestDeleteDeclinedLeavesEveryObjectWhereItWas checks what an abandoned confirmation
// leaves behind, and what the operator was shown before they abandoned it.
//
// They are shown what deleting buys -- how many objects and how many bytes -- and every
// copy the deletion would be recorded against, because the objects belong to the bucket
// those copies share rather than to any one of them. They are also told the thing that
// most distinguishes this from a purge: there is no window on it. Declining then deletes
// nothing and exits non-zero, so a script can tell an emptied bucket from an untouched
// one.
func TestDeleteDeclinedLeavesEveryObjectWhereItWas(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStatePurged)
	doubles.seedCopy(t, copyID(2), dataset.CopyStateFailed)
	doubles.objects.ListObjectsV2Fn = bucketHolding(stagedObjects(3), 2)

	out, err := deleteTheObjects(t, doubles, "y\n")
	if err == nil {
		t.Fatal("an abandoned deletion must exit non-zero so a script can tell it apart from one that ran")
	}

	var declined *deleteDeclinedError
	if !errors.As(err, &declined) {
		t.Fatalf("expected the abandoned deletion to be reported as such, got %v", err)
	}

	for _, want := range []string{copyID(1), copyID(2), releaseBucket, "4 object(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in what the operator was shown, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, fmt.Sprintf("%d byte(s)", 4*objectSize)) {
		t.Errorf("expected the weight of what would go to be quoted, got:\n%s", out)
	}
	if !strings.Contains(out, "no window") {
		t.Errorf("expected the operator to be told there is no way back, got:\n%s", out)
	}

	if len(doubles.objects.DeleteObjectsCalls) != 0 {
		t.Error("an abandoned deletion must delete no object")
	}
	for _, id := range []string{copyID(1), copyID(2)} {
		if got := doubles.attr(t, id, "state"); got == string(dataset.CopyStateDeleted) {
			t.Errorf("copy %s was recorded as deleted after the confirmation was abandoned", id)
		}
	}
}

// TestDeleteEmptiesTheBucketAndRecordsEveryCopy covers a deletion carried through, from
// both states it applies from at once: a copy that was purged, and one whose transfer
// never finished.
//
// Everything in the bucket goes, the copy's own control artefacts included -- they
// describe a transfer whose objects are being deleted and nothing reads them afterwards.
// Both copies are then recorded, because the bucket they share is what was emptied. What
// must not happen is anything else: no key is scheduled, since access ended at the purge,
// and no policy is written.
func TestDeleteEmptiesTheBucketAndRecordsEveryCopy(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStatePurged)
	doubles.seedCopy(t, copyID(2), dataset.CopyStateFailed)

	staged := stagedObjects(5)
	doubles.objects.ListObjectsV2Fn = bucketHolding(staged, 2)

	out, err := deleteTheObjects(t, doubles, releaseDataset+"\n")
	if err != nil {
		t.Fatalf("deleting: %v\n%s", err, out)
	}

	deleted := deletedObjectKeys(doubles)
	if !slices.Equal(deleted, staged) {
		t.Errorf("deleted %v, want every object in the bucket %v", deleted, staged)
	}
	if !slices.ContainsFunc(deleted, func(key string) bool {
		return strings.HasPrefix(key, dataset.ControlPrefix+"/")
	}) {
		t.Error("the copy's own control artefacts were left behind; they go with the objects they describe")
	}
	for _, call := range doubles.objects.DeleteObjectsCalls {
		if got := aws.ToString(call.Bucket); got != releaseBucket {
			t.Errorf("objects were deleted from %q, want the bucket the copies staged into (%s)", got, releaseBucket)
		}
	}

	for _, id := range []string{copyID(1), copyID(2)} {
		if got := doubles.attr(t, id, "state"); got != string(dataset.CopyStateDeleted) {
			t.Errorf("copy %s state = %q, want deleted", id, got)
		}
		if doubles.attr(t, id, "deletedAt") == "" {
			t.Errorf("copy %s recorded no time its objects were deleted", id)
		}
	}
	// The purge that preceded it is part of the record of what happened and is not
	// rewritten by the objects going.
	if doubles.attr(t, copyID(1), "keyDeletionDate") == "" {
		t.Error("the deletion lost the date the purge recorded against the copy")
	}

	if len(doubles.keys.ScheduleDeletionCalls) != 0 || len(doubles.objects.PutBucketPolicyCalls) != 0 {
		t.Error("deleting objects schedules no key and writes no policy: access ended at the purge")
	}
	if !strings.Contains(out, "The bucket is still deployed") {
		t.Errorf("expected the report to say the bucket itself stays, got:\n%s", out)
	}
}

// TestDeleteIsRefusedWhileAnyCopyCouldStillBeRead is the refusal the command exists around.
//
// The objects sit in one bucket every copy of the dataset shares, so a single copy that
// has not been purged is enough to refuse the whole dataset: emptying the bucket would
// take the data out from under whatever is reading through that copy. The refusal names
// the copy and the state it is in, since putting the dataset in order means purging that
// copy first.
func TestDeleteIsRefusedWhileAnyCopyCouldStillBeRead(t *testing.T) {
	for _, blocking := range []dataset.CopyState{dataset.CopyStateStaged, dataset.CopyStateCopying} {
		t.Run(string(blocking), func(t *testing.T) {
			doubles := newReleaseDoubles()
			doubles.seedCopy(t, copyID(1), dataset.CopyStatePurged)
			doubles.seedCopy(t, copyID(2), blocking)

			// Counted here rather than recorded on the double, because the point is that
			// a refusal this certain is reached without reading the bucket at all.
			listed := 0
			holding := bucketHolding(stagedObjects(3), 2)
			doubles.objects.ListObjectsV2Fn = func(ctx context.Context, params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
				listed++
				return holding(ctx, params)
			}

			_, err := deleteTheObjects(t, doubles, releaseDataset+"\n")
			if err == nil {
				t.Fatalf("deleting the objects of a dataset with a %s copy must be refused", blocking)
			}

			var refused *copiesNotDeletableError
			if !errors.As(err, &refused) {
				t.Fatalf("expected the refusal to name the copies standing in the way, got %v", err)
			}
			for _, want := range []string{copyID(2), string(blocking)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("expected the refusal to report %q, got %v", want, err)
				}
			}
			if strings.Contains(err.Error(), copyID(1)) {
				t.Errorf("the purged copy is not what blocks the deletion, got %v", err)
			}

			if len(doubles.objects.DeleteObjectsCalls) != 0 {
				t.Error("a refused deletion must delete no object")
			}
			if listed != 0 {
				t.Error("the bucket was listed before the refusal; nothing needed reading to reach it")
			}
			if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStatePurged) {
				t.Errorf("the purged copy moved to %q; a refusal changes nothing", got)
			}
		})
	}
}

// TestDeleteIsRefusedWhileAGrantIsStillDeployed checks the defensive refusal. A purge
// withdraws every grant of the dataset before it schedules the key, so a copy that is
// purged and still carries a deployed grant means a purge did not finish -- which is
// exactly when deleting the objects underneath it would be worst.
func TestDeleteIsRefusedWhileAGrantIsStillDeployed(t *testing.T) {
	doubles := newReleaseDoubles()

	// Granted first and purged afterwards, because that is the only order the ledger
	// allows and the only way a deployed grant outlives a purge.
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	grant := doubles.seedGrant(t, staged, grantSeed{})
	if _, err := doubles.copyStore().MarkPurged(
		context.Background(), staged.CopyID, releaseClock.Add(30*24*time.Hour)); err != nil {
		t.Fatalf("purging the copy: %v", err)
	}
	doubles.objects.ListObjectsV2Fn = bucketHolding(stagedObjects(3), 2)

	_, err := deleteTheObjects(t, doubles, releaseDataset+"\n")
	if err == nil {
		t.Fatal("deleting objects a deployed grant still reaches must be refused")
	}

	var deployed *accessStillDeployedError
	if !errors.As(err, &deployed) {
		t.Fatalf("expected the refusal to name the grants still deployed, got %v", err)
	}
	for _, want := range []string{grant.GrantID, releaseResearcher} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the refusal to report %q, got %v", want, err)
		}
	}
	if len(doubles.objects.DeleteObjectsCalls) != 0 {
		t.Error("a refused deletion must delete no object")
	}
	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStatePurged) {
		t.Errorf("the copy moved to %q; a refusal changes nothing", got)
	}
}

// TestDeleteOfADatasetAlreadyEmptiedDoesNothing checks a repeated invocation. The copies
// already say the objects are gone and the bucket is empty, so there is nothing to delete
// and nothing to record, and the confirmation is never even asked for.
func TestDeleteOfADatasetAlreadyEmptiedDoesNothing(t *testing.T) {
	doubles := newReleaseDoubles()
	deleted := copyFixture(copyID(1), dataset.CopyStateDeleted)
	doubles.seedCopies(t, deleted)

	out, err := deleteTheObjects(t, doubles, "")
	if err != nil {
		t.Fatalf("a repeated deletion should report and do nothing: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already had its objects deleted") {
		t.Errorf("expected the report to say the objects are already gone, got:\n%s", out)
	}
	if len(doubles.objects.DeleteObjectsCalls) != 0 {
		t.Error("a repeated deletion must delete nothing")
	}
}

// TestDeleteBatchesObjectsAtTheApiLimit checks the batching, which is not an optimisation:
// a request naming more than a thousand keys is refused whole, and a real dataset is
// thousands of objects.
//
// Every object still has to go exactly once, so what is asserted is both the shape of the
// requests and that nothing fell between them.
func TestDeleteBatchesObjectsAtTheApiLimit(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStatePurged)

	// More than two full batches, so the last is a partial one and an off-by-one in
	// either direction shows up.
	staged := stagedObjects(2*deleteBatchSize + 249)
	doubles.objects.ListObjectsV2Fn = bucketHolding(staged, 700)

	if _, err := deleteTheObjects(t, doubles, releaseDataset+"\n"); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	calls := doubles.objects.DeleteObjectsCalls
	if len(calls) != 3 {
		t.Fatalf("expected %d objects to be deleted in 3 batches, got %d", len(staged), len(calls))
	}
	for i, call := range calls {
		if call.Delete == nil {
			t.Fatalf("batch %d carried no objects to delete", i)
		}
		if got := len(call.Delete.Objects); got > deleteBatchSize {
			t.Errorf("batch %d named %d keys; the API refuses a request naming more than %d",
				i, got, deleteBatchSize)
		}
	}
	if got := deletedObjectKeys(doubles); !slices.Equal(got, staged) {
		t.Errorf("the batches deleted %d keys, want the %d the listing gave", len(got), len(staged))
	}
	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStateDeleted) {
		t.Errorf("copy state = %q, want deleted", got)
	}
}

// TestDeleteReportsTheObjectsTheServiceRefused checks the failure that is easiest to miss.
// A batched delete that refuses half its keys still returns successfully and reports the
// refusals in its body, so a caller checking only the error would record a deletion of
// objects that are still there.
//
// Nothing may be recorded when that happens: the copies stay purged, which is what they
// are, and re-running finishes the job because deleting an object that is not there
// succeeds.
func TestDeleteReportsTheObjectsTheServiceRefused(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStatePurged)

	staged := stagedObjects(3)
	doubles.objects.ListObjectsV2Fn = bucketHolding(staged, 4)
	doubles.objects.DeleteObjectsFn = func(_ context.Context, params *s3.DeleteObjectsInput) (*s3.DeleteObjectsOutput, error) {
		return &s3.DeleteObjectsOutput{Errors: []s3types.Error{{
			Key:     params.Delete.Objects[0].Key,
			Code:    aws.String("AccessDenied"),
			Message: aws.String("the object is under a legal hold"),
		}}}, nil
	}

	_, err := deleteTheObjects(t, doubles, releaseDataset+"\n")
	if err == nil {
		t.Fatal("a delete the service partly refused must be reported as a failure")
	}

	var refused *objectsNotDeletedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected the failure to name the objects that are still there, got %v", err)
	}
	for _, want := range []string{staged[0], "AccessDenied", "legal hold"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the failure to report %q, got %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "again") {
		t.Errorf("expected the failure to say re-running finishes it, got %v", err)
	}

	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStatePurged) {
		t.Errorf("copy state = %q; nothing may be recorded as deleted while objects are still there", got)
	}
}

// TestDeleteOfADatasetNeverCopiedHereIsRefused checks that a dataset with nothing here is
// refused rather than reported as emptied, and that the refusal points at the likeliest
// cause: this is keyed on the dataset, so a URI spelled differently names a different one.
func TestDeleteOfADatasetNeverCopiedHereIsRefused(t *testing.T) {
	doubles := newReleaseDoubles()

	_, err := deleteTheObjects(t, doubles, "")
	if err == nil {
		t.Fatal("deleting the objects of a dataset never copied here must be refused")
	}

	var nothing *nothingToDeleteError
	if !errors.As(err, &nothing) {
		t.Fatalf("expected the refusal to say there is nothing here to delete, got %v", err)
	}
	if len(doubles.objects.DeleteObjectsCalls) != 0 {
		t.Error("nothing may be deleted for a dataset with no copy here")
	}
}

// restoreTheDataset runs one restore against the fixture dataset.
func restoreTheDataset(t *testing.T, doubles *releaseDoubles) (string, error) {
	t.Helper()

	restorer := newRestorer(releaseConfig(), doubles.clients())
	restorer.now = func() time.Time { return releaseClock }

	cmd, out, _ := testCommand("")
	return outputOf(out, restorer.restore(context.Background(), cmd,
		restoreRequest{datasetURI: releaseDataset}))
}

// TestRestoreCancelsTheDeletionAndEnablesTheKey covers a purge taken back, and above all
// the half of it that is easy to leave out.
//
// Cancelling a scheduled deletion returns a key to disabled rather than to enabled, and a
// disabled key decrypts nothing: a run that cancelled and stopped there would present to a
// researcher exactly as the purge did, while every record said the dataset was back. So
// both calls have to be made, and the copy is only staged again once the key reports
// itself usable.
//
// What must not come back is anybody's access. The purge withdrew every grant before it
// scheduled the key -- which is the whole reason cancelling is safe -- and those
// withdrawals stand.
func TestRestoreCancelsTheDeletionAndEnablesTheKey(t *testing.T) {
	doubles := newReleaseDoubles()
	purged, grants := doubles.seedPurgedCopy(t, copyID(1),
		grantSeed{revoked: true},
		grantSeed{account: secondResearcher, vpc: secondVpc, revoked: true},
	)
	doubles.keys.DescribeKeyFn = keyRecoveringFromDeletion(doubles.keys)

	out, err := restoreTheDataset(t, doubles)
	if err != nil {
		t.Fatalf("restoring: %v\n%s", err, out)
	}

	if len(doubles.keys.CancelDeletionCalls) != 1 {
		t.Fatalf("expected the scheduled deletion to be cancelled once, got %d call(s)",
			len(doubles.keys.CancelDeletionCalls))
	}
	if len(doubles.keys.EnableKeyCalls) != 1 {
		t.Fatalf("a cancelled deletion leaves the key disabled, so it has to be enabled too, "+
			"got %d call(s)", len(doubles.keys.EnableKeyCalls))
	}
	for _, recovered := range []string{
		aws.ToString(doubles.keys.CancelDeletionCalls[0].KeyId),
		aws.ToString(doubles.keys.EnableKeyCalls[0].KeyId),
	} {
		// The key the copy records name, rather than one derived from the dataset URI, so
		// what comes back is what the objects were actually encrypted under.
		if recovered != releaseKeyArn {
			t.Errorf("recovered key %q, want the one the copy records name (%s)", recovered, releaseKeyArn)
		}
	}
	if !strings.Contains(out, "Key enabled") {
		t.Errorf("expected the report to say the key was enabled and read back, got:\n%s", out)
	}

	if got := doubles.attr(t, purged.CopyID, "state"); got != string(dataset.CopyStateStaged) {
		t.Errorf("copy state = %q, want staged", got)
	}
	// A record saying both that the copy can be granted and when its destruction becomes
	// irreversible says two things at once, and a listing would have to pick one.
	stored := doubles.item(t, purged.CopyID)
	for _, attribute := range []string{"purgedAt", "keyDeletionDate"} {
		if _, present := stored[attribute]; present {
			t.Errorf("the restored copy still carries %q, so it reads as grantable and as counting "+
				"down to its own destruction at once", attribute)
		}
	}

	for _, grant := range grants {
		if got := doubles.attr(t, grant.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
			t.Errorf("grant %s is %q; the purge withdrew it and cancelling a key deletion gives "+
				"nobody their reads back", grant.GrantID, got)
		}
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
		t.Error("no grant changed, so neither document has anything new to say")
	}
	if written := manifestWrites(doubles); len(written) != 0 {
		t.Errorf("a researcher's description of their access was republished (%v), but their "+
			"access ended at the purge and has not come back", written)
	}
	if !strings.Contains(out, "Nobody has access yet") {
		t.Errorf("expected the report to say restoring grants nobody anything, got:\n%s", out)
	}
}

// TestRestoreEnablesAKeyAlreadyTakenOffItsDeletion covers the half-finished job: somebody
// cancelled the deletion by hand, or a previous run stopped between its two calls.
//
// There is nothing left to cancel, and the key is still disabled -- which is the state
// cancelling actually leaves behind, and one a researcher cannot tell from a purge. So the
// enable is made anyway, and only then is the copy staged again.
func TestRestoreEnablesAKeyAlreadyTakenOffItsDeletion(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedPurgedCopy(t, copyID(1), grantSeed{revoked: true})
	doubles.keys.DescribeKeyFn = func(ctx context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
		if len(doubles.keys.EnableKeyCalls) > 0 {
			return keyInState(kmstypes.KeyStateEnabled)(ctx, params)
		}
		return keyInState(kmstypes.KeyStateDisabled)(ctx, params)
	}

	out, err := restoreTheDataset(t, doubles)
	if err != nil {
		t.Fatalf("restoring a dataset whose key was already taken off its deletion: %v\n%s", err, out)
	}

	if len(doubles.keys.CancelDeletionCalls) != 0 {
		t.Error("a key with no deletion pending must not have one cancelled")
	}
	if len(doubles.keys.EnableKeyCalls) != 1 {
		t.Errorf("expected the disabled key to be enabled once, got %d call(s)",
			len(doubles.keys.EnableKeyCalls))
	}
	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStateStaged) {
		t.Errorf("copy state = %q, want staged", got)
	}
}

// TestRestoreFailsWhenTheKeyDoesNotComeBackEnabled is the case the command exists for.
//
// The deletion was cancelled and the key enabled, and the key still reports itself
// disabled -- a key store or imported material can leave it that way for reasons nothing
// here can put right. A disabled key denies every read exactly as a purged one does, so
// recording the copies as staged would leave a restore that looks finished, reports itself
// grantable, and works for nobody. The run fails instead, and says what the state means.
func TestRestoreFailsWhenTheKeyDoesNotComeBackEnabled(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedPurgedCopy(t, copyID(1), grantSeed{revoked: true})

	// Pending deletion until it is cancelled and disabled from then on: the enable was
	// made and did not take.
	doubles.keys.DescribeKeyFn = func(ctx context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
		if len(doubles.keys.CancelDeletionCalls) > 0 {
			return keyInState(kmstypes.KeyStateDisabled)(ctx, params)
		}
		return keyInState(kmstypes.KeyStatePendingDeletion)(ctx, params)
	}

	_, err := restoreTheDataset(t, doubles)
	if err == nil {
		t.Fatal("a key that does not report itself enabled must fail the run: it decrypts nothing")
	}

	var notEnabled *keyNotEnabledError
	if !errors.As(err, &notEnabled) {
		t.Fatalf("expected the failure to name the key that did not come back, got %v", err)
	}
	for _, want := range []string{
		releaseKeyArn,
		string(kmstypes.KeyStateDisabled),
		string(kmstypes.KeyStateEnabled),
		"decrypts nothing",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the failure to report %q, got %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "again") {
		t.Errorf("expected the failure to say a second run finishes it, got %v", err)
	}

	if len(doubles.keys.EnableKeyCalls) != 1 {
		t.Errorf("the key has to have been enabled before its state is held against it, got %d call(s)",
			len(doubles.keys.EnableKeyCalls))
	}
	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStatePurged) {
		t.Errorf("copy state = %q; nothing may read as grantable against a key that decrypts nothing", got)
	}
}

// TestRestoreIsRefusedWhereThereIsNothingSafeToTakeBack covers the refusals, and that each
// leaves the ledger and the key exactly as the purge left them.
//
// They are separate refusals because they leave the operator with different things to do.
// Objects that have been deleted and a key whose window elapsed have no way back at all and
// the dataset has to be copied in again; a dataset with no purged copy is the ordinary
// mistake and needs nothing done to it; a grant still deployed means a purge did not finish
// and has to be finished before the key can safely decrypt again.
func TestRestoreIsRefusedWhereThereIsNothingSafeToTakeBack(t *testing.T) {
	tests := map[string]struct {
		// seed puts the ledger and the key into the shape the refusal is about, and
		// returns whatever the refusal has to name back.
		seed func(t *testing.T, doubles *releaseDoubles) []string
		// unchanged is the state the first copy has to still be in afterwards, and is
		// empty where the dataset has no copy in this account at all.
		unchanged dataset.CopyState
		refused   func(err error) bool
		reports   []string
	}{
		"its objects have been deleted": {
			seed: func(t *testing.T, doubles *releaseDoubles) []string {
				doubles.seedCopy(t, copyID(1), dataset.CopyStateDeleted)
				doubles.seedCopy(t, copyID(2), dataset.CopyStatePurged)
				return []string{copyID(1)}
			},
			unchanged: dataset.CopyStateDeleted,
			refused:   func(err error) bool { var e *copiesDeletedError; return errors.As(err, &e) },
			// A key that decrypts is worth nothing against ciphertext that is not there,
			// and saying so is more use than saying a copy of it is purged.
			reports: []string{"not there", "Nothing was cancelled"},
		},
		"no copy of it is purged": {
			seed: func(t *testing.T, doubles *releaseDoubles) []string {
				doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
				return []string{copyID(1), string(dataset.CopyStateStaged)}
			},
			unchanged: dataset.CopyStateStaged,
			refused:   func(err error) bool { var e *nothingPurgedError; return errors.As(err, &e) },
			reports:   []string{"nothing here to take back", "Nothing was cancelled"},
		},
		"it was never copied here": {
			seed:    func(*testing.T, *releaseDoubles) []string { return nil },
			refused: func(err error) bool { var e *nothingToRestoreError; return errors.As(err, &e) },
			reports: []string{releaseDataset, "never been copied", "no deletion was cancelled"},
		},
		"a grant of it is still deployed": {
			seed: func(t *testing.T, doubles *releaseDoubles) []string {
				_, grants := doubles.seedPurgedCopy(t, copyID(1), grantSeed{})
				return []string{grants[0].GrantID, releaseResearcher}
			},
			unchanged: dataset.CopyStatePurged,
			refused:   func(err error) bool { var e *grantsWouldReviveError; return errors.As(err, &e) },
			// The one thing a restore must never do quietly: a key that decrypts again
			// would hand that account its reads back without anybody granting them.
			reports: []string{"without anybody granting them", "Nothing was cancelled"},
		},
		"its key has no deletion pending": {
			seed: func(t *testing.T, doubles *releaseDoubles) []string {
				doubles.seedPurgedCopy(t, copyID(1), grantSeed{revoked: true})
				doubles.keys.DescribeKeyFn = keyInState(kmstypes.KeyStateUnavailable)
				return []string{releaseKeyArn, string(kmstypes.KeyStateUnavailable)}
			},
			unchanged: dataset.CopyStatePurged,
			refused:   func(err error) bool { var e *keysNotRestorableError; return errors.As(err, &e) },
			reports:   []string{"no deletion of it is pending", "there is none to cancel"},
		},
		"its key is already destroyed": {
			seed: func(t *testing.T, doubles *releaseDoubles) []string {
				doubles.seedPurgedCopy(t, copyID(1), grantSeed{revoked: true})
				doubles.keys.DescribeKeyFn = keyDestroyed()
				return []string{releaseKeyArn}
			},
			unchanged: dataset.CopyStatePurged,
			refused:   func(err error) bool { var e *keysNotRestorableError; return errors.As(err, &e) },
			// The end of the story rather than a state to wait out: the objects in the
			// bucket are ciphertext nobody will read again.
			reports: []string{"no longer exists", "pending window has elapsed", "stage the dataset"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			doubles := newReleaseDoubles()
			doubles.keys.DescribeKeyFn = keyRecoveringFromDeletion(doubles.keys)
			named := tt.seed(t, doubles)

			_, err := restoreTheDataset(t, doubles)
			if err == nil {
				t.Fatalf("restoring a dataset where %s must be refused", name)
			}
			if !tt.refused(err) {
				t.Fatalf("the refusal does not say what actually stands in the way: %v", err)
			}
			for _, want := range slices.Concat(named, tt.reports) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("expected the refusal to report %q, got %v", want, err)
				}
			}

			if len(doubles.keys.CancelDeletionCalls) != 0 || len(doubles.keys.EnableKeyCalls) != 0 {
				t.Error("a refused restore must leave the key exactly as the purge left it")
			}
			if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
				t.Error("a refused restore must write no document")
			}
			if tt.unchanged == "" {
				if len(doubles.ledger.Items) != 0 {
					t.Errorf("a dataset never copied here gained %d record(s)", len(doubles.ledger.Items))
				}
				return
			}
			if got := doubles.attr(t, copyID(1), "state"); got != string(tt.unchanged) {
				t.Errorf("copy state = %q, want %q; a refusal changes nothing", got, tt.unchanged)
			}
		})
	}
}

// TestARestoredCopyCanBeGrantedAgain is what the whole command is for.
//
// The objects never left the bucket, so giving a researcher access again costs the policy
// write and nothing else -- no second billed transfer of a dataset that was there the whole
// time. It is a fresh grant rather than the old one waking up: the withdrawal the purge made
// is part of the account of what happened and keeps saying when that access ended.
func TestARestoredCopyCanBeGrantedAgain(t *testing.T) {
	ctx := context.Background()
	doubles := newReleaseDoubles()
	_, grants := doubles.seedPurgedCopy(t, copyID(1), grantSeed{revoked: true})
	withdrawn := grants[0]
	doubles.keys.DescribeKeyFn = keyRecoveringFromDeletion(doubles.keys)

	if out, err := restoreTheDataset(t, doubles); err != nil {
		t.Fatalf("restoring: %v\n%s", err, out)
	}

	out, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err != nil {
		t.Fatalf("granting a restored copy: %v\n%s", err, out)
	}

	restored, err := doubles.copyStore().Get(ctx, copyID(1))
	if err != nil {
		t.Fatalf("reading the restored copy back: %v", err)
	}
	standing, err := doubles.grantStore().GrantsOfCopy(ctx, restored)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(standing) != 2 {
		t.Fatalf("expected the new grant alongside the withdrawn one, got %d", len(standing))
	}

	var deployed []*dataset.Grant
	for _, grant := range standing {
		if grant.GrantID != withdrawn.GrantID {
			deployed = append(deployed, grant)
		}
	}
	if len(deployed) != 1 || deployed[0].State != dataset.GrantStateGranted {
		t.Fatalf("expected one new grant giving access against the restored copy, got %+v", deployed)
	}
	if deployed[0].CopyID != restored.CopyID {
		t.Errorf("the new grant reads copy %q rather than the one that was restored (%s)",
			deployed[0].CopyID, restored.CopyID)
	}

	if got := doubles.attr(t, withdrawn.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
		t.Errorf("the withdrawn grant is now %q; taking a purge back undoes no withdrawal", got)
	}
	if got := doubles.attr(t, withdrawn.GrantID, "revokedAt"); got != withdrawn.RevokedAt.Format(time.RFC3339) {
		t.Errorf("the withdrawn grant's timestamp moved to %q; it records when that access ended", got)
	}

	// The access is real, and it is the new grant's statements that carry it.
	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	if !strings.Contains(written[0], releaseResearcher) {
		t.Errorf("the rewritten bucket policy does not name the researcher:\n%s", written[0])
	}
	if strings.Contains(written[0], withdrawn.GrantID) {
		t.Errorf("the withdrawn grant's statements were written back:\n%s", written[0])
	}
}

// outputOf pairs a captured stream with the error the command returned, so a test can
// report both when an assertion fails.
func outputOf(out interface{ String() string }, err error) (string, error) {
	return out.String(), err
}
