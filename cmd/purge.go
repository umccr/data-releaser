package cmd

// Purging destroys this account's copy of a dataset, by scheduling deletion of the key
// that copy is encrypted under. It is the only irreversible thing this tool does: once
// the recovery window elapses the objects in the cache bucket are ciphertext nobody can
// read, ourselves included.
//
// Three facts shape the command.
//
// One key encrypts one dataset's bucket, so the unit destroyed is the dataset and never
// one copy of it or one researcher's access to it. The command is therefore keyed on the
// dataset rather than on any record identifier, and every copy of that dataset and every
// grant made against those copies is listed before anything is asked, because they all
// die together and an operator thinking about one of them should see the rest.
//
// A scheduled deletion can be cancelled until its date passes, and cancelling it makes
// the key able to decrypt again. A grant left standing in the policies would come back
// to life with it, so every grant still deployed is taken out of the policies first and
// the key is scheduled second. The reversible half of the operation cannot then quietly
// restore access to a dataset someone decided to destroy.
//
// The authoritative copy of the data lives in the external source store, and nothing
// here touches it. What is destroyed is the cache, not the dataset itself, which is why
// a purged dataset can be copied in again from scratch.

import (
	"bufio"
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

// The recovery window KMS accepts, in days. The longest is the default: this is the only
// window in which a purge decided in error can still be taken back, so the tool leans
// towards leaving as much of it as possible.
const (
	minPendingWindowDays     = 7
	maxPendingWindowDays     = 30
	defaultPendingWindowDays = maxPendingWindowDays
)

var purgeCmd = &cobra.Command{
	Use:   "purge <mechanism>",
	Short: "Destroy this account's copy of a dataset",
	Long: `Destroy the copy of a dataset held in this account.

Purging is irreversible once its recovery window elapses, and it takes every copy
of the dataset and every grant of access to them with it. The copy held in the
external source store is untouched.

The mechanism is named as part of the invocation and is never inferred. Only the
mechanism that holds a copy of the data has anything to destroy; the available
mechanisms are listed below.`,
}

var purgeS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Schedule destruction of a dataset's copied objects by deleting the key encrypting them",
	Long: `Destroy the copy of one dataset held in this account by scheduling deletion of
the key that copy is encrypted under.

The dataset is named rather than a copy or a grant, because one key encrypts one
dataset's cache bucket: destroying it destroys every copy of that dataset and
every researcher's access to them at once. All of them are listed before
confirmation is sought.

Access is taken out of the bucket and key policies first, and the key is
scheduled for deletion second. A scheduled deletion can be cancelled while its
recovery window runs, and cancelling it would otherwise revive a grant that was
left in place.

Nothing here reads or writes the external source store. The authoritative copy
of the data lives there, so a purged dataset can be copied in again.`,
	RunE: runPurgeS3,
}

func init() {
	purgeS3Cmd.Flags().String("dataset-uri", "",
		"URI of the dataset whose copy in this account is to be destroyed")
	purgeS3Cmd.Flags().Int32("pending-window-days", defaultPendingWindowDays,
		fmt.Sprintf("Days before deletion of the key becomes irreversible (%d-%d). This is the "+
			"only window in which the purge can still be taken back",
			minPendingWindowDays, maxPendingWindowDays))
	purgeS3Cmd.Flags().Bool("yes", false,
		"Destroy the copy without asking. Intended for a script that has already established "+
			"what it is destroying; every other invocation should confirm interactively")

	_ = purgeS3Cmd.MarkFlagRequired("dataset-uri")

	purgeCmd.AddCommand(purgeS3Cmd)
	requireMechanism(purgeCmd)
	rootCmd.AddCommand(purgeCmd)
}

func runPurgeS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	req, err := readPurgeRequest(cmd)
	if err != nil {
		return err
	}

	// The cache bucket and the key are read from the records themselves rather than
	// derived here: what is destroyed has to be what the copy actually staged in, not
	// what a name derivation says it should have been.
	cfg, err := requireConfig(
		config.SectionAccount,
		config.SectionLedger,
		config.SectionReleasePolicies,
		config.SectionDatasetManifest,
	)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return err
	}

	purger, err := newPurger(cfg, releaseClients{
		ledger:  factory.DynamoDBClient(),
		objects: factory.S3Client(),
		keys:    factory.KMSClient(),
	})
	if err != nil {
		return err
	}

	return purger.purge(ctx, cmd, req)
}

