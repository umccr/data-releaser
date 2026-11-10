package cmd

// Restoring takes a purge back while its recovery window is still running.
//
// A purge destroys this account's copy of a dataset by scheduling deletion of the key it
// is encrypted under, and KMS keeps that schedule cancellable for the whole of its
// pending window -- seven to thirty days -- precisely so a decision made in error can be
// undone. Without this command the tool can only schedule, never unschedule: an operator
// has to drop to raw AWS, and even then the ledger still reads purged, so a grant is
// refused and the only way forward is copying in again a dataset that is sitting in the
// bucket, intact, the whole time.
//
// What makes taking it back safe is the order the purge did things in. Every grant is
// withdrawn from the deployed documents before the key is scheduled, so the key is the
// only thing a cancellation gives back. Nobody's access comes with it: the grants stay
// withdrawn, which is what grants being records of their own buys, and a researcher gets
// their access back only when somebody grants it again on purpose.
//
// Cancelling is not the whole of it, and the half that is easy to miss is the reason this
// is a command rather than a note in the docs telling an operator to run two AWS calls. A
// cancelled deletion leaves the key Disabled, not Enabled, and a disabled key decrypts
// nothing -- which presents to a researcher exactly like the purge did. So the key is
// enabled afterwards and then read back, and a key that does not report itself enabled
// fails the run loudly rather than leaving a restore that looks done and works for
// nobody.
//
// The key is recovered first and the records written second. A failure in between leaves
// copies still reading purged against a key that works, which is the safe direction --
// nothing is granted either way -- and running the command again finishes the job.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/spf13/cobra"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
)

var restoreCmd = &cobra.Command{
	Use:   "restore <mechanism>",
	Short: "Take back a purge while its recovery window is still open",
	Long: `Take back the purge of a dataset, while the recovery window on its key is
still running.

The copy is already in the bucket and nothing is transferred a second time.
Nobody's access comes back with it: the grants the purge withdrew stay
withdrawn, and granting access again is a separate, explicit act.

The mechanism is named as part of the invocation and is never inferred. Only
the mechanism that holds a copy of the data has a purge to take back; the
available mechanisms are listed below.`,
}

var restoreS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Cancel the scheduled deletion of a dataset's key and stage its copy again",
	Long: `Cancel the scheduled deletion of the key encrypting one dataset's copy, and
record that copy as staged again.

The dataset is named rather than a copy, because one key encrypts one dataset's
cache bucket: cancelling its deletion recovers every copy of that dataset at
once. The objects were never touched, so nothing is copied again.

The deletion is cancelled and the key is then enabled, because cancelling on
its own leaves the key disabled and a disabled key decrypts nothing. The key is
read back afterwards and the run fails if it does not report itself enabled.

Nobody has access afterwards. A purge withdraws every grant before it schedules
the key, which is what makes cancelling safe, and those grants stay withdrawn:
share the dataset again to give anyone access.

This is refused once the objects have been deleted, and once the pending window
has elapsed and the key material is destroyed. Neither has a way back.`,
	RunE: runRestoreS3,
}

func init() {
	restoreS3Cmd.Flags().String("dataset-uri", "",
		"URI of the dataset whose purge is to be taken back")

	_ = restoreS3Cmd.MarkFlagRequired("dataset-uri")

	restoreCmd.AddCommand(restoreS3Cmd)
	requireMechanism(restoreCmd)
	rootCmd.AddCommand(restoreCmd)
}

func runRestoreS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	req, err := readRestoreRequest(cmd)
	if err != nil {
		return err
	}

	// The ledger and the key, because that is all this touches. Nothing here renders a
	// policy or publishes a researcher's document: no grant changes, so there is nothing
	// new for either to say. The key recovered is the one the copy records name rather
	// than one derived from the dataset URI, so what is brought back is what the copy was
	// actually encrypted under.
	cfg, err := requireConfig(config.SectionLedger)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return err
	}

	restorer := newRestorer(cfg, releaseClients{
		ledger: factory.DynamoDBClient(),
		keys:   factory.KMSClient(),
	})
	return restorer.restore(ctx, cmd, req)
}

// restoreRequest is one invocation, as read and checked before anything is looked up.
type restoreRequest struct {
	// datasetURI names the dataset whose purge is taken back.
	datasetURI string
}

