package cmd

// These tests cover the two commands that put a dataset within reach of a researcher: the
// one that copies it into this account, and the one that grants access to that copy.
//
// What is worth testing about them is almost entirely their refusals, and which refusals
// remain. Both commands act on things that are expensive or irreversible to get wrong -- a
// transfer runs for hours and is billed for it, and a grant is real the moment the
// documents are written -- so each of them establishes everything it can before the first
// mutation, and the tests below check that a refusal really did leave the world alone
// rather than merely reporting that it had.
//
// Granting has one refusal fewer than it used to. Several researchers may read one copy at
// once, so a dataset someone already holds is granted again without being copied; what is
// still refused is the same account reading the same copy from the same VPC twice.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/umccr/data-releaser/internal/dataset"
)

// newTestStager assembles the staging command against the doubles. The bucket a dataset
// lands in is pinned in the fixture configuration, so a test asserts against a name rather
// than against a derivation it would have to reproduce.
func newTestStager(t *testing.T, doubles *releaseDoubles) *stager {
	t.Helper()

	return newStager(releaseConfig(), stageClients{
		ledger:  doubles.ledger,
		objects: doubles.objects,
		keys:    doubles.keys,
		copies:  doubles.copies,
	})
}

// stageTheDataset runs one staging invocation against the fixture dataset.
func stageTheDataset(t *testing.T, doubles *releaseDoubles) (string, error) {
	t.Helper()

	cmd, out, _ := testCommand("")
	err := newTestStager(t, doubles).stage(context.Background(), cmd.OutOrStdout(), stageRequest{
		datasetURI:     releaseDataset,
		hotCacheBucket: releaseBucket,
	})
	return out.String(), err
}

// TestStagingADatasetAlreadyAccountedForStartsNoCopy checks that a dataset whose objects
// are already here, or on their way, is reported rather than copied a second time. The
// transfer is the expensive half of this mechanism and it is billed by the byte, so
// starting a redundant one is the mistake worth making impossible.
func TestStagingADatasetAlreadyAccountedForStartsNoCopy(t *testing.T) {
	for _, state := range []dataset.CopyState{
		dataset.CopyStateCopying,
		dataset.CopyStateStaged,
	} {
		t.Run(string(state), func(t *testing.T) {
			doubles := newReleaseDoubles()
			doubles.seedCopy(t, copyID(1), state)

			_, err := stageTheDataset(t, doubles)
			if err == nil {
				t.Fatal("staging a dataset that is already accounted for must be refused")
			}

			var already *alreadyStagedError
			if !errors.As(err, &already) {
				t.Fatalf("expected the refusal to name the copy standing in the way, got %v", err)
			}
			if !strings.Contains(err.Error(), copyID(1)) {
				t.Errorf("expected the refusal to name copy %s, got %v", copyID(1), err)
			}
			if len(doubles.copies.StartCalls) != 0 {
				t.Errorf("expected no copy to be started, got %d", len(doubles.copies.StartCalls))
			}
			if len(doubles.ledger.Items) != 1 {
				t.Errorf("expected the ledger to be untouched, got %d records", len(doubles.ledger.Items))
			}
		})
	}
}

// TestPricingADatasetAlreadyStagedIsAllowed is the exception to that rule.
//
// What a covering copy saves is the hours and the money a second transfer would spend, and
// a priced run spends neither: it moves no bytes, and the copy service records nothing
// under the name it is given, so the copy it would have been refused on account of is left
// exactly as it was. Refusing it would mean a staged dataset could never be re-priced, and
// never checked to still reach the copy service as intended -- which is precisely when an
// operator wants to ask.
func TestPricingADatasetAlreadyStagedIsAllowed(t *testing.T) {
	for _, state := range []dataset.CopyState{
		dataset.CopyStateCopying,
		dataset.CopyStateStaged,
	} {
		t.Run(string(state), func(t *testing.T) {
			doubles := newReleaseDoubles()
			doubles.seedCopy(t, copyID(1), state)

			cmd, _, _ := testCommand("")
			err := newTestStager(t, doubles).stage(context.Background(), cmd.OutOrStdout(), stageRequest{
				datasetURI:     releaseDataset,
				hotCacheBucket: releaseBucket,
				dryRun:         true,
			})
			if err != nil {
				t.Fatalf("pricing a staged dataset must be allowed: %v", err)
			}
			if len(doubles.copies.StartCalls) != 1 {
				t.Fatalf("expected the priced run to be started, got %d executions", len(doubles.copies.StartCalls))
			}
			// The ledger still holds only the copy that was seeded: pricing records nothing.
			if len(doubles.ledger.Items) != 1 {
				t.Errorf("expected the priced run to record nothing, got %d records", len(doubles.ledger.Items))
			}
		})
	}
}

