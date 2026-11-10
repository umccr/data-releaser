package cmd

// Deleting a dataset's objects empties the bucket its copy was staged into. What that
// buys is the storage those objects occupy, and nothing else.
//
// Access stopped at the purge. A key pending deletion decrypts nothing from the moment it
// is scheduled, so what a purged bucket holds is ciphertext nobody can read -- not the
// researcher, not this account, not us -- and the bucket's lifecycle rule expires it on
// its own in any case, a year after it was written unless the deployment set another
// retention. This command is therefore not a second kill switch, and anyone reaching for
// it to stop somebody reading has misread which command does that. What it does is
// reclaim that storage now rather than whenever the lifecycle rule gets to it.
//
// It is nonetheless the one thing here with no way back at all. A scheduled key deletion
// can be cancelled for as long as its window runs, and cancelling it restores a purged
// copy completely: the objects are still there and the key can read them again. Deleting
// the objects takes that path away too. There is no window, nothing to cancel, and the
// objects are gone the moment the call returns.
//
// Three things follow from the objects belonging to a bucket rather than to a copy.
//
// The bucket is deployed per dataset and every copy of that dataset stages into it, so
// the command is keyed on the dataset and refuses unless every copy of it is one nobody
// is reading. A purged copy sharing a bucket with a staged one is a bucket somebody may
// still be reading, and emptying it would take away access that was never withdrawn.
//
// The objects are deleted first and the records written afterwards. A run that fails
// partway then leaves copies recorded as purged with some of their objects gone, which is
// honest, because purged never claimed the objects were there; recording first would
// claim a deletion that had not happened. Deleting an object that is not there succeeds,
// so an interrupted run is finished by running it again.
//
// The bucket itself stays. It is deployed by the per-dataset stack, which retains it on
// purpose, and removing it out of band would leave that stack describing a bucket that is
// not there. Emptying it is the right granularity, and it leaves the dataset able to be
// staged into the same place again.

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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/cobra"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
)

// deleteBatchSize is the most keys one batched delete may carry. It is the API's own
// ceiling rather than a choice of ours: a request naming more than this is refused whole.
const deleteBatchSize = 1000

// maxReportedFailures bounds how many refused keys a failure quotes before counting the
// rest. A batch can refuse a thousand keys at once, and a message naming all of them is
// one nobody reads to the end.
const maxReportedFailures = 5

var deleteCmd = &cobra.Command{
	Use:   "delete <mechanism>",
	Short: "Delete the objects of a dataset's copy out of its bucket",
	Long: `Delete the objects one dataset was copied into this account as.

This reclaims the storage the copy occupies and does nothing else. It does not
stop anyone reading: purge does that, and a dataset has to have been purged --
or to have failed to copy at all -- before its objects can be deleted. The
deletion itself cannot be cancelled or undone.

The mechanism is named as part of the invocation and is never inferred. Only
the mechanism that holds a copy of the data has any objects to delete; the
available mechanisms are listed below.`,
}

var deleteS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Empty a dataset's cache bucket of the objects copied into it",
	Long: `Delete every object out of the bucket holding one dataset's copy.

This buys storage and nothing else. Access to the copy ended when the dataset
was purged: a key pending deletion decrypts nothing, so what is deleted here is
ciphertext nobody can read, which the bucket's lifecycle rule would expire on
its own regardless -- a year after it was written, by default. To stop a
researcher reading, purge the dataset: this is not a second way of doing that.

There is no window and no way back. A scheduled key deletion can be cancelled
while it runs, and cancelling it makes a purged copy readable again; once the
objects are deleted that recovery is gone as well.

The dataset is named rather than one copy of it, because the objects sit in one
bucket every copy of the dataset shares. Every copy must therefore be purged or
failed and no grant of the dataset may still be deployed; anything else is
refused and named. The copy's own control artefacts go with the rest: they
describe a transfer whose objects are being deleted.

The bucket is left in place, so the dataset can be staged into it again. The
copy in the external source store is untouched throughout.`,
	RunE: runDeleteS3,
}