// readRestoreRequest reads and checks the flags.
func readRestoreRequest(cmd *cobra.Command) (restoreRequest, error) {
	datasetURI, err := cmd.Flags().GetString("dataset-uri")
	if err != nil {
		return restoreRequest{}, fmt.Errorf("reading --dataset-uri flag: %w", err)
	}
	datasetURI = strings.TrimSpace(datasetURI)

	if datasetURI == "" {
		return restoreRequest{}, errors.New(
			"the dataset whose purge is to be taken back is required (--dataset-uri must not be empty)")
	}
	// The canonical form is the key the records of this dataset are found under, so the
	// ledger is queried for the same dataset the purge recorded against.
	canonical, err := dataset.CanonicalDatasetURI(datasetURI)
	if err != nil {
		return restoreRequest{}, err
	}
	return restoreRequest{datasetURI: canonical}, nil
}

// restorer recovers one dataset's key and stages its copies back.
//
// It holds the two ledgers and the key client and nothing else. The copy ledger is what
// is written; the grant ledger is only read, to be able to say that the withdrawals the
// purge made are still standing, which is the part of the report an operator is most
// likely to assume the other way round.
type restorer struct {
	copies *dataset.CopyStore
	grants *dataset.GrantStore
	keys   internalaws.KMSClient

	// now is indirected so a test can pin the clock the listing derives an expiry
	// against. Nothing this command writes is timestamped from it.
	now func() time.Time
}