// purgeRequest is one purge invocation, as read and checked before anything is looked up.
type purgeRequest struct {
	// datasetURI names the dataset whose copy is destroyed.
	datasetURI string

	// pendingWindowDays is how long the scheduled deletion can still be cancelled for.
	pendingWindowDays int32

	// assumeYes destroys the copy without asking.
	assumeYes bool
}

// readPurgeRequest reads and checks the flags. The window is bounded here rather than
// left to KMS, because the confirmation quotes it: an operator has to be told the same
// number the schedule will actually carry.
func readPurgeRequest(cmd *cobra.Command) (purgeRequest, error) {
	datasetURI, err := cmd.Flags().GetString("dataset-uri")
	if err != nil {
		return purgeRequest{}, fmt.Errorf("reading --dataset-uri flag: %w", err)
	}
	// Trimmed once, here, because the URI is both the key records of this dataset are
	// found under and the phrase the confirmation compares against.
	datasetURI = strings.TrimSpace(datasetURI)

	window, err := cmd.Flags().GetInt32("pending-window-days")
	if err != nil {
		return purgeRequest{}, fmt.Errorf("reading --pending-window-days flag: %w", err)
	}
	assumeYes, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return purgeRequest{}, fmt.Errorf("reading --yes flag: %w", err)
	}

	if datasetURI == "" {
		return purgeRequest{}, errors.New(
			"the dataset whose copy is to be destroyed is required (--dataset-uri must not be empty)")
	}
	canonical, err := dataset.CanonicalDatasetURI(datasetURI)
	if err != nil {
		return purgeRequest{}, err
	}
	// The canonical form is what the ledger is queried with and what the confirmation asks
	// the operator to type back, so both describe the same dataset as the records do.
	datasetURI = canonical
	if window < minPendingWindowDays || window > maxPendingWindowDays {
		return purgeRequest{}, fmt.Errorf(
			"--pending-window-days must be between %d and %d, got %d; it is the only window in "+
				"which a purge can be taken back, and KMS accepts no other value",
			minPendingWindowDays, maxPendingWindowDays, window)
	}

	return purgeRequest{
		datasetURI:        datasetURI,
		pendingWindowDays: window,
		assumeYes:         assumeYes,
	}, nil
}

// purger destroys one dataset's copy: it works out what would die, has that confirmed,
// takes access out of the policies, schedules the key, and records what it did.
//
// It builds on the same things the other mutating commands do, because a purge is a
// withdrawal of every grant at once that then destroys what was withdrawn: the same two
// ledgers, the same applier writing the same two documents, and the same publisher
// rebuilding the description each affected researcher reads. The key client is its own,
// since scheduling a deletion is the one thing no other command does.
type purger struct {
	*releaser

	accountID string
	keys      internalaws.KMSClient
}

func newPurger(cfg *config.Config, clients releaseClients) (*purger, error) {
	releaser, err := newReleaser(cfg, clients)
	if err != nil {
		return nil, err
	}
	return &purger{
		releaser:  releaser,
		accountID: strings.TrimSpace(cfg.Account),
		keys:      clients.keys,
	}, nil
}

// purge destroys the copy of one dataset.
//
// Nothing is written until the whole of what would happen has been established and
// confirmed: which copies die, whose access goes with them, which keys are scheduled, and
// which of those are already on their way out. An operator who abandons the confirmation
// has changed nothing, and so has one whose dataset turns out to have nothing left to
// destroy.
func (p *purger) purge(ctx context.Context, cmd *cobra.Command, req purgeRequest) error {
	out := cmd.OutOrStdout()

	copies, err := p.copies.CopiesOfDataset(ctx, req.datasetURI)
	if err != nil {
		return err
	}
	if len(copies) == 0 {
		return &nothingToPurgeError{datasetURI: req.datasetURI}
	}

	plan, err := p.planPurge(ctx, req, copies)
	if err != nil {
		return err
	}

	if plan.settled() {
		reportSettledPurge(out, plan)
		return nil
	}

	reportPurgePlan(out, plan)

	if !req.assumeYes {
		confirmed, err := confirmPurge(out, cmd.InOrStdin(), plan)
		if err != nil {
			return err
		}
		if !confirmed {
			return &purgeDeclinedError{plan: plan}
		}
	}

	return p.execute(ctx, cmd, plan)
}