func init() {
	deleteS3Cmd.Flags().String("dataset-uri", "",
		"URI of the dataset whose copied objects are to be deleted")
	deleteS3Cmd.Flags().Bool("yes", false,
		"Delete the objects without asking. Intended for a script that has already established "+
			"what it is deleting; every other invocation should confirm interactively")

	_ = deleteS3Cmd.MarkFlagRequired("dataset-uri")

	deleteCmd.AddCommand(deleteS3Cmd)
	requireMechanism(deleteCmd)
	rootCmd.AddCommand(deleteCmd)
}

func runDeleteS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	req, err := readDeleteRequest(cmd)
	if err != nil {
		return err
	}

	// The ledger alone, because that is all this reads. The bucket emptied is the one the
	// copy records name rather than one derived from the dataset URI: what is deleted has
	// to be what the copy actually staged in. Nothing here renders a policy or publishes
	// a document, so nothing here needs the configuration those are built from.
	cfg, err := requireConfig(config.SectionLedger)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return err
	}

	deleter := newDeleter(cfg, releaseClients{
		ledger:  factory.DynamoDBClient(),
		objects: factory.S3Client(),
	})
	return deleter.delete(ctx, cmd, req)
}

// deleteRequest is one invocation, as read and checked before anything is looked up.
type deleteRequest struct {
	// datasetURI names the dataset whose copied objects are deleted.
	datasetURI string

	// assumeYes deletes them without asking.
	assumeYes bool
}

// readDeleteRequest reads and checks the flags.
func readDeleteRequest(cmd *cobra.Command) (deleteRequest, error) {
	datasetURI, err := cmd.Flags().GetString("dataset-uri")
	if err != nil {
		return deleteRequest{}, fmt.Errorf("reading --dataset-uri flag: %w", err)
	}
	// Trimmed once, here, because the URI is both the key the records of this dataset are
	// found under and the phrase the confirmation compares against.
	datasetURI = strings.TrimSpace(datasetURI)

	assumeYes, err := cmd.Flags().GetBool("yes")
	if err != nil {
		return deleteRequest{}, fmt.Errorf("reading --yes flag: %w", err)
	}

	if datasetURI == "" {
		return deleteRequest{}, errors.New(
			"the dataset whose copied objects are to be deleted is required (--dataset-uri must not be empty)")
	}
	canonical, err := dataset.CanonicalDatasetURI(datasetURI)
	if err != nil {
		return deleteRequest{}, err
	}
	// The canonical form is what the ledger is queried with and what the confirmation asks
	// the operator to type back, so both describe the same dataset as the records do.
	return deleteRequest{datasetURI: canonical, assumeYes: assumeYes}, nil
}

// deleter empties one dataset's bucket and records that against its copies.
//
// It holds less than the commands that move a grant do, and deliberately: nothing here
// renders a policy, because access ended at the purge, and nothing here publishes a
// researcher's document, because a researcher whose grants were all withdrawn has already
// had theirs rebuilt. What is left is the two ledgers -- one to say which copies these
// objects belong to, the other to confirm nobody is still reading them -- and the client
// that does the deleting.
type deleter struct {
	copies  *dataset.CopyStore
	grants  *dataset.GrantStore
	objects internalaws.S3Client
}

func newDeleter(cfg *config.Config, clients releaseClients) *deleter {
	return &deleter{
		copies:  dataset.NewCopyStore(clients.ledger, cfg.Ledger.Table),
		grants:  dataset.NewGrantStore(clients.ledger, cfg.Ledger.Table),
		objects: clients.objects,
	}
}