// TestStagingIsNotBlockedByAGrantOnTheCoveringCopy is the other half of the rule: what
// stands in the way is the copy, and only the copy.
//
// A staged copy blocks a second transfer whether it has been granted to nobody, to one
// researcher, or to two, because the bytes are there either way. The grants are what an
// operator is most likely to be thinking about, so it is worth showing that they make no
// difference at all to this decision.
func TestStagingIsNotBlockedByAGrantOnTheCoveringCopy(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.seedGrants(t, staged,
		grantSeed{},
		grantSeed{account: secondResearcher, vpc: secondVpc},
	)

	_, err := stageTheDataset(t, doubles)

	var already *alreadyStagedError
	if !errors.As(err, &already) {
		t.Fatalf("expected the staged copy to stand in the way, got %v", err)
	}
	if !strings.Contains(err.Error(), copyID(1)) {
		t.Errorf("expected the refusal to name the copy rather than a grant, got %v", err)
	}
	if len(doubles.copies.StartCalls) != 0 {
		t.Error("expected no copy to be started")
	}
}

// TestStagingProceedsWhereNothingCoversTheDataset is the contrast: a transfer that
// delivered nothing, or a copy whose key is being destroyed, leaves the dataset needing a
// copy, so one is started. Without this the refusal above could be passing for having
// refused everything.
func TestStagingProceedsWhereNothingCoversTheDataset(t *testing.T) {
	for _, state := range []dataset.CopyState{
		dataset.CopyStateFailed,
		dataset.CopyStatePurged,
	} {
		t.Run(string(state), func(t *testing.T) {
			doubles := newReleaseDoubles()
			doubles.seedCopy(t, copyID(1), state)

			if _, err := stageTheDataset(t, doubles); err != nil {
				t.Fatalf("staging: %v", err)
			}
			if len(doubles.copies.StartCalls) != 1 {
				t.Fatalf("expected one copy to be started, got %d", len(doubles.copies.StartCalls))
			}
			if len(doubles.ledger.Items) != 2 {
				t.Errorf("expected the new copy to be recorded alongside the old, got %d records",
					len(doubles.ledger.Items))
			}
		})
	}
}

// TestStagingRecordsWhatItStartedAsACopy checks the record staging leaves behind, since
// nothing else can: the transfer reports its own outcome hours later against exactly this
// record, found by exactly this identifier.
func TestStagingRecordsWhatItStartedAsACopy(t *testing.T) {
	doubles := newReleaseDoubles()

	out, err := stageTheDataset(t, doubles)
	if err != nil {
		t.Fatalf("staging: %v\n%s", err, out)
	}
	if len(doubles.ledger.Items) != 1 {
		t.Fatalf("expected exactly one record, got %d", len(doubles.ledger.Items))
	}

	var recorded string
	for id := range doubles.ledger.Items {
		recorded = id
	}
	if got := doubles.attr(t, recorded, "kind"); got != "copy" {
		t.Errorf("staging recorded a %q; it stages a copy and grants nobody anything", got)
	}
	if got := doubles.attr(t, recorded, "state"); got != string(dataset.CopyStateCopying) {
		t.Errorf("recorded state = %q, want copying", got)
	}
	if got := doubles.attr(t, recorded, "researcherAccountId"); got != "" {
		t.Errorf("a copy named researcher %q; who may read is a grant's business", got)
	}
	if got := doubles.attr(t, recorded, "datasetUri"); got != releaseDataset {
		t.Errorf("recorded dataset = %q, want %s", got, releaseDataset)
	}

	// The identifier the transfer runs under is the one the record carries, or the
	// completion callback would have nothing to move.
	if len(doubles.copies.StartCalls) != 1 {
		t.Fatalf("expected one copy to be started, got %d", len(doubles.copies.StartCalls))
	}
	if !strings.Contains(out, recorded) {
		t.Errorf("expected the report to name the copy it recorded (%s), got:\n%s", recorded, out)
	}
}