// purgePlan is everything one purge would do, worked out before any of it is done.
type purgePlan struct {
	datasetURI string
	// now is the clock the reported states and the projected date were derived against.
	now               time.Time
	pendingWindowDays int32

	// copies is every copy of the dataset, newest first, whatever state each is in. All
	// of them are reported, because destroying the key destroys the lot.
	copies []*dataset.Copy

	// grants is every grant made against those copies, whatever state each is in. They
	// are reported alongside the copies because a grant is what names the researcher, and
	// a copy never does.
	grants []*dataset.Grant

	// withdraw pairs each copy with the grants of it still deployed. They are grouped by
	// copy because the two documents are shared by every grant against one and are
	// rendered whole: taking one researcher out is the same pair of writes as taking all
	// of them out.
	withdraw []copyWithdrawal

	// record are the copies whose destruction has to be written down. A copy already
	// recorded as purged is absent: its outcome is already what this would say.
	record []*dataset.Copy

	// keys are the keys encrypting this dataset's copies, one entry each however many
	// copies share it.
	keys []*purgeKey
}

// copyWithdrawal is one copy and the grants of it whose statements have to come out.
type copyWithdrawal struct {
	copy   *dataset.Copy
	grants []*dataset.Grant
}

// purgeKey is one key protecting a dataset's copy, and what is to become of it.
type purgeKey struct {
	arn   string
	state kmstypes.KeyState
	// buckets are the cache buckets this key encrypts among the dataset's copies.
	buckets []string
	// scheduled is the date deletion becomes irreversible, where the key is already on
	// its way out. Nil means this run has to schedule it.
	scheduled *time.Time
}

// settled reports whether the dataset is already destroyed as far as this command is
// concerned: nobody's access left to take out, every copy recorded as purged, and every
// key on its way out. Running again then schedules nothing.
func (p *purgePlan) settled() bool {
	if len(p.withdraw) > 0 || len(p.record) > 0 {
		return false
	}
	return !slices.ContainsFunc(p.keys, func(k *purgeKey) bool { return k.scheduled == nil })
}

// deployedGrants counts the grants whose statements are still in the deployed documents,
// which is what the confirmation quotes and what the withdrawal step will take out.
func (p *purgePlan) deployedGrants() int {
	total := 0
	for _, withdrawal := range p.withdraw {
		total += len(withdrawal.grants)
	}
	return total
}

// irreversibleFrom is the date the operator is actually up against: the earliest at which
// some part of this copy becomes unrecoverable.
//
// A key this run schedules goes by the window that was asked for. A key already on its way
// out keeps the date it is already counting down to, which may be much sooner — quoting the
// window in that case would promise time the operator does not have.
func (p *purgePlan) irreversibleFrom() time.Time {
	projected := p.now.AddDate(0, 0, int(p.pendingWindowDays))

	earliest := projected
	for _, key := range p.keys {
		if key.scheduled != nil && key.scheduled.Before(earliest) {
			earliest = *key.scheduled
		}
	}
	return earliest
}