// delete empties the bucket holding one dataset's copy.
//
// Nothing is deleted until the whole of what would go has been established and confirmed:
// which copies the objects belong to, that none of them is one anybody could still be
// reading, how many objects there are and how much they weigh. An operator who abandons
// the confirmation has deleted nothing, and so has one whose dataset turns out to have
// nothing left to delete.
func (d *deleter) delete(ctx context.Context, cmd *cobra.Command, req deleteRequest) error {
	out := cmd.OutOrStdout()

	copies, err := d.copies.CopiesOfDataset(ctx, req.datasetURI)
	if err != nil {
		return err
	}
	if len(copies) == 0 {
		return &nothingToDeleteError{datasetURI: req.datasetURI}
	}

	plan, err := d.planDelete(ctx, req, copies)
	if err != nil {
		return err
	}

	if plan.settled() {
		reportSettledDelete(out, plan)
		return nil
	}

	reportDeletePlan(out, plan)

	if !req.assumeYes {
		confirmed, err := confirmDelete(out, cmd.InOrStdin(), plan)
		if err != nil {
			return err
		}
		if !confirmed {
			return &deleteDeclinedError{plan: plan}
		}
	}

	return d.execute(ctx, cmd, plan)
}

// deletePlan is everything one deletion would do, worked out before any of it is done.
type deletePlan struct {
	datasetURI string

	// copies is every copy of the dataset, newest first. All of them are reported,
	// because the objects belong to the bucket they share rather than to any one of them.
	copies []*dataset.Copy

	// record are the copies whose deletion has to be written down. A copy already
	// recorded as deleted is absent: its outcome is already what this would say.
	record []*dataset.Copy

	// buckets are the cache buckets the dataset's copies staged into, one entry each
	// however many copies share it, together with what each holds.
	buckets []*deleteBucket
}

// deleteBucket is one cache bucket and the objects it is about to lose.
type deleteBucket struct {
	name string
	// keys is every object in the bucket, the copy's own control artefacts included:
	// they describe a transfer whose objects are going, and nothing reads them once it
	// has.
	keys []string
	// totalBytes is what those objects weigh, which is the whole of what deleting them
	// buys and so the number the report leads with.
	totalBytes int64
}

// settled reports whether the dataset is already emptied as far as this command is
// concerned: every copy recorded as deleted and no object left in the bucket. Running
// again then deletes nothing.
func (p *deletePlan) settled() bool {
	return len(p.record) == 0 && p.objectCount() == 0
}

// objectCount and totalBytes are what is about to go, across every bucket the dataset's
// copies named.
func (p *deletePlan) objectCount() int {
	total := 0
	for _, bucket := range p.buckets {
		total += len(bucket.keys)
	}
	return total
}

func (p *deletePlan) totalBytes() int64 {
	var total int64
	for _, bucket := range p.buckets {
		total += bucket.totalBytes
	}
	return total
}

// planDelete works out what emptying this dataset's bucket would involve, and refuses
// before it reads a single object where it must not happen at all.
//
// The refusals come first because they are the answer that matters most, and because both
// of them are about somebody still reading. A copy that has not been purged is one whose
// key still decrypts; a grant still deployed is a researcher whose statements are still in
// the documents. Either way the objects would be deleted out from under a live read, which
// is the mistake the ordering of purge and this exists to prevent.
func (d *deleter) planDelete(ctx context.Context, req deleteRequest, copies []*dataset.Copy) (*deletePlan, error) {
	plan := &deletePlan{datasetURI: req.datasetURI, copies: copies}

	// Every copy is checked before anything else is read, so a dataset that must not be
	// emptied is refused without a listing of thousands of objects first, and so that the
	// report names all the copies standing in the way rather than one per run.
	var blocking []*dataset.Copy
	for _, c := range copies {
		if !c.IsDeletable() && c.State != dataset.CopyStateDeleted {
			blocking = append(blocking, c)
		}
	}
	if len(blocking) > 0 {
		return nil, &copiesNotDeletableError{datasetURI: req.datasetURI, blocking: blocking}
	}

	var deployed []*dataset.Grant
	var names []string
	for _, c := range copies {
		// Read defensively. A purge withdraws every grant of the dataset before it
		// schedules the key, so a grant still deployed here means something went wrong
		// halfway through one -- which is exactly when deleting the objects underneath it
		// would be worst.
		grants, err := d.grants.GrantsOfCopy(ctx, c)
		if err != nil {
			return nil, err
		}
		for _, grant := range grants {
			if grant.IsDeployed() {
				deployed = append(deployed, grant)
			}
		}

		if c.State != dataset.CopyStateDeleted {
			plan.record = append(plan.record, c)
		}
		if !slices.Contains(names, c.HotCacheBucket) {
			names = append(names, c.HotCacheBucket)
		}
	}
	if len(deployed) > 0 {
		return nil, &accessStillDeployedError{datasetURI: req.datasetURI, grants: deployed}
	}

	for _, name := range names {
		bucket, err := d.listBucket(ctx, name)
		if err != nil {
			return nil, err
		}
		plan.buckets = append(plan.buckets, bucket)
	}

	return plan, nil
}