func newRestorer(cfg *config.Config, clients releaseClients) *restorer {
	return &restorer{
		copies: dataset.NewCopyStore(clients.ledger, cfg.Ledger.Table),
		grants: dataset.NewGrantStore(clients.ledger, cfg.Ledger.Table),
		keys:   clients.keys,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// restore takes back the purge of one dataset.
//
// Nothing is written until the whole of what would happen has been established: which
// copies come back, that none of them has had its objects deleted, and that every key
// they name can still be recovered. A refusal has changed nothing at all, which is worth
// stating for a command whose refusals are mostly about things that cannot be undone.
func (r *restorer) restore(ctx context.Context, cmd *cobra.Command, req restoreRequest) error {
	copies, err := r.copies.CopiesOfDataset(ctx, req.datasetURI)
	if err != nil {
		return err
	}
	if len(copies) == 0 {
		return &nothingToRestoreError{datasetURI: req.datasetURI}
	}

	plan, err := r.planRestore(ctx, req, copies)
	if err != nil {
		return err
	}

	reportRestorePlan(cmd.OutOrStdout(), plan)
	return r.execute(ctx, cmd, plan)
}

// restorePlan is everything one restore would do, worked out before any of it is done.
type restorePlan struct {
	datasetURI string
	// now is the clock the reported grant states were derived against.
	now time.Time

	// copies is every copy of the dataset, newest first. All of them are reported,
	// because the key is per dataset and recovering it recovers the lot.
	copies []*dataset.Copy

	// record are the purged copies, which are the ones staged back. A copy that failed
	// to transfer is absent: recovering the key does not turn a partial copy into a
	// dataset, and it was never granted to anybody.
	record []*dataset.Copy

	// grants is every grant made against those copies, all of them withdrawn by now.
	// They are carried so the report can say they stay that way.
	grants []*dataset.Grant

	// keys are the keys encrypting this dataset's copies, one entry each however many
	// copies share it.
	keys []*restoreKey
}

// restoreKey is one key protecting a dataset's copy, and what has to be done to it.
type restoreKey struct {
	arn   string
	state kmstypes.KeyState
	// buckets are the cache buckets this key encrypts among the dataset's copies.
	buckets []string
	// scheduled is the date the key's deletion becomes irreversible, and nil where it is
	// not counting down to one. It is what the report quotes as the deadline that was
	// beaten.
	scheduled *time.Time
}

// mustCancel reports whether this key still has a deletion to call off. A key that has
// already had one cancelled -- by an interrupted run of this command, or by hand -- has
// nothing left to cancel and is not scheduled a second time.
func (k *restoreKey) mustCancel() bool {
	return k.state == kmstypes.KeyStatePendingDeletion || k.state == kmstypes.KeyStatePendingReplicaDeletion
}

// mustEnable reports whether this key still has to be enabled. Cancelling a deletion
// leaves a key disabled, so a key that was scheduled needs it, and so does one somebody
// cancelled by hand and left there.
func (k *restoreKey) mustEnable() bool {
	return k.state != kmstypes.KeyStateEnabled
}

// isRecoverable reports whether this key can be made to decrypt again.
//
// The three states that qualify are the ones a purge and a half-finished recovery leave
// behind: a deletion still pending, a key cancelled but left disabled, and one already
// enabled. Everything else is refused, because a key in some other state is not one this
// command can do anything for, and a copy staged back against it would report itself
// grantable while decrypting nothing.
func (k *restoreKey) isRecoverable() bool {
	return k.mustCancel() || k.state == kmstypes.KeyStateDisabled || k.state == kmstypes.KeyStateEnabled
}

// planRestore works out what taking this purge back would involve, and refuses before it
// reads a single key where it must not happen at all.
//
// The two refusals about the copies come first, and in this order. A copy whose objects
// have been deleted is the one thing here with no way back: the ciphertext the key would
// decrypt is not there any more, so cancelling the deletion buys nothing, and saying that
// is more use than saying no copy is purged -- which is also true of it, and answers a
// question the operator did not ask. A dataset with no purged copy at all is the ordinary
// mistake: it was never purged, or it has already been restored.
func (r *restorer) planRestore(ctx context.Context, req restoreRequest, copies []*dataset.Copy) (*restorePlan, error) {
	plan := &restorePlan{datasetURI: req.datasetURI, now: r.now(), copies: copies}

	var deleted []*dataset.Copy
	for _, c := range copies {
		if c.State == dataset.CopyStateDeleted {
			deleted = append(deleted, c)
		}
	}
	if len(deleted) > 0 {
		return nil, &copiesDeletedError{datasetURI: req.datasetURI, deleted: deleted}
	}

	for _, c := range copies {
		if c.State == dataset.CopyStatePurged {
			plan.record = append(plan.record, c)
		}
	}
	if len(plan.record) == 0 {
		return nil, &nothingPurgedError{datasetURI: req.datasetURI, copies: copies}
	}

	var stillDeployed []*dataset.Grant
	byKey := make(map[string]*restoreKey)
	for _, c := range copies {
		// Read defensively. A purge withdraws every grant of the dataset before it
		// schedules the key, which is the whole reason cancelling the schedule is safe:
		// a grant still in the deployed documents would have its access handed back the
		// moment the key can decrypt again, without anybody granting it.
		grants, err := r.grants.GrantsOfCopy(ctx, c)
		if err != nil {
			return nil, err
		}
		plan.grants = append(plan.grants, grants...)
		for _, grant := range grants {
			if grant.IsDeployed() {
				stillDeployed = append(stillDeployed, grant)
			}
		}

		key, seen := byKey[c.KeyArn]
		if !seen {
			key = &restoreKey{arn: c.KeyArn}
			byKey[c.KeyArn] = key
			plan.keys = append(plan.keys, key)
		}
		if !slices.Contains(key.buckets, c.HotCacheBucket) {
			key.buckets = append(key.buckets, c.HotCacheBucket)
		}
	}
	if len(stillDeployed) > 0 {
		return nil, &grantsWouldReviveError{datasetURI: req.datasetURI, grants: stillDeployed}
	}

	return plan, r.describeKeys(ctx, plan)
}

// describeKeys reads the state of every key the dataset's copies name, and refuses unless
// all of them can be brought back.
//
// They are reported together rather than one per run, so an operator whose dataset spans
// more than one key learns the whole of what stands in the way at once. Nothing has been
// written by this point and nothing is called on any key, so a refusal here leaves the
// purge exactly as it was.
func (r *restorer) describeKeys(ctx context.Context, plan *restorePlan) error {
	var refused []restoreKeyRefusal

	for _, key := range plan.keys {
		state, deletionDate, err := r.describeKey(ctx, key.arn)
		if err != nil {
			// A key that is simply not there is the window having elapsed: KMS deletes
			// the key when the schedule matures, and with it the only thing that could
			// ever decrypt the objects. That reads quite differently from a key that is
			// present in a state this cannot act on, so the two are separated here rather
			// than reported as one failure to read a key.
			var absent *kmstypes.NotFoundException
			if errors.As(err, &absent) {
				refused = append(refused, restoreKeyRefusal{key: key, destroyed: true})
				continue
			}
			return fmt.Errorf(
				"the state of the key encrypting dataset %q could not be read, so no deletion was "+
					"cancelled and every copy is exactly as it was: %w", plan.datasetURI, err)
		}

		key.state = state
		key.scheduled = deletionDate
		if !key.isRecoverable() {
			refused = append(refused, restoreKeyRefusal{key: key})
		}
	}

	if len(refused) > 0 {
		return &keysNotRestorableError{datasetURI: plan.datasetURI, refused: refused}
	}
	return nil
}

// describeKey reports a key's state, and the date its deletion becomes irreversible where
// it is counting down to one.
func (r *restorer) describeKey(ctx context.Context, keyArn string) (kmstypes.KeyState, *time.Time, error) {
	described, err := r.keys.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(keyArn)})
	if err != nil {
		return "", nil, err
	}
	if described.KeyMetadata == nil || strings.TrimSpace(string(described.KeyMetadata.KeyState)) == "" {
		return "", nil, errors.New("the key was found but reported no state")
	}

	state := described.KeyMetadata.KeyState
	if described.KeyMetadata.DeletionDate == nil {
		return state, nil, nil
	}
	scheduled := described.KeyMetadata.DeletionDate.UTC()
	return state, &scheduled, nil
}