// TestStagingReportsTheBucketAndKeyItCouldNotFind checks that a dataset whose resources
// have not been deployed is reported against the deployment that owns them, rather than as
// whichever AWS call happened to notice first. Both are reported together, because an
// operator who has not deployed a dataset's resources is missing both.
func TestStagingReportsTheBucketAndKeyItCouldNotFind(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.objects.HeadBucketFn = func(context.Context, *s3.HeadBucketInput) (*s3.HeadBucketOutput, error) {
		return nil, errNotFound("bucket")
	}
	doubles.keys.DescribeKeyFn = func(context.Context, *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
		return nil, errNotFound("alias")
	}

	_, err := stageTheDataset(t, doubles)
	if err == nil {
		t.Fatal("staging into a bucket that is not there must be refused")
	}
	for _, want := range []string{releaseBucket, releaseAccount} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the refusal to name %q, got %v", want, err)
		}
	}
	if len(doubles.copies.StartCalls) != 0 {
		t.Error("no copy may be started before the bucket and key are known to be there")
	}
}

// TestStagingRefusesAKeyThatCannotServeDataKeys checks the key's state is established
// before the copy starts. The copy asks for a data key per object, so a key that cannot
// serve one fails the transfer partway through -- after it may already have run for hours.
func TestStagingRefusesAKeyThatCannotServeDataKeys(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.keys.DescribeKeyFn = keyInState(kmstypes.KeyStateDisabled)

	_, err := stageTheDataset(t, doubles)
	if err == nil {
		t.Fatal("staging into a bucket whose key is disabled must be refused")
	}
	if !strings.Contains(err.Error(), string(kmstypes.KeyStateDisabled)) {
		t.Errorf("expected the refusal to name the state of the key, got %v", err)
	}
	if len(doubles.copies.StartCalls) != 0 {
		t.Error("no copy may be started against a key that cannot serve data keys")
	}
}

// TestPinnedBucketIsFoundHoweverTheDatasetIsWritten checks the lookup that decides which
// bucket a dataset lands in.
//
// The configured keys are written by hand and the dataset URI arrives from a command line,
// so the two differing by a trailing slash is the likeliest mismatch there is. Missing the
// pin does not fail: it falls through to derivation and names a bucket that was never
// deployed, for a dataset whose bucket is sitting right there.
func TestPinnedBucketIsFoundHoweverTheDatasetIsWritten(t *testing.T) {
	cfg := releaseConfig()

	for _, written := range []string{
		releaseDataset,
		strings.TrimSuffix(releaseDataset, "/"),
		strings.TrimPrefix(releaseDataset, "s3://"),
	} {
		got, err := hotCacheBucketFor(cfg, written)
		if err != nil {
			t.Errorf("resolving the bucket for %q: %v", written, err)
			continue
		}
		if got != releaseBucket {
			t.Errorf("dataset written as %q resolved to bucket %q, want the pinned %q",
				written, got, releaseBucket)
		}
	}

	// A configured key written differently from the dataset URI still pins it, since both
	// sides are reduced before they are compared.
	cfg.S3.HotCache.Buckets = map[string]string{
		strings.TrimSuffix(releaseDataset, "/"): releaseBucket,
	}
	got, err := hotCacheBucketFor(cfg, releaseDataset)
	if err != nil {
		t.Fatalf("resolving the bucket against a differently written pin: %v", err)
	}
	if got != releaseBucket {
		t.Errorf("a pin written without its trailing slash was missed: got %q", got)
	}

	// A dataset that is pinned to nothing still derives a name rather than failing.
	derived, err := hotCacheBucketFor(cfg, "s3://source/some-other-dataset/")
	if err != nil {
		t.Fatalf("deriving a name for an unpinned dataset: %v", err)
	}
	if derived == releaseBucket || derived == "" {
		t.Errorf("an unpinned dataset derived %q", derived)
	}
}

// shareTheDataset runs one grant against the fixture dataset.
func shareTheDataset(t *testing.T, doubles *releaseDoubles, request shareS3Request) (string, string, error) {
	t.Helper()

	cmd, out, errOut := testCommand("")
	err := newTestReleaser(t, doubles).share(context.Background(), cmd, request)
	return out.String(), errOut.String(), err
}