// listBucket reads every object in a cache bucket: what the report quotes, and what the
// deletion then works through.
//
// The whole bucket is listed rather than the dataset's own prefix within it. The bucket
// holds one dataset and the artefacts of copying it and nothing else, so anything in it
// is part of what is being deleted, and a prefix would leave behind exactly the objects
// whose keys this tool cannot predict.
//
// Pagination is driven to exhaustion. A partial listing would understate what is about to
// go, and the number an operator sees before they confirm is the point of showing them
// anything at all.
func (d *deleter) listBucket(ctx context.Context, name string) (*deleteBucket, error) {
	bucket := &deleteBucket{name: name}

	var token *string
	for {
		page, err := d.objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(name),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"listing the objects in %s, so nothing was deleted and every copy of the dataset "+
					"is exactly as it was: %w", name, err)
		}

		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if key == "" {
				continue
			}
			bucket.keys = append(bucket.keys, key)
			bucket.totalBytes += aws.ToInt64(object.Size)
		}

		token = page.NextContinuationToken
		if !aws.ToBool(page.IsTruncated) || aws.ToString(token) == "" {
			return bucket, nil
		}
	}
}

// execute carries the plan out: the objects first, then the record of them having gone.
//
// That order is the whole of the recovery story. A failure between the two leaves copies
// recorded as purged with some of their objects deleted, which is honest -- purged never
// said the objects were there -- and re-running finishes the job. The other order would
// record a deletion that had not happened, and nothing afterwards would go looking.
func (d *deleter) execute(ctx context.Context, cmd *cobra.Command, plan *deletePlan) error {
	fmt.Fprintln(cmd.OutOrStdout())
	if err := d.emptyBuckets(ctx, cmd, plan); err != nil {
		return err
	}
	return d.recordDeletion(ctx, cmd, plan)
}

// emptyBuckets deletes every object listed, in batches of the size the API accepts.
//
// There is no point of no return to arrive at: each batch takes effect as it is sent, and
// from the first one the copy is incomplete. So progress is reported per bucket, and a
// failure says how far it got rather than only that it stopped.
//
// The per-object failures a batch reports are read out of the response body. A request
// that refused half its keys still returns successfully, so a caller checking only the
// error would go on to record a deletion of objects that are still there.
func (d *deleter) emptyBuckets(ctx context.Context, cmd *cobra.Command, plan *deletePlan) error {
	out := cmd.OutOrStdout()

	for _, bucket := range plan.buckets {
		deleted := 0

		for batch := range slices.Chunk(bucket.keys, deleteBatchSize) {
			objects := make([]s3types.ObjectIdentifier, 0, len(batch))
			for _, key := range batch {
				objects = append(objects, s3types.ObjectIdentifier{Key: aws.String(key)})
			}

			// Quiet, because the response would otherwise name every one of thousands of
			// keys back at us to say what was asked for. The refusals come back either
			// way, and they are the only part of the answer worth reading.
			response, err := d.objects.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket.name),
				Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
			})
			if err != nil {
				return &objectsNotDeletedError{
					datasetURI: plan.datasetURI,
					bucket:     bucket.name,
					deleted:    deleted,
					cause:      err,
				}
			}

			deleted += len(batch) - len(response.Errors)
			if len(response.Errors) > 0 {
				return &objectsNotDeletedError{
					datasetURI: plan.datasetURI,
					bucket:     bucket.name,
					deleted:    deleted,
					refused:    describeDeleteFailures(response.Errors),
				}
			}
		}

		fmt.Fprintf(out, "Bucket emptied: %s, %d object(s) deleted, %d byte(s) reclaimed\n",
			bucket.name, deleted, bucket.totalBytes)
	}
	return nil
}