// execute carries the plan out: the key first, then the record of it having come back.
//
// That order is the recovery story. A failure between the two leaves copies recorded as
// purged against a key that works, which grants nobody anything and is put right by
// running the command again. The other order would record copies as staged against a key
// that still cannot decrypt, and a grant made against one of them would deny every read
// for a reason nothing in the ledger explains.
func (r *restorer) execute(ctx context.Context, cmd *cobra.Command, plan *restorePlan) error {
	fmt.Fprintln(cmd.OutOrStdout())
	if err := r.recoverKeys(ctx, cmd, plan); err != nil {
		return err
	}
	return r.recordRestore(ctx, cmd, plan)
}

// recoverKeys cancels the scheduled deletion of every key the dataset's copies name and
// enables it afterwards.
//
// Both steps, and in that order, because either alone leaves a key that decrypts nothing.
// Cancelling returns a key to disabled rather than to enabled, and a disabled key denies
// every read exactly as a purged one does, so a run that stopped between them would look
// finished and work for nobody.
//
// The key is then read back. Nothing else in this command can tell whether the key came
// back, and a researcher granted access against one that did not would meet an
// unexplained denial, so the check is made here where it can still fail the run.
func (r *restorer) recoverKeys(ctx context.Context, cmd *cobra.Command, plan *restorePlan) error {
	out := cmd.OutOrStdout()

	for _, key := range plan.keys {
		if !key.mustCancel() && !key.mustEnable() {
			fmt.Fprintf(out, "Key already usable: %s (%s), nothing was cancelled\n", key.arn, key.state)
			continue
		}

		if key.mustCancel() {
			if _, err := r.keys.CancelKeyDeletion(ctx, &kms.CancelKeyDeletionInput{
				KeyId: aws.String(key.arn),
			}); err != nil {
				return &keyNotRecoveredError{datasetURI: plan.datasetURI, key: key, cause: err}
			}
			fmt.Fprintf(out, "Key deletion cancelled: %s\n", key.arn)
		}

		if _, err := r.keys.EnableKey(ctx, &kms.EnableKeyInput{KeyId: aws.String(key.arn)}); err != nil {
			return &keyNotRecoveredError{
				datasetURI: plan.datasetURI,
				key:        key,
				cancelled:  key.mustCancel(),
				cause:      err,
			}
		}

		state, _, err := r.describeKey(ctx, key.arn)
		if err != nil {
			return &keyNotRecoveredError{
				datasetURI: plan.datasetURI,
				key:        key,
				cancelled:  key.mustCancel(),
				enabled:    true,
				cause:      err,
			}
		}
		if state != kmstypes.KeyStateEnabled {
			return &keyNotEnabledError{datasetURI: plan.datasetURI, key: key, state: state}
		}

		fmt.Fprintf(out, "Key enabled: %s (%s)\n", key.arn, state)
	}
	return nil
}