// shareRequest is the fixture invocation, with an expiry the caller chooses.
func shareRequest(expiresAt *time.Time) shareS3Request {
	return shareS3Request{
		datasetURI:  releaseDataset,
		account:     releaseResearcher,
		targetVpcID: releaseVpc,
		expiresAt:   expiresAt,
	}
}

// TestSharingRefusesADatasetWithNoStagedCopy checks that every copy state but the one that
// delivered the dataset is refused, and that the refusal says which state it found.
//
// Refusing is the whole point of the check. Granting a dataset whose transfer is still
// running would hand over whatever fraction of it has staged; granting one whose transfer
// failed would hand over a partial dataset presented as a whole one; and granting one whose
// key is being destroyed would hand over ciphertext nobody can read.
func TestSharingRefusesADatasetWithNoStagedCopy(t *testing.T) {
	for _, state := range []dataset.CopyState{
		dataset.CopyStateCopying,
		dataset.CopyStateFailed,
		dataset.CopyStatePurged,
	} {
		t.Run(string(state), func(t *testing.T) {
			doubles := newReleaseDoubles()
			doubles.seedCopy(t, copyID(1), state)

			_, _, err := shareTheDataset(t, doubles, shareRequest(nil))
			if err == nil {
				t.Fatalf("granting a dataset whose copy is %s must be refused", state)
			}
			if !strings.Contains(err.Error(), string(state)) {
				t.Errorf("expected the refusal to report the state it found (%s), got %v", state, err)
			}
			if !strings.Contains(err.Error(), "No policy was written") {
				t.Errorf("expected the refusal to say nothing was written, got %v", err)
			}
			if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
				t.Error("a refused grant must write neither document")
			}
			if got := doubles.attr(t, copyID(1), "state"); got != string(state) {
				t.Errorf("the record moved to %q; a refusal must leave it exactly as it was", got)
			}
			if len(doubles.ledger.Items) != 1 {
				t.Errorf("a refused grant recorded something: %d records", len(doubles.ledger.Items))
			}
		})
	}
}

// TestSharingRefusesADatasetNeverCopiedHere checks the case with no record at all, which
// reads differently from every state above: there is nothing to report the state of, and
// the answer is to stage the dataset first.
func TestSharingRefusesADatasetNeverCopiedHere(t *testing.T) {
	doubles := newReleaseDoubles()

	_, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err == nil {
		t.Fatal("granting a dataset that was never copied here must be refused")
	}
	if !strings.Contains(err.Error(), "stage it first") {
		t.Errorf("expected the refusal to say what to do instead, got %v", err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 {
		t.Error("a refused grant must write no document")
	}
}

// TestSharingGrantsTheNewestStagedCopy checks which copy a grant is made against where a
// dataset has been transferred more than once.
//
// Every copy of a dataset lands in the same bucket, so the later transfer wrote over the
// earlier one: the newest staged copy is the one whose objects are actually there, and a
// grant against an older record would describe bytes that have since been replaced.
func TestSharingGrantsTheNewestStagedCopy(t *testing.T) {
	doubles := newReleaseDoubles()

	newest := copyFixture(copyID(1), dataset.CopyStateStaged)
	older := copyFixture(copyID(2), dataset.CopyStateStaged)
	older.CreatedAt = newest.CreatedAt.Add(-2 * time.Hour)
	doubles.seedCopies(t, newest, older)

	out, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err != nil {
		t.Fatalf("granting: %v\n%s", err, out)
	}

	grants, err := doubles.grantStore().GrantsOfDataset(context.Background(), releaseDataset)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected one grant, got %d", len(grants))
	}
	if grants[0].CopyID != copyID(1) {
		t.Errorf("the grant reads copy %s, want the newest staged one (%s)", grants[0].CopyID, copyID(1))
	}
}

// TestSharingGrantsACopyASecondResearcherAlreadyHolds is the case this whole separation
// exists for.
//
// One copy carries as many grants as have been agreed. Granting a second researcher takes
// nothing from the first: the documents are rendered from both, so the deployed pair names
// both accounts, and neither record knows anything about the other.
func TestSharingGrantsACopyASecondResearcherAlreadyHolds(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	first := doubles.seedGrant(t, staged, grantSeed{})

	out, _, err := shareTheDataset(t, doubles, shareS3Request{
		datasetURI:  releaseDataset,
		account:     secondResearcher,
		targetVpcID: secondVpc,
	})
	if err != nil {
		t.Fatalf("granting a copy a second researcher already holds: %v\n%s", err, out)
	}

	// Both grants stand against the one copy, and nothing was copied to make the second.
	grants, err := doubles.grantStore().GrantsOfCopy(context.Background(), staged)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected two grants against copy %s, got %d", staged.CopyID, len(grants))
	}
	if len(doubles.copies.StartCalls) != 0 {
		t.Error("granting a copy that is already here must transfer nothing")
	}

	// The deployed documents name both researchers, which is the point: rendering from
	// the new grant alone would have written the first researcher's access out.
	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	for _, want := range []string{releaseResearcher, secondResearcher, releaseVpc, secondVpc} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the deployed bucket policy does not name %q:\n%s", want, written[0])
		}
	}

	// The first researcher's grant is untouched: still deployed, still theirs.
	if got := doubles.attr(t, first.GrantID, "state"); got != string(dataset.GrantStateGranted) {
		t.Errorf("the first researcher's grant is %q; granting another must not move it", got)
	}
	if !strings.Contains(out, "2 grant(s) on this copy") {
		t.Errorf("expected the report to say both grants are deployed, got:\n%s", out)
	}
}