// describeDeleteFailures renders the per-object refusals a batch reported, one line each.
func describeDeleteFailures(failures []s3types.Error) []string {
	described := make([]string, 0, len(failures))
	for _, failure := range failures {
		described = append(described, fmt.Sprintf("%s (%s: %s)",
			valueOrAbsent(aws.ToString(failure.Key)),
			valueOrAbsent(aws.ToString(failure.Code)),
			valueOrAbsent(aws.ToString(failure.Message))))
	}
	return described
}

// recordDeletion writes the deletion down against every copy whose objects have gone.
//
// It runs last because it is the only reversible part: the objects are already deleted by
// now, so a record that could not be written is a reporting failure rather than a
// discrepancy in what exists. It is still worth reporting loudly, since until it is put
// right a listing describes copies as holding objects that are not there.
func (d *deleter) recordDeletion(ctx context.Context, cmd *cobra.Command, plan *deletePlan) error {
	var unrecorded []string
	deleted := make([]*dataset.Copy, 0, len(plan.record))

	for _, c := range plan.record {
		recorded, err := d.copies.MarkDeleted(ctx, c.CopyID)
		if err != nil {
			unrecorded = append(unrecorded, fmt.Sprintf("%s (%v)", c.CopyID, err))
			continue
		}
		deleted = append(deleted, recorded)
	}

	reportDeletion(cmd.OutOrStdout(), plan, deleted)

	if len(unrecorded) > 0 {
		return fmt.Errorf(
			"the objects of dataset %q have been deleted, but %d of its copies could not be updated "+
				"to record that: %s. Run the same command again to record them; the bucket is empty "+
				"and deleting an object that is not there succeeds, so nothing further goes",
			plan.datasetURI, len(unrecorded), strings.Join(unrecorded, "; "))
	}
	return nil
}

// reportDeletePlan describes everything that is about to be deleted, before anything is
// asked and before anything is done.
//
// It leads with the objects and their weight, because reclaiming that weight is the only
// thing this buys, and it lists every copy of the dataset, because the objects belong to
// the bucket they share rather than to any one of them.
func reportDeletePlan(out io.Writer, plan *deletePlan) {
	fmt.Fprintf(out, "Deleting dataset %s empties the bucket its copy was staged into.\n\n",
		plan.datasetURI)

	// Named for what is about to go rather than what has gone: this is printed before the
	// confirmation, so an operator reading "deleted" would be reading about objects that
	// are still there and that they may yet decide to keep.
	fmt.Fprintf(out, "To delete (%d object(s), %d byte(s)):\n", plan.objectCount(), plan.totalBytes())
	for _, bucket := range plan.buckets {
		fmt.Fprintf(out, "  Bucket:  %s (%d object(s), %d byte(s))\n",
			bucket.name, len(bucket.keys), bucket.totalBytes)
	}

	fmt.Fprintf(out, "\nTo record deleted (%d of %d copy record(s)):\n", len(plan.record), len(plan.copies))
	renderListing(out, copyRows(plan.copies))

	fmt.Fprintf(out, `
This reclaims storage and nothing else. Access to this dataset ended when it
was purged -- a key pending deletion decrypts nothing -- so what goes here is
ciphertext nobody could read, which the bucket's lifecycle rule would have
expired on its own before long. If the intent is to stop somebody reading, this
is the wrong command.

There is no window on it. A scheduled key deletion can be cancelled while it
runs, and cancelling it makes a purged copy readable again; deleting the
objects takes that away too, and they are gone the moment the deletion is made.

The bucket stays where it is and the copy in the external source store is
untouched, so the dataset can be copied in again from scratch.
`)
}