// planPurge works out what destroying this dataset would involve.
//
// Every key is inspected first, so that a key already being destroyed is recognised as
// such rather than scheduled a second time and refused. That is also what lets a purge
// begun outside this tool be finished by it: the ledger is brought into line with what
// the key already says.
func (p *purger) planPurge(ctx context.Context, req purgeRequest, copies []*dataset.Copy) (*purgePlan, error) {
	plan := &purgePlan{
		datasetURI:        req.datasetURI,
		now:               p.now(),
		pendingWindowDays: req.pendingWindowDays,
		copies:            copies,
	}

	byKey := make(map[string]*purgeKey)
	for _, c := range copies {
		grants, err := p.grants.GrantsOfCopy(ctx, c)
		if err != nil {
			return nil, err
		}
		plan.grants = append(plan.grants, grants...)

		// The deployed ones are the only ones with statements to take out. Expiry leaves
		// those statements in place, so a grant whose expiry has passed is withdrawn here
		// too.
		var deployed []*dataset.Grant
		for _, grant := range grants {
			if grant.IsDeployed() {
				deployed = append(deployed, grant)
			}
		}
		if len(deployed) > 0 {
			plan.withdraw = append(plan.withdraw, copyWithdrawal{copy: c, grants: deployed})
		}

		// A copy already purged has nothing left to record, and one whose objects have
		// since been deleted has moved past purged for good: neither can be written
		// again, and attempting it would report a failure for a purge that is complete.
		if c.State != dataset.CopyStatePurged && c.State != dataset.CopyStateDeleted {
			plan.record = append(plan.record, c)
		}

		key, seen := byKey[c.KeyArn]
		if !seen {
			key = &purgeKey{arn: c.KeyArn}
			byKey[c.KeyArn] = key
			plan.keys = append(plan.keys, key)
		}
		if !slices.Contains(key.buckets, c.HotCacheBucket) {
			key.buckets = append(key.buckets, c.HotCacheBucket)
		}
	}

	// Reported together, so an operator whose per-dataset resources are in an unexpected
	// state learns all of it from one run rather than one key per run.
	report := &dataset.PrerequisiteReport{}
	for _, key := range plan.keys {
		state, deletionDate, err := p.describeKey(ctx, key.arn)
		if err != nil {
			report.Add(dataset.DatasetKeyPrerequisite(key.arn, p.accountID).Missing(fmt.Errorf(
				"reading the state of the key encrypting dataset %q before destroying it. Nothing "+
					"was scheduled, so the copy is exactly as it was: %w", req.datasetURI, err)))
			continue
		}
		key.state = state
		key.scheduled = deletionDate
	}
	if err := report.Err(); err != nil {
		return nil, err
	}

	return plan, nil
}

// describeKey reports a key's state, and the date its deletion becomes irreversible where
// it is already on its way out.
func (p *purger) describeKey(ctx context.Context, keyArn string) (kmstypes.KeyState, *time.Time, error) {
	described, err := p.keys.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(keyArn)})
	if err != nil {
		return "", nil, err
	}
	if described.KeyMetadata == nil || strings.TrimSpace(string(described.KeyMetadata.KeyState)) == "" {
		return "", nil, errors.New("the key was found but reported no state")
	}

	state := described.KeyMetadata.KeyState
	if state != kmstypes.KeyStatePendingDeletion && state != kmstypes.KeyStatePendingReplicaDeletion {
		return state, nil, nil
	}

	// Already on its way out, so this run schedules nothing for it and records the date
	// it is already going by.
	deletionDate := described.KeyMetadata.DeletionDate
	if deletionDate == nil {
		return state, nil, nil
	}
	scheduled := deletionDate.UTC()
	return state, &scheduled, nil
}

// execute carries the plan out, in the order that keeps the reversible half of it from
// undoing the rest: access out of the policies, then the key scheduled, then what
// happened written down.
//
// Progress is reported as it is made. Each step is a change an operator cannot see from
// anywhere else in this tool until the last one records it, so a failure halfway has to
// leave behind an account of what already took effect.
func (p *purger) execute(ctx context.Context, cmd *cobra.Command, plan *purgePlan) error {
	fmt.Fprintln(cmd.OutOrStdout())
	if err := p.withdrawAccess(ctx, cmd, plan); err != nil {
		return err
	}

	deletionDates, err := p.scheduleKeys(ctx, cmd, plan)
	if err != nil {
		return err
	}

	if err := p.recordPurge(ctx, cmd, plan, deletionDates); err != nil {
		return err
	}

	return p.publishAffectedManifests(ctx, cmd, plan)
}