// TestSharingRefusesADuplicateGrant checks the one refusal that remains: the same account
// reading the same copy from the same VPC twice.
//
// That is not a second researcher, it is the grant already in place said again. A second
// record would deploy statements identical in everything but their name and leave two
// records where one describes the access, so it is refused and the operator is pointed at
// the extension instead.
func TestSharingRefusesADuplicateGrant(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	existing := doubles.seedGrant(t, staged, grantSeed{})

	_, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err == nil {
		t.Fatal("granting the same account the same copy from the same VPC must be refused")
	}

	var duplicate *duplicateGrantError
	if !errors.As(err, &duplicate) {
		t.Fatalf("expected the refusal to name the grant already in place, got %v", err)
	}
	if !strings.Contains(err.Error(), existing.GrantID) {
		t.Errorf("expected the refusal to name grant %s, got %v", existing.GrantID, err)
	}
	if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
		t.Error("a refused duplicate must write neither document")
	}

	grants, err := doubles.grantStore().GrantsOfCopy(context.Background(), staged)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 1 {
		t.Errorf("a refused duplicate recorded a second grant: %d against the copy", len(grants))
	}
}

// TestSharingTheSameAccountFromAnotherVpcIsNotADuplicate draws the other side of that
// line. The VPC is half of what a grant says, and reads arriving from a different one are
// denied by the first grant's condition, so a second VPC is genuinely more access and
// genuinely a second grant.
func TestSharingTheSameAccountFromAnotherVpcIsNotADuplicate(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.seedGrant(t, staged, grantSeed{})

	out, _, err := shareTheDataset(t, doubles, shareS3Request{
		datasetURI:  releaseDataset,
		account:     releaseResearcher,
		targetVpcID: secondVpc,
	})
	if err != nil {
		t.Fatalf("granting the same account access from a second VPC: %v\n%s", err, out)
	}

	grants, err := doubles.grantStore().GrantsOfCopy(context.Background(), staged)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected two grants, got %d", len(grants))
	}

	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	for _, want := range []string{releaseVpc, secondVpc} {
		if !strings.Contains(written[0], want) {
			t.Errorf("the deployed bucket policy does not admit reads from %q:\n%s", want, written[0])
		}
	}
}

// TestSharingAfterAWithdrawalMakesANewGrant covers granting a copy whose access was
// withdrawn earlier.
//
// The withdrawn record is not brought back to life. Its withdrawal is part of the account
// of what happened, so it keeps the timestamp saying when the access ended, and restoring
// access writes a second record. Nothing is copied for it: withdrawing took statements out
// of a policy and left the objects and the key exactly where they were.
func TestSharingAfterAWithdrawalMakesANewGrant(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	withdrawn := doubles.seedGrant(t, staged, grantSeed{revoked: true})

	out, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err != nil {
		t.Fatalf("granting a copy whose access was withdrawn: %v\n%s", err, out)
	}

	if got := doubles.attr(t, withdrawn.GrantID, "state"); got != string(dataset.GrantStateRevoked) {
		t.Errorf("the withdrawn grant is now %q; a withdrawal is never undone", got)
	}
	if doubles.attr(t, withdrawn.GrantID, "revokedAt") == "" {
		t.Error("the withdrawn grant lost the timestamp recording when its access ended")
	}

	grants, err := doubles.grantStore().GrantsOfCopy(context.Background(), staged)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected the new grant alongside the withdrawn one, got %d", len(grants))
	}
	if len(doubles.copies.StartCalls) != 0 {
		t.Error("granting a copy that is already here must transfer nothing")
	}

	// The access is real: the document names the researcher again, and it does so through
	// the new grant's statements rather than the withdrawn one's.
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