// confirmDelete asks the operator to confirm by naming the dataset they are emptying.
//
// Naming it, rather than answering yes, is what purge asks for and for the same reason:
// it costs a copy and paste, and it will not be given by accident to a prompt that
// appeared where the operator did not expect one. This one has even less of a way back
// than a purge does, since a purge can be cancelled for as long as its window runs.
//
// Anything else declines, and so does an invocation with nothing on its input at all. A
// script that meant to delete something says so with the flag instead.
func confirmDelete(out io.Writer, in io.Reader, plan *deletePlan) (bool, error) {
	fmt.Fprintf(out, "\nTo delete these objects, type the dataset URI (%s). Anything else abandons "+
		"the deletion: ", plan.datasetURI)

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading the confirmation, so nothing was deleted: %w", err)
	}
	fmt.Fprintln(out)

	return strings.TrimSpace(line) == plan.datasetURI, nil
}

// reportSettledDelete describes a dataset whose objects are already gone, which is what a
// repeated invocation finds. Nothing is deleted a second time.
func reportSettledDelete(out io.Writer, plan *deletePlan) {
	fmt.Fprintf(out, "Dataset %s has already had its objects deleted; nothing was deleted.\n\n",
		plan.datasetURI)

	for _, bucket := range plan.buckets {
		fmt.Fprintf(out, "  Bucket:  %s (empty)\n", bucket.name)
	}

	fmt.Fprintf(out, "\nRecords (%d copy record(s)):\n", len(plan.copies))
	renderListing(out, copyRows(plan.copies))
	fmt.Fprintf(out, "\nThe bucket is still deployed and the copy in the external source store is "+
		"unaffected, so the dataset can be staged into it again.\n")
}