// recordRestore stages every purged copy of the dataset back.
//
// It runs last because it is the only reversible part: the key already decrypts by now,
// so a record that could not be written is a reporting failure rather than a discrepancy
// in who can read what. It is still worth reporting loudly, since until it is put right a
// listing describes copies as being destroyed when their key is working, and a grant of
// them is refused.
func (r *restorer) recordRestore(ctx context.Context, cmd *cobra.Command, plan *restorePlan) error {
	var unrecorded []string
	restored := make([]*dataset.Copy, 0, len(plan.record))

	for _, c := range plan.record {
		recorded, err := r.copies.MarkRestored(ctx, c.CopyID)
		if err != nil {
			unrecorded = append(unrecorded, fmt.Sprintf("%s (%v)", c.CopyID, err))
			continue
		}
		restored = append(restored, recorded)
	}

	reportRestore(cmd.OutOrStdout(), plan, restored)

	if len(unrecorded) > 0 {
		return fmt.Errorf(
			"the key encrypting dataset %q decrypts again, but %d of its copies could not be "+
				"updated to record that: %s. They still read as purged, so nothing can be granted "+
				"against them and nobody has access either way. Run the same command again to "+
				"finish it; the key is already usable, so nothing further is cancelled",
			plan.datasetURI, len(unrecorded), strings.Join(unrecorded, "; "))
	}
	return nil
}

// restoreRows renders the copies coming back and the grants that are not, in one table.
//
// The grants are shown for what they do not say. An operator taking a purge back is
// usually thinking about a researcher who lost access, and the answer -- that every one of
// those grants is still withdrawn -- is easier to take from a listing that names them than
// from a sentence.
func restoreRows(plan *restorePlan) []listingRow {
	rows := append(copyRows(plan.copies), grantRows(plan.grants, plan.now)...)
	sortRows(rows)
	return rows
}

// reportRestorePlan describes what is about to be recovered, before anything is done.
func reportRestorePlan(out io.Writer, plan *restorePlan) {
	fmt.Fprintf(out, "Restoring dataset %s takes back the purge of the copy held in this account.\n\n",
		plan.datasetURI)

	// The rows are the records as they stand, which is why the heading says what is about
	// to change rather than describing them as already changed: a copy still reads purged
	// here, and reporting it as staged before the key has been recovered would be a claim
	// about work that has not happened and may yet fail.
	fmt.Fprintf(out, "Records of this dataset (%d of %d copy record(s) staged again; %d grant(s) stay withdrawn):\n",
		len(plan.record), len(plan.copies), len(plan.grants))
	renderListing(out, restoreRows(plan))

	fmt.Fprintf(out, "\nWhat is recovered:\n")
	for _, key := range plan.keys {
		fmt.Fprintf(out, "  Bucket:  %s\n", strings.Join(key.buckets, ", "))
		fmt.Fprintf(out, "  Key:     %s (%s)\n", key.arn, key.state)
		if key.scheduled != nil {
			fmt.Fprintf(out, "           its deletion becomes irreversible at %s\n",
				key.scheduled.Format(time.RFC3339))
		}
	}
}

// reportRestore describes what was recovered, taking the copies from the records as they
// now stand rather than from the plan, so that what is printed is what was stored.
//
// It closes on the thing an operator is most likely to have assumed the other way round:
// nobody has access. The copy is back and readable by this account, and that is the whole
// of what a restore does.
func reportRestore(out io.Writer, plan *restorePlan, restored []*dataset.Copy) {
	fmt.Fprintf(out, "\nDataset restored:\n")
	fmt.Fprintf(out, "  Dataset:  %s\n", plan.datasetURI)
	for _, key := range plan.keys {
		fmt.Fprintf(out, "  Key:      %s\n", key.arn)
		fmt.Fprintf(out, "  Bucket:   %s\n", strings.Join(key.buckets, ", "))
	}
	for _, c := range restored {
		fmt.Fprintf(out, "  Copy:     %s now %s\n", c.CopyID, c.State)
	}

	fmt.Fprintf(out, `
Nobody has access yet. A purge withdraws every grant before it schedules the
key -- which is what makes cancelling the deletion safe -- and those %d grant(s)
stay withdrawn: cancelling gives none of them back. Share the dataset again to
grant access, which costs nothing beyond the policy write, since the objects
never left the bucket and are not transferred a second time.
`, len(plan.grants))
}