// TestSharingSelectsWithinOneDatasetOnly checks that "the newest staged copy" means the
// newest copy of the dataset being granted, and not the newest in the ledger.
//
// Every dataset has its own bucket and its own key, so copies of one say nothing about
// another. A second dataset transferred more recently must not reach the choice made here
// -- neither to be granted by mistake, nor to refuse a grant that should have been
// allowed.
func TestSharingSelectsWithinOneDatasetOnly(t *testing.T) {
	const otherDataset = "s3://ceph-source/datasets/cohort-b/"
	const otherBucket = "data-releaser-dataset-cohort-b"

	doubles := newReleaseDoubles()

	// This dataset: staged, and the only copy it has.
	staged := copyFixture(copyID(1), dataset.CopyStateStaged)

	// Another dataset, more recent, and in the two states that would refuse a grant had
	// they belonged to this one.
	for i, state := range []dataset.CopyState{dataset.CopyStateCopying, dataset.CopyStateFailed} {
		other := copyFixture(copyID(i+2), state)
		other.DatasetURI = otherDataset
		other.HotCacheBucket = otherBucket
		other.CreatedAt = staged.CreatedAt.Add(time.Duration(i+1) * time.Hour)
		doubles.seedCopies(t, other)
	}
	doubles.seedCopies(t, staged)

	out, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err != nil {
		t.Fatalf("a newer copy of another dataset refused this one: %v\n%s", err, out)
	}

	grants, err := doubles.grantStore().GrantsOfDataset(context.Background(), releaseDataset)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 1 || grants[0].CopyID != copyID(1) {
		t.Fatalf("the grant was not made against this dataset's copy: %+v", grants)
	}

	// The grant names this dataset's bucket, not the other one's.
	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	if !strings.Contains(written[0], releaseBucket) || strings.Contains(written[0], otherBucket) {
		t.Errorf("the policy was written against the wrong dataset's bucket:\n%s", written[0])
	}
}

// TestSharingAKeyPendingDeletionIsRefusedBeforeAnythingIsRecorded checks a dataset on its
// way out, and that asking the key first turns the one unrecoverable case into a clean
// refusal.
//
// The key's state is what decides it rather than the record's, because a deletion
// scheduled outside this tool leaves the ledger behind: the copy still reads as staged
// while the data it points at is already being destroyed. Recording the grant first here
// would leave one that nothing could ever deploy -- reconciliation included, since the key
// that would have to decrypt is the thing being destroyed -- so the state is read before
// the record is written, and nothing is written at all.
func TestSharingAKeyPendingDeletionIsRefusedBeforeAnythingIsRecorded(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.keys.DescribeKeyFn = keyInState(kmstypes.KeyStatePendingDeletion)

	_, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err == nil {
		t.Fatal("granting a dataset whose key is being destroyed must fail")
	}

	var undeployed *undeployedGrantError
	if errors.As(err, &undeployed) {
		t.Fatalf("expected a refusal with nothing recorded, got a grant left undeployed: %v", err)
	}
	for _, want := range []string{"nothing was recorded", "PendingDeletion", "purged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the report to carry %q, got %v", want, err)
		}
	}

	if len(doubles.objects.PutBucketPolicyCalls) != 0 || len(doubles.keys.PutKeyPolicyCalls) != 0 {
		t.Error("a key being destroyed must stop both documents being written")
	}
	grants, err := doubles.grantStore().GrantsOfCopy(context.Background(), staged)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 0 {
		t.Errorf("expected nothing in the ledger, got %d grant(s): %+v", len(grants), grants)
	}
	if got := doubles.attr(t, copyID(1), "state"); got != string(dataset.CopyStateStaged) {
		t.Errorf("the copy moved to %q; nothing here changes what was transferred", got)
	}
}