// reportDeletion describes what was deleted, taking the copies from the records as they
// now stand rather than from the plan, so that what is printed is what was stored.
func reportDeletion(out io.Writer, plan *deletePlan, deleted []*dataset.Copy) {
	fmt.Fprintf(out, "\nObjects deleted:\n")
	fmt.Fprintf(out, "  Dataset:  %s\n", plan.datasetURI)
	for _, bucket := range plan.buckets {
		fmt.Fprintf(out, "  Bucket:   %s, now empty (%d object(s), %d byte(s) reclaimed)\n",
			bucket.name, len(bucket.keys), bucket.totalBytes)
	}
	for _, c := range deleted {
		when := absentValue
		if c.DeletedAt != nil {
			when = c.DeletedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(out, "  Copy:     %s now %s, recorded %s\n", c.CopyID, c.State, when)
	}

	fmt.Fprintf(out, "\nThe storage is reclaimed and the objects are unrecoverable. The bucket is "+
		"still deployed, so staging this dataset again copies it back into the same place; the "+
		"copy in the external source store was never touched.\n")
}

// nothingToDeleteError reports a dataset that has no copy in this account, and so no
// objects here to delete. Nothing was read beyond the ledger and nothing was deleted.
type nothingToDeleteError struct {
	datasetURI string
}

func (e *nothingToDeleteError) Error() string {
	return fmt.Sprintf(
		"dataset %q has never been copied into this account, so there are no objects here to "+
			"delete; no bucket was listed and nothing was deleted. Check the URI against the one the "+
			"dataset was copied under -- this is keyed on the dataset, and a URI spelled differently "+
			"names a different one",
		e.datasetURI)
}

// copiesNotDeletableError reports a dataset holding at least one copy that somebody could
// still be reading. Nothing was listed and nothing was deleted.
//
// The objects belong to the bucket rather than to any one copy, so a single copy in the
// wrong state is enough to refuse the whole dataset: emptying the bucket would take the
// data away from whatever is reading through that copy.
type copiesNotDeletableError struct {
	datasetURI string
	// blocking are the copies standing in the way, newest first. All of them are named,
	// so an operator putting the dataset in order does it in one pass.
	blocking []*dataset.Copy
}

func (e *copiesNotDeletableError) Error() string {
	described := make([]string, 0, len(e.blocking))
	for _, c := range e.blocking {
		described = append(described, fmt.Sprintf("%s is %s", c.CopyID, c.State))
	}

	return fmt.Sprintf(
		"the objects of dataset %q cannot be deleted while %d of its copies could still be read: "+
			"%s. They share one bucket, so emptying it would take the data out from under whoever "+
			"is reading through those copies. Purge the dataset first -- that is what stops the "+
			"reading -- and delete its objects afterwards. Nothing was deleted",
		e.datasetURI, len(e.blocking), strings.Join(described, ", "))
}

// accessStillDeployedError reports a dataset whose documents still grant somebody access.
// Nothing was deleted.
//
// It should not arise: a purge takes every grant out of the deployed documents before it
// schedules the key, and every copy here has been purged or never delivered. So this is
// the backstop for a purge that failed partway, and finding one is a reason to look at
// what else it left behind rather than to press on.
type accessStillDeployedError struct {
	datasetURI string
	grants     []*dataset.Grant
}

func (e *accessStillDeployedError) Error() string {
	described := make([]string, 0, len(e.grants))
	for _, grant := range e.grants {
		described = append(described, fmt.Sprintf("%s to account %s",
			grant.GrantID, valueOrAbsent(grant.ResearcherAccountID)))
	}

	return fmt.Sprintf(
		"the objects of dataset %q were not deleted: %d grant(s) of it are still in the deployed "+
			"documents (%s). A purge withdraws every grant before it schedules the key, so a grant "+
			"still deployed means one did not finish; withdraw them, or purge the dataset again, "+
			"before deleting anything",
		e.datasetURI, len(e.grants), strings.Join(described, ", "))
}

// deleteDeclinedError reports a deletion the operator abandoned. It is an error rather
// than a quiet return so that the exit status distinguishes a bucket that was emptied from
// one that was left alone, which a script cannot otherwise tell.
type deleteDeclinedError struct {
	plan *deletePlan
}

func (e *deleteDeclinedError) Error() string {
	return fmt.Sprintf(
		"deletion of dataset %q was abandoned: its %d object(s) are still in %s and its %d copy "+
			"record(s) are exactly as they were",
		e.plan.datasetURI, e.plan.objectCount(),
		strings.Join(bucketNames(e.plan), ", "), len(e.plan.copies))
}

// bucketNames lists the buckets a plan covers, for a report that names them in a sentence.
func bucketNames(plan *deletePlan) []string {
	names := make([]string, 0, len(plan.buckets))
	for _, bucket := range plan.buckets {
		names = append(names, bucket.name)
	}
	return names
}

// objectsNotDeletedError reports a deletion that did not get through the bucket, either
// because a request failed outright or because the service refused keys inside one that
// otherwise succeeded.
//
// Whatever went is gone, and no copy was recorded as deleted: the ledger still describes
// them as purged, which is what they are, since purged never claimed the objects were
// there. Running the command again finishes the job, because deleting an object that is
// not there succeeds.
type objectsNotDeletedError struct {
	datasetURI string
	bucket     string
	// deleted is how many objects went before this failure, so the report says how far
	// the run got rather than only that it stopped.
	deleted int
	// refused are the per-object failures the service reported inside a response that
	// itself succeeded, and empty where the request failed outright.
	refused []string
	// cause is the failure of the request itself, and nil where the request succeeded and
	// refused keys within it.
	cause error
}

func (e *objectsNotDeletedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the objects of dataset %q were not all deleted from %s: ", e.datasetURI, e.bucket)
	if e.cause != nil {
		fmt.Fprintf(&b, "%v", e.cause)
	} else {
		fmt.Fprintf(&b, "the service refused %d of them (%s)",
			len(e.refused), strings.Join(firstFew(e.refused, maxReportedFailures), "; "))
	}
	fmt.Fprintf(&b, ". %d object(s) were deleted and are gone; the rest are still there. No copy was "+
		"recorded as deleted, so the ledger still reports them as purged, which is what they are. "+
		"Run the same command again to finish it: deleting an object that is not there succeeds, "+
		"so the ones already gone cost nothing to attempt again", e.deleted)
	return b.String()
}

func (e *objectsNotDeletedError) Unwrap() error {
	return e.cause
}

// firstFew keeps a report of many failures readable, counting whatever it does not name.
func firstFew(items []string, most int) []string {
	if len(items) <= most {
		return items
	}
	return append(slices.Clone(items[:most]), fmt.Sprintf("and %d more", len(items)-most))
}