// nothingToRestoreError reports a dataset that has no copy in this account, and so no
// purge here to take back. Nothing was read beyond the ledger and nothing was cancelled.
type nothingToRestoreError struct {
	datasetURI string
}

func (e *nothingToRestoreError) Error() string {
	return fmt.Sprintf(
		"dataset %q has never been copied into this account, so there is no purge here to take "+
			"back; no key was read and no deletion was cancelled. Check the URI against the one the "+
			"dataset was copied under -- this is keyed on the dataset, and a URI spelled differently "+
			"names a different one",
		e.datasetURI)
}

// copiesDeletedError reports a dataset whose objects have been deleted out of the bucket.
// Nothing was read beyond the ledger and nothing was cancelled.
//
// This is the refusal with no way round it. The recovery window makes a purge reversible
// because the ciphertext is still there for the key to decrypt; deleting the objects took
// that away, and cancelling the key deletion now recovers the ability to decrypt nothing.
type copiesDeletedError struct {
	datasetURI string
	// deleted are the copies whose objects are gone, newest first.
	deleted []*dataset.Copy
}

func (e *copiesDeletedError) Error() string {
	described := make([]string, 0, len(e.deleted))
	for _, c := range e.deleted {
		when := absentValue
		if c.DeletedAt != nil {
			when = c.DeletedAt.Format(time.RFC3339)
		}
		described = append(described, fmt.Sprintf("%s (deleted %s)", c.CopyID, when))
	}

	return fmt.Sprintf(
		"the purge of dataset %q cannot be taken back: the objects %d of its copies staged have "+
			"been deleted out of the bucket -- %s. Cancelling the key deletion would recover the "+
			"ability to decrypt objects that are not there. Deleting them has no window and no way "+
			"back; stage the dataset again to copy it in from the source store, which is untouched. "+
			"Nothing was cancelled",
		e.datasetURI, len(e.deleted), strings.Join(described, ", "))
}

// nothingPurgedError reports a dataset that has copies here but no purged one, which is
// what an already-restored dataset and a never-purged one both look like. Nothing was
// cancelled.
type nothingPurgedError struct {
	datasetURI string
	copies     []*dataset.Copy
}

func (e *nothingPurgedError) Error() string {
	described := make([]string, 0, len(e.copies))
	for _, c := range e.copies {
		described = append(described, fmt.Sprintf("%s is %s", c.CopyID, c.State))
	}

	return fmt.Sprintf(
		"no copy of dataset %q is purged, so there is nothing here to take back: %s. A dataset "+
			"whose copies are staged has either been restored already or was never purged, and "+
			"either way its copy is readable and can be shared as it is. Nothing was cancelled",
		e.datasetURI, strings.Join(described, ", "))
}

// grantsWouldReviveError reports a dataset whose deployed documents still grant somebody
// access. Nothing was cancelled.
//
// It should not arise: a purge takes every grant out of the deployed documents before it
// schedules the key, and that ordering is exactly what makes cancelling the schedule safe.
// So this is the backstop for a purge that did not finish, and it matters here more than
// anywhere -- recovering the key would hand that researcher their access back without
// anybody granting it, which is the one thing a restore must never do quietly.
type grantsWouldReviveError struct {
	datasetURI string
	grants     []*dataset.Grant
}

func (e *grantsWouldReviveError) Error() string {
	described := make([]string, 0, len(e.grants))
	for _, grant := range e.grants {
		described = append(described, fmt.Sprintf("%s to account %s",
			grant.GrantID, valueOrAbsent(grant.ResearcherAccountID)))
	}

	return fmt.Sprintf(
		"the purge of dataset %q was not taken back: %d grant(s) of it are still in the deployed "+
			"documents (%s). Making the key decrypt again would give those accounts their reads "+
			"back without anybody granting them, which a restore must never do quietly. A purge "+
			"withdraws every grant before it schedules the key, so a grant still deployed means one "+
			"did not finish; withdraw them, or purge the dataset again, before restoring it. "+
			"Nothing was cancelled",
		e.datasetURI, len(e.grants), strings.Join(described, ", "))
}

// restoreKeyRefusal is one key that stands in the way, and why.
type restoreKeyRefusal struct {
	key *restoreKey
	// destroyed says the key is gone rather than merely in the wrong state, which is what
	// the pending window elapsing leaves behind.
	destroyed bool
}