// publishAffectedManifests rebuilds the description held by every researcher who had
// access to this dataset.
//
// It runs after the records have been written, so each document is built from a ledger
// that already reports the grants as withdrawn -- which for an account holding nothing
// else means the document is removed rather than left describing data on its way out.
// Every account is attempted whatever the ones before it did, since one researcher's
// document failing to publish is no reason to leave another's stale.
func (p *purger) publishAffectedManifests(ctx context.Context, cmd *cobra.Command, plan *purgePlan) error {
	var failures []string

	for _, accountID := range affectedAccounts(plan.grants) {
		if err := p.publishManifest(ctx, cmd, accountID, "the copy is being destroyed"); err != nil {
			failures = append(failures, err.Error())
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf(
			"the copy of dataset %q is being destroyed and access to it has stopped, but %d "+
				"researcher manifest(s) could not be republished: %s",
			plan.datasetURI, len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// affectedAccounts lists the researcher accounts a dataset's grants named, each once and
// in the order they were first seen, so an account holding two grants is not published
// twice.
func affectedAccounts(grants []*dataset.Grant) []string {
	var accounts []string
	for _, grant := range grants {
		account := strings.TrimSpace(grant.ResearcherAccountID)
		if account != "" && !slices.Contains(accounts, account) {
			accounts = append(accounts, account)
		}
	}
	return accounts
}

// withdrawAccess takes the granting statements out of the deployed documents for every
// grant still carrying them.
//
// This is the whole of stopping access, and it is done before the key is scheduled
// precisely because scheduling can be cancelled: a cancelled deletion restores the key's
// ability to decrypt, and a grant still in the policies would then be live again.
//
// One pair of writes per copy takes every one of its grants out at once. The documents are
// shared by all of them and rendered whole, so writing them once with all the grants
// described as withdrawn is both cheaper and safer than a write per researcher: there is
// no intermediate document that still admits half of them.
func (p *purger) withdrawAccess(ctx context.Context, cmd *cobra.Command, plan *purgePlan) error {
	for _, withdrawal := range plan.withdraw {
		target := policyTargetFor(withdrawal.copy, revokedGrants(withdrawal.grants))

		application, err := p.applier.Apply(ctx, target)
		if err != nil {
			return fmt.Errorf(
				"dataset %q was not destroyed: the access granted against copy %s could not be "+
					"taken out of the deployed policies, and no key deletion was scheduled. %w",
				plan.datasetURI, withdrawal.copy.CopyID, err)
		}

		for _, grant := range withdrawal.grants {
			if _, err := p.grants.MarkRevoked(ctx, grant.GrantID); err != nil {
				return fmt.Errorf(
					"the deployed policies of dataset %q no longer grant account %s any access, so "+
						"their reads have stopped, but grant %s could not be updated to record it "+
						"and no key deletion was scheduled: %w",
					plan.datasetURI, grant.ResearcherAccountID, grant.GrantID, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"Access withdrawn: grant %s no longer gives account %s anything (%s)\n",
				grant.GrantID, grant.ResearcherAccountID, formatAppliedTargets(application))
		}
	}
	return nil
}

// revokedGrants describes a copy's grants as the ledger will describe them once the
// withdrawals below are recorded, which is what makes the rendered pair grant nobody
// anything. The stored records are untouched; they are written afterwards.
func revokedGrants(grants []*dataset.Grant) []*dataset.Grant {
	described := make([]*dataset.Grant, 0, len(grants))
	for _, grant := range grants {
		withdrawn := *grant
		withdrawn.State = dataset.GrantStateRevoked
		described = append(described, &withdrawn)
	}
	return described
}

// scheduleKeys schedules deletion of every key still holding this dataset's copy, and
// returns the date each becomes irreversible.
//
// This is the destructive step and the point of no return: from here the copy is on its
// way out whatever else fails, which is why the date comes back from the key rather than
// being computed from the window that was asked for.
func (p *purger) scheduleKeys(ctx context.Context, cmd *cobra.Command, plan *purgePlan) (map[string]time.Time, error) {
	out := cmd.OutOrStdout()
	dates := make(map[string]time.Time, len(plan.keys))

	for _, key := range plan.keys {
		if key.scheduled != nil {
			dates[key.arn] = *key.scheduled
			fmt.Fprintf(out, "Key already being destroyed: %s, irreversible from %s\n",
				key.arn, key.scheduled.Format(time.RFC3339))
			continue
		}

		scheduled, err := p.keys.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
			KeyId:               aws.String(key.arn),
			PendingWindowInDays: aws.Int32(plan.pendingWindowDays),
		})
		if err != nil {
			return nil, fmt.Errorf(
				"dataset %q was not destroyed: deletion of the key encrypting %s could not be "+
					"scheduled, so the copy it holds is still readable by this account. Access "+
					"granted to anyone has already been withdrawn: %w",
				plan.datasetURI, strings.Join(key.buckets, ", "),
				dataset.DatasetKeyPrerequisite(key.arn, p.accountID).Missing(err))
		}

		// The window KMS applied is what the date has to come from: a schedule that was
		// clamped or rounded would otherwise be recorded as the window we asked for.
		date := p.now()
		if scheduled.DeletionDate != nil {
			date = scheduled.DeletionDate.UTC()
		} else {
			warnf(cmd, "the key %s reported no deletion date, so %s is recorded instead; read the "+
				"key's own pending window before relying on that date",
				key.arn, date.Format(time.RFC3339))
		}
		dates[key.arn] = date

		fmt.Fprintf(out, "Key scheduled for deletion: %s, irreversible from %s\n",
			key.arn, date.Format(time.RFC3339))
	}

	return dates, nil
}

// recordPurge writes the destruction down against every copy the key takes with it.
//
// It runs last because it is the only reversible part: the key is already scheduled by
// now, so a record that could not be written is a reporting failure rather than a
// discrepancy in who can read what. It is still worth reporting loudly, since until it is
// put right a listing will describe copies that are being destroyed as though they were
// intact.
func (p *purger) recordPurge(
	ctx context.Context,
	cmd *cobra.Command,
	plan *purgePlan,
	deletionDates map[string]time.Time,
) error {
	var unrecorded []string
	purged := make([]*dataset.Copy, 0, len(plan.record))

	for _, c := range plan.record {
		date, known := deletionDates[c.KeyArn]
		if !known {
			date = p.now()
		}

		recorded, err := p.copies.MarkPurged(ctx, c.CopyID, date)
		if err != nil {
			unrecorded = append(unrecorded, fmt.Sprintf("%s (%v)", c.CopyID, err))
			continue
		}
		purged = append(purged, recorded)
	}

	reportPurge(cmd.OutOrStdout(), plan, purged)

	if len(unrecorded) > 0 {
		return fmt.Errorf(
			"the copy of dataset %q is being destroyed and access to it has stopped, but %d of its "+
				"copies could not be updated to record that: %s. Run the same command again to "+
				"record them; nothing further will be scheduled",
			plan.datasetURI, len(unrecorded), strings.Join(unrecorded, "; "))
	}
	return nil
}

// purgeRows renders everything the destruction takes with it, the copies and the grants
// made against them in one table.
//
// Both kinds are shown because neither answers on its own. A copy says what was
// transferred and where it staged; a grant says who has been reading it. An operator about
// to destroy the lot needs the second at least as much as the first.
func purgeRows(plan *purgePlan) []listingRow {
	rows := append(copyRows(plan.copies), grantRows(plan.grants, plan.now)...)
	sortRows(rows)
	return rows
}

// reportPurgePlan describes everything that is about to be destroyed, before anything is
// asked and before anything is done.
//
// Every copy of the dataset is listed and so is every grant against them, not only the
// ones still giving access, because the key is per dataset: an operator reasoning about
// one record is about to destroy all of them, and the ones they were not thinking about
// are exactly the ones worth showing.
func reportPurgePlan(out io.Writer, plan *purgePlan) {
	fmt.Fprintf(out, "Purging dataset %s destroys the copy of it held in this account.\n\n", plan.datasetURI)

	fmt.Fprintf(out, "Destroyed with it (%d copy record(s), %d grant(s)):\n",
		len(plan.copies), len(plan.grants))
	renderListing(out, purgeRows(plan))

	fmt.Fprintf(out, "\nWhat is destroyed:\n")
	for _, key := range plan.keys {
		fmt.Fprintf(out, "  Bucket:  %s\n", strings.Join(key.buckets, ", "))
		fmt.Fprintf(out, "  Key:     %s (%s)\n", key.arn, key.state)
		if key.scheduled != nil {
			fmt.Fprintf(out, "           deletion is already scheduled and irreversible from %s; "+
				"nothing further will be scheduled for it\n", key.scheduled.Format(time.RFC3339))
		}
	}

	if deployed := plan.deployedGrants(); deployed > 0 {
		fmt.Fprintf(out, "\nAccess withdrawn first (%d grant(s) still deployed):\n", deployed)
		for _, withdrawal := range plan.withdraw {
			for _, grant := range withdrawal.grants {
				fmt.Fprintf(out, "  %s to account %s from %s, expiry %s\n",
					grant.GrantID,
					valueOrAbsent(grant.ResearcherAccountID),
					valueOrAbsent(grant.TargetVpcID),
					formatExpiry(grant.ExpiresAt))
			}
		}
	}

	fmt.Fprintf(out, `
Deleting the key is how the copy is destroyed: every object in the bucket above
is encrypted under it and nothing can read them once it is gone -- not the
researcher, not this account, not us. The deletion can be cancelled until %s,
after which it is irreversible and the copy is lost for good.

The authoritative copy in the external source store is untouched, so the dataset
can be copied in again from scratch. Anything already downloaded by a researcher
is beyond reach of this or any other command.
`, plan.irreversibleFrom().Format(time.RFC3339))
}

// confirmPurge asks the operator to confirm by naming the dataset they are destroying.
//
// Naming it, rather than answering yes, is deliberate for the one operation that cannot be
// undone: it costs a copy and paste, and it will not be given by accident to a prompt that
// appeared where the operator did not expect one.
//
// Anything else declines, and so does an invocation with nothing on its input at all. A
// script that meant to destroy something says so with the flag instead.
func confirmPurge(out io.Writer, in io.Reader, plan *purgePlan) (bool, error) {
	fmt.Fprintf(out, "\nTo destroy this copy, type the dataset URI (%s). Anything else abandons the purge: ",
		plan.datasetURI)

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading the confirmation, so nothing was destroyed: %w", err)
	}
	fmt.Fprintln(out)

	return strings.TrimSpace(line) == plan.datasetURI, nil
}

// reportSettledPurge describes a dataset whose copy is already being destroyed, which is
// what a repeated purge finds. Nothing is scheduled a second time, and the dates reported
// are the ones already in force.
func reportSettledPurge(out io.Writer, plan *purgePlan) {
	fmt.Fprintf(out, "Dataset %s is already purged; nothing was scheduled.\n\n", plan.datasetURI)

	for _, key := range plan.keys {
		fmt.Fprintf(out, "  Bucket:  %s\n", strings.Join(key.buckets, ", "))
		fmt.Fprintf(out, "  Key:     %s (%s)\n", key.arn, key.state)
		if key.scheduled != nil {
			fmt.Fprintf(out, "           irreversible from %s\n", key.scheduled.Format(time.RFC3339))
		}
	}

	fmt.Fprintf(out, "\nRecords (%d copy record(s), %d grant(s)):\n", len(plan.copies), len(plan.grants))
	renderListing(out, purgeRows(plan))
	fmt.Fprintf(out, "\nThe copy in the external source store is unaffected, so the dataset can be "+
		"copied in again.\n")
}

// reportPurge describes what was destroyed, from the records as they now stand rather than
// from the plan, so that what is printed is what was stored.
func reportPurge(out io.Writer, plan *purgePlan, purged []*dataset.Copy) {
	fmt.Fprintf(out, "\nDataset purged:\n")
	fmt.Fprintf(out, "  Dataset:  %s\n", plan.datasetURI)
	for _, key := range plan.keys {
		fmt.Fprintf(out, "  Key:      %s\n", key.arn)
		fmt.Fprintf(out, "  Bucket:   %s\n", strings.Join(key.buckets, ", "))
	}
	for _, c := range purged {
		irreversible := absentValue
		if c.KeyDeletionDate != nil {
			irreversible = c.KeyDeletionDate.Format(time.RFC3339)
		}
		fmt.Fprintf(out, "  Copy:     %s now %s, irreversible from %s\n",
			c.CopyID, c.State, irreversible)
	}

	fmt.Fprintf(out, "\nAccess has already stopped: a key pending deletion decrypts nothing. Until "+
		"the date above the deletion can still be cancelled, which would leave the objects "+
		"readable by this account but granted to nobody.\n")
}

// nothingToPurgeError reports a dataset that has no copy in this account to destroy.
// Nothing was read beyond the ledger and nothing was written.
type nothingToPurgeError struct {
	datasetURI string
}

func (e *nothingToPurgeError) Error() string {
	return fmt.Sprintf(
		"dataset %q has never been copied into this account, so there is nothing here to destroy; "+
			"no key deletion was scheduled and no policy was written. Check the URI against the one "+
			"the dataset was copied under -- purging is keyed on the dataset, and a URI spelled "+
			"differently names a different one",
		e.datasetURI)
}

// purgeDeclinedError reports a purge the operator abandoned. It is an error rather than a
// quiet return so that the exit status distinguishes a copy that was destroyed from one
// that was left alone, which a script cannot otherwise tell.
type purgeDeclinedError struct {
	plan *purgePlan
}

func (e *purgeDeclinedError) Error() string {
	return fmt.Sprintf(
		"purge of dataset %q was abandoned: no key deletion was scheduled, no policy was written, "+
			"and its %d copy record(s) and %d grant(s) are exactly as they were",
		e.plan.datasetURI, len(e.plan.copies), len(e.plan.grants))
}