// TestSharingWithNoExpiryRecordsAnUnlimitedGrant checks that an absent expiry is carried
// through as an absence rather than defaulted to something distant.
//
// It has to reach three places consistently: the record carries no expiry attribute at
// all, the deployed documents carry no time condition, and the report says so. A far-future
// date would satisfy none of them -- it would be indistinguishable from a long grant when
// the deployed policy is read back, which is exactly the question reconciliation exists to
// answer.
func TestSharingWithNoExpiryRecordsAnUnlimitedGrant(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)

	out, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err != nil {
		t.Fatalf("granting: %v\n%s", err, out)
	}

	grants, err := doubles.grantStore().GrantsOfDataset(context.Background(), releaseDataset)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected one grant, got %d", len(grants))
	}
	if got := doubles.attr(t, grants[0].GrantID, "expiresAt"); got != "" {
		t.Errorf("an unlimited grant recorded an expiry of %q", got)
	}
	if !strings.Contains(out, "Expiry:      unlimited") {
		t.Errorf("expected the report to name the grant as unlimited, got:\n%s", out)
	}

	written := bucketPolicyWrites(doubles)
	if len(written) != 1 {
		t.Fatalf("expected one bucket policy written, got %d", len(written))
	}
	if strings.Contains(written[0], "DateLessThan") {
		t.Errorf("an unlimited grant wrote a time condition:\n%s", written[0])
	}
}

// TestSharingRecordsTheGrantAndPublishesTheManifest covers the whole of a successful
// grant: the record written first, both documents written from it, and the researcher's
// own description of the access republished.
func TestSharingRecordsTheGrantAndPublishesTheManifest(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)

	expiresAt := releaseClock.Add(30 * 24 * time.Hour)
	out, errOut, err := shareTheDataset(t, doubles, shareRequest(&expiresAt))
	if err != nil {
		t.Fatalf("granting: %v\n%s", err, out)
	}

	if len(doubles.objects.PutBucketPolicyCalls) != 1 || len(doubles.keys.PutKeyPolicyCalls) != 1 {
		t.Fatalf("expected both documents written once, got %d bucket and %d key writes",
			len(doubles.objects.PutBucketPolicyCalls), len(doubles.keys.PutKeyPolicyCalls))
	}

	written := bucketPolicyWrites(doubles)[0]
	for _, want := range []string{releaseResearcher, releaseVpc, expiresAt.Format(time.RFC3339)} {
		if !strings.Contains(written, want) {
			t.Errorf("expected the deployed bucket policy to carry %q, got:\n%s", want, written)
		}
	}

	grants, err := doubles.grantStore().GrantsOfDataset(context.Background(), releaseDataset)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected one grant, got %d", len(grants))
	}
	grant := grants[0]
	if grant.CopyID != copyID(1) {
		t.Errorf("the grant reads copy %q, want %s", grant.CopyID, copyID(1))
	}
	if grant.ResearcherAccountID != releaseResearcher || grant.TargetVpcID != releaseVpc {
		t.Errorf("recorded grant = %+v, want the researcher and VPC that were asked for", grant)
	}
	if got := doubles.attr(t, grant.GrantID, "kind"); got != "grant" {
		t.Errorf("the record was written as a %q rather than a grant", got)
	}
	// The statements are named after the grant, which is what lets a second one sit
	// beside them without either being unmatchable.
	if !strings.Contains(written, grant.GrantID) {
		t.Errorf("the deployed statements are not named after grant %s:\n%s", grant.GrantID, written)
	}

	published := manifestWrites(doubles)
	if len(published) != 1 {
		t.Fatalf("expected the researcher's manifest to be published once, got %v", published)
	}
	if !strings.Contains(published[0], releaseResearcher) {
		t.Errorf("expected the manifest to be written under the researcher's own key, got %q", published[0])
	}

	// The three ways a grant can fail to work are indistinguishable to the researcher, so
	// the command says so rather than letting an operator assume.
	if !strings.Contains(errOut, "target VPC") {
		t.Errorf("expected a note that the grant can only be confirmed by reading, got:\n%s", errOut)
	}
}