// describe renders the refusal as a phrase naming the key and what became of it.
func (r restoreKeyRefusal) describe() string {
	if r.destroyed {
		return fmt.Sprintf("%s no longer exists, so its pending window has elapsed and the key "+
			"material is destroyed", r.key.arn)
	}
	return fmt.Sprintf("%s is %s, so no deletion of it is pending", r.key.arn, r.key.state)
}

// keysNotRestorableError reports keys that cannot be made to decrypt again. Nothing was
// cancelled and no copy was written.
//
// The two ways a key gets here read quite differently and are reported as such. A key that
// is gone is the window having elapsed, which is the end of the story: the objects in the
// bucket are ciphertext nobody will ever read. A key that is present but in some other
// state was never scheduled, or is in a state this command cannot act on, and the dataset
// is not being destroyed at all.
type keysNotRestorableError struct {
	datasetURI string
	refused    []restoreKeyRefusal
}

func (e *keysNotRestorableError) Error() string {
	described := make([]string, 0, len(e.refused))
	destroyed := 0
	for _, refusal := range e.refused {
		described = append(described, refusal.describe())
		if refusal.destroyed {
			destroyed++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "the purge of dataset %q was not taken back: %s", e.datasetURI,
		strings.Join(described, "; "))
	if destroyed > 0 {
		b.WriteString(". A destroyed key has no way back and the objects it encrypted cannot be " +
			"read by anyone again; stage the dataset to copy it in from the source store, which is " +
			"untouched")
	} else {
		b.WriteString(". Restoring cancels a deletion that is pending, and there is none to cancel")
	}
	b.WriteString(". Nothing was cancelled and no copy was written")
	return b.String()
}

// keyNotRecoveredError reports a key that could not be brought back. Whatever the run
// managed before it stopped is recorded in the message rather than in the ledger, which
// still reads exactly as the purge left it.
type keyNotRecoveredError struct {
	datasetURI string
	key        *restoreKey
	// cancelled and enabled say how far this key got, so the report distinguishes a
	// deletion still pending from one already called off.
	cancelled bool
	enabled   bool
	cause     error
}

func (e *keyNotRecoveredError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the purge of dataset %q was not taken back: the key %s could not be ",
		e.datasetURI, e.key.arn)
	switch {
	case e.enabled:
		b.WriteString("read back after it was enabled")
	case e.cancelled:
		b.WriteString("enabled after its deletion was cancelled")
	default:
		b.WriteString("taken off its scheduled deletion")
	}
	fmt.Fprintf(&b, ": %v. ", e.cause)

	if e.cancelled {
		b.WriteString("The deletion is no longer pending, so the copy is no longer counting down, " +
			"but the key may still be disabled and a disabled key decrypts nothing. ")
	} else {
		b.WriteString("The deletion is still pending and still becomes irreversible on its own " +
			"date. ")
	}
	b.WriteString("No copy was recorded as staged, so nothing can be granted against this dataset " +
		"and nobody has access either way. Run the same command again to finish it")
	return b.String()
}

func (e *keyNotRecoveredError) Unwrap() error {
	return e.cause
}

// keyNotEnabledError reports a key that came off its scheduled deletion and did not come
// back enabled. It fails the run rather than warning, because a disabled key is
// indistinguishable from a purged one to everyone who matters: it decrypts nothing, every
// read is denied, and nothing else in this tool inspects the key again.
type keyNotEnabledError struct {
	datasetURI string
	key        *restoreKey
	// state is what the key reported after it was enabled.
	state kmstypes.KeyState
}

func (e *keyNotEnabledError) Error() string {
	return fmt.Sprintf(
		"the purge of dataset %q was not taken back: the deletion of key %s was cancelled and the "+
			"key was enabled, but it reports itself %s rather than %s. A key that is not enabled "+
			"decrypts nothing and presents to a researcher exactly as the purge did, so no copy was "+
			"recorded as staged and nothing can be granted against this dataset. Look at the key "+
			"itself -- a key store or imported key material can leave it unusable for reasons "+
			"nothing here can put right -- and run the same command again once it reports enabled; "+
			"the deletion is already cancelled, so nothing further is",
		e.datasetURI, e.key.arn, e.state, kmstypes.KeyStateEnabled)
}