// TestSharingReportsAManifestThatCouldNotBePublished covers the one failure that leaves
// nothing dangerous behind.
//
// The grant is exactly what was asked for -- recorded, and both documents written from it
// -- and what is stale is only the researcher's description of it. So the grant stands and
// the failure is reported rather than anything being unwound. It is still a failure rather
// than a warning: nothing reads the document back, so a stale one would go unnoticed until
// a researcher could not find data they had been given.
func TestSharingReportsAManifestThatCouldNotBePublished(t *testing.T) {
	doubles := newReleaseDoubles()
	doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.objects.PutObjectFn = func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
		return nil, errors.New("the manifest bucket refused the write")
	}

	out, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err == nil {
		t.Fatal("a manifest that could not be published must be reported")
	}

	var unpublished *manifestNotPublishedError
	if !errors.As(err, &unpublished) {
		t.Fatalf("expected the failure to name the manifest, got %v", err)
	}
	for _, want := range []string{releaseResearcher, "exactly as intended", "again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the report to carry %q, got %v", want, err)
		}
	}

	// The grant itself is untouched by the failure, and the report already said so.
	if len(doubles.objects.PutBucketPolicyCalls) != 1 || len(doubles.keys.PutKeyPolicyCalls) != 1 {
		t.Error("the documents were written; the failure is about the description of them")
	}
	if !strings.Contains(out, "Dataset released successfully") {
		t.Errorf("expected the grant itself to still be reported, got:\n%s", out)
	}
}

// TestSharingReportsAGrantThatCouldNotBeDeployed checks the failure the ordering here is
// chosen to leave behind, and that it is the mild one.
//
// The record says the researcher may read and the documents say nothing about them, so
// they cannot: no access exists that no record describes. What the report has to make
// plain is that the grant is recorded and not deployed, and that repeating the command
// would be refused as a duplicate of it.
func TestSharingReportsAGrantThatCouldNotBeDeployed(t *testing.T) {
	doubles := newReleaseDoubles()
	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	doubles.objects.PutBucketPolicyFn = func(context.Context, *s3.PutBucketPolicyInput) (*s3.PutBucketPolicyOutput, error) {
		return nil, errors.New("the bucket refused the policy")
	}

	_, _, err := shareTheDataset(t, doubles, shareRequest(nil))
	if err == nil {
		t.Fatal("a grant whose policies could not be written must be reported as a failure")
	}

	var undeployed *undeployedGrantError
	if !errors.As(err, &undeployed) {
		t.Fatalf("expected the failure to name the grant left undeployed, got %v", err)
	}
	for _, want := range []string{"can read nothing", "Reconcile copy", "duplicate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the report to carry %q, got %v", want, err)
		}
	}

	grants, err := doubles.grantStore().GrantsOfCopy(context.Background(), staged)
	if err != nil {
		t.Fatalf("reading the grants back: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected the grant to be recorded, got %d", len(grants))
	}
	if !grants[0].IsDeployed() {
		t.Error("the recorded grant should still read as granted, which is what reconciliation deploys")
	}
}

// TestSharingRejectsAResearcherVpcInTheWrongForm checks the input the two mechanisms most
// plausibly confuse. The bucket policy compares this value literally against the VPC a read
// arrived from, so an ARN here is written happily and denies every read.
func TestSharingRejectsAResearcherVpcInTheWrongForm(t *testing.T) {
	cases := map[string]string{
		"a VPC ARN":      "arn:aws:ec2:ap-southeast-2:123456789012:vpc/vpc-0a1b2c3d4e5f60718",
		"an empty value": "",
		"a bare word":    "not-a-vpc",
	}
	for name, vpcID := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkTargetVpcID(vpcID); err == nil {
				t.Errorf("expected %q to be refused as a researcher VPC identifier", vpcID)
			}
		})
	}

	if err := checkTargetVpcID(releaseVpc); err != nil {
		t.Errorf("the bare identifier is the accepted form, got %v", err)
	}
}

// TestSharingRejectsAResearcherAccountThatNamesNoPrincipal covers the same class of
// mistake on the account: the value is written into both documents as the principal being
// granted, where an alias or a role ARN names nobody at all.
func TestSharingRejectsAResearcherAccountThatNamesNoPrincipal(t *testing.T) {
	for _, account := range []string{"", "my-researcher", "12345", "arn:aws:iam::123456789012:root"} {
		if err := checkResearcherAccount(account); err == nil {
			t.Errorf("expected %q to be refused as a researcher account", account)
		}
	}
	if err := checkResearcherAccount(releaseResearcher); err != nil {
		t.Errorf("a 12-digit account id is the accepted form, got %v", err)
	}
}
