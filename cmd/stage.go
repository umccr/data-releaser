package cmd

// Staging copies a dataset out of the external source store into the cache bucket that
// holds only that dataset, so it can later be granted to a researcher without the source
// store ever being exposed. It grants nothing itself.
//
// What it records is a copy: one transfer of one dataset into this account, carrying the
// provenance of the bytes and nothing about who may read them. Access is granted later, as
// records of its own made against the copy, and however many of those come and go the
// dataset is transferred once.
//
// Two things shape the command. The copy runs for hours to days and reports its outcome
// as an event, so this returns as soon as the copy has been started and recorded, and
// never waits for it. And the bucket and the key encrypting it are deployed out of band,
// one deployment per dataset — this only locates them, which means it has to arrive at
// the same bucket name that deployment did, from the same inputs.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
)

var stageCmd = &cobra.Command{
	Use:   "stage <mechanism>",
	Short: "Copy a dataset into this account so that it can be released",
	Long: `Copy a dataset into this account so that it can be released.

Staging grants nobody anything. It puts a copy of the data where a grant can
later point at it, and the copy runs for hours to days, so the command returns
once the copy has been started rather than waiting for it.

The mechanism is named as part of the invocation and is never inferred. Only the
mechanism that holds a copy of the data has anything to stage; the available
mechanisms are listed below.`,
}

var stageS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Copy a dataset out of the source store into its own cache bucket",
	Long: `Copy one dataset out of the external source store into the cache bucket
that holds only that dataset, and record the copy it starts.

The bucket and the key encrypting it are deployed separately, one deployment per
dataset, and both must exist before staging. Neither is created here: creating a
bucket and a key is not something a release command should do as a side effect.

The copy itself is performed by the separately deployed copy service. Its outcome
arrives as an event, which moves the copy from copying to staged or failed without
anything having to poll, so nothing can be granted until it reports.`,
	RunE: runStageS3,
}

func init() {
	stageS3Cmd.Flags().String("dataset-uri", "",
		"URI of the dataset to copy, naming a folder in the source store "+
			"(e.g. s3://source-bucket/datasets/cohort-a/)")
	stageS3Cmd.Flags().Bool("dry-run", false,
		"Check that every source object can be reached and that the destination can be "+
			"written, and price the transfer, without copying any data")
	stageS3Cmd.Flags().String("objects-from", "",
		"Path to a file naming the dataset's objects, one key per line, instead of leaving "+
			"enumeration to the copy service. Needed for a dataset larger than the copy "+
			"service's wildcard expansion limit, which refuses the whole run rather than "+
			"copying part of it. Produce one with, for example: aws s3api list-objects-v2 "+
			"--bucket B --prefix P/ --endpoint-url URL --query 'Contents[].Key' --output text "+
			"| tr '\\t' '\\n'")
	stageS3Cmd.Flags().Float64("max-cost", 0,
		"Refuse the copy if the transfer is estimated to cost more than this many US "+
			"dollars. Pricing the transfer takes a run of its own, so the first invocation "+
			"starts that run and the command is then run again")

	_ = stageS3Cmd.MarkFlagRequired("dataset-uri")

	stageCmd.AddCommand(stageS3Cmd)
	requireMechanism(stageCmd)
	rootCmd.AddCommand(stageCmd)
}

func runStageS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	datasetURI, err := cmd.Flags().GetString("dataset-uri")
	if err != nil {
		return fmt.Errorf("reading --dataset-uri flag: %w", err)
	}
	// Trimmed once, here, because the URI is used as an identity in three places that
	// have to agree: the bucket name derived from it, the ledger index the copies of one
	// dataset are found through, and the folder a priced run's estimate is kept under.
	datasetURI = strings.TrimSpace(datasetURI)
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("reading --dry-run flag: %w", err)
	}
	maxCost, err := stageCostCeiling(cmd, dryRun)
	if err != nil {
		return err
	}
	objects, err := stageObjectList(cmd, datasetURI)
	if err != nil {
		return err
	}

	// The URI decides which bucket is looked for and what the copy is pointed at, so a
	// malformed one is refused before any configuration is read or any call is made. It is
	// reduced to one form at the same time, and everything downstream -- the copy record,
	// the bucket name, the pinned-bucket lookup -- uses that form, so a URI written with or
	// without its scheme or its trailing slash names one dataset rather than several.
	canonical, err := dataset.CanonicalDatasetURI(datasetURI)
	if err != nil {
		return err
	}
	datasetURI = canonical

	cfg, err := requireConfig(
		config.SectionAccount,
		config.SectionLedger,
		config.SectionCopyService,
		config.SectionSourceStore,
		config.SectionHotCache,
	)
	if err != nil {
		return err
	}

	bucket, err := hotCacheBucketFor(cfg, datasetURI)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return err
	}

	stager := newStager(cfg, stageClients{
		ledger:  factory.DynamoDBClient(),
		objects: factory.S3Client(),
		keys:    factory.KMSClient(),
		copies:  factory.StepFunctionsClient(),
	})

	return stager.stage(ctx, cmd.OutOrStdout(), stageRequest{
		datasetURI:     datasetURI,
		hotCacheBucket: bucket,
		objects:        objects,
		dryRun:         dryRun,
		maxCostUSD:     maxCost,
	})
}

// stageCostCeiling reads the cost ceiling, refusing the combinations that cannot mean
// anything before any work is done for them.
//
// A ceiling on a priced run is the one worth catching early: the priced run is what
// produces the estimate, and asking for it to be gated on an estimate it is about to
// produce would otherwise start the run and then refuse it.
func stageCostCeiling(cmd *cobra.Command, dryRun bool) (*float64, error) {
	if !cmd.Flags().Changed("max-cost") {
		return nil, nil
	}

	ceiling, err := cmd.Flags().GetFloat64("max-cost")
	if err != nil {
		return nil, fmt.Errorf("reading --max-cost flag: %w", err)
	}
	if dryRun {
		return nil, errors.New(
			"--max-cost cannot be combined with --dry-run: the priced run is what produces the " +
				"estimate and it commits to nothing, so the ceiling belongs on the copy that " +
				"follows it")
	}
	if ceiling < 0 {
		return nil, fmt.Errorf(
			"--max-cost must not be negative: no transfer can be estimated to cost less than "+
				"%.2f USD, so the ceiling could never be met; omit the flag for an unlimited one",
			ceiling)
	}
	return &ceiling, nil
}

// stageObjectList reads the dataset's objects from the file the operator named, or
// returns nothing where they named none.
//
// The list is read and checked here, before any configuration is loaded or any call is
// made, because a list that does not describe this dataset is a mistake that costs
// nothing to catch and a great deal to discover halfway through a transfer.
func stageObjectList(cmd *cobra.Command, datasetURI string) ([]dataset.SourceObject, error) {
	path, err := cmd.Flags().GetString("objects-from")
	if err != nil {
		return nil, fmt.Errorf("reading --objects-from flag: %w", err)
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the object list for dataset %q: %w", datasetURI, err)
	}
	defer func() { _ = file.Close() }()

	objects, err := dataset.ParseObjectList(datasetURI, file)
	if err != nil {
		return nil, fmt.Errorf("the object list in %s does not describe dataset %q: %w",
			path, datasetURI, err)
	}
	return objects, nil
}

// stageClients are the services staging touches, gathered so that the command's own
// decisions can be exercised without reaching AWS.
type stageClients struct {
	ledger  internalaws.DynamoDBClient
	objects internalaws.S3Client
	keys    internalaws.KMSClient
	copies  internalaws.StepFunctionsClient
}

// stager stages one dataset: it checks that nothing already covers it, that the bucket and
// key it needs are there, and then starts the copy and records what it started.
type stager struct {
	accountID string
	copies    *dataset.CopyStore
	submitter *dataset.Submitter
	objects   internalaws.S3Client
	keys      internalaws.KMSClient
}

// newStager gathers what staging touches, with one copy ledger shared between the command
// and the submitter.
//
// The submitter is handed that ledger directly because what it records is a transfer and
// nothing else. It also fixes the identifier before the transfer begins -- the same value
// names the execution and the folder the copy's artefacts are written to -- so the store
// has to take the identifier it is given rather than minting one of its own, which is what
// its Record call is for as against its Create.
func newStager(cfg *config.Config, clients stageClients) *stager {
	copies := dataset.NewCopyStore(clients.ledger, cfg.Ledger.Table)
	return &stager{
		accountID: strings.TrimSpace(cfg.Account),
		copies:    copies,
		submitter: dataset.NewSubmitter(copyConfigFor(cfg), clients.objects, clients.copies, copies),
		objects:   clients.objects,
		keys:      clients.keys,
	}
}

// copyConfigFor gathers what a copy depends on from the configuration. Everything here is
// a property of a deployment this tool does not own — where the copy service was
// installed, where it keeps its working artefacts, how the source store is reached — so
// all of it is configuration rather than an assumption in code.
func copyConfigFor(cfg *config.Config) dataset.CopyConfig {
	return dataset.CopyConfig{
		AccountID:           strings.TrimSpace(cfg.Account),
		CopyAccountID:       strings.TrimSpace(cfg.S3.Copy.Account),
		Region:              strings.TrimSpace(cfg.Region),
		StateMachineARN:     strings.TrimSpace(cfg.S3.Copy.StateMachineArn),
		WorkingBucket:       strings.TrimSpace(cfg.S3.Copy.WorkingBucket),
		WorkingBucketPrefix: cfg.S3.Copy.WorkingBucketPrefix,
		Source: dataset.SourceStore{
			EndpointURL:       strings.TrimSpace(cfg.S3.Source.EndpointURL),
			Region:            strings.TrimSpace(cfg.S3.Source.Region),
			CredentialsSecret: strings.TrimSpace(cfg.S3.Source.CredentialsSecret),
		},
		Performance: dataset.CopyPerformance{
			HeadConcurrency:      cfg.S3.Copy.Performance.HeadConcurrency,
			SmallCopyConcurrency: cfg.S3.Copy.Performance.SmallCopyConcurrency,
			SmallCopyBatchSize:   cfg.S3.Copy.Performance.SmallCopyBatchSize,
			LargeCopyConcurrency: cfg.S3.Copy.Performance.LargeCopyConcurrency,
		},
	}
}

// stageRequest is one staging invocation.
type stageRequest struct {
	// datasetURI names the folder in the source store to copy.
	datasetURI string

	// hotCacheBucket is the bucket that holds this dataset and no other.
	hotCacheBucket string

	// objects names the dataset's objects explicitly. Empty leaves enumeration to the
	// copy service, which is the default and is bounded by what one wildcard may expand
	// to.
	objects []dataset.SourceObject

	// dryRun checks both ends and prices the transfer without moving any bytes.
	dryRun bool

	// maxCostUSD is the most the transfer may be estimated to cost. Nil is unlimited.
	maxCostUSD *float64
}

// stage starts the copy of one dataset and records it.
//
// Everything that can be read is read before anything is written: an existing copy of the
// dataset, then the bucket and the key. A dataset that is already staged costs nothing to
// discover, and a bucket or key that is absent is reported together with the deployment
// responsible for it rather than as whichever call happened to notice.
func (s *stager) stage(ctx context.Context, out io.Writer, req stageRequest) error {
	existing, err := s.copies.CopiesOfDataset(ctx, req.datasetURI)
	if err != nil {
		return err
	}
	// A priced run is exempt: what the existing copy saves is the hours and the money a
	// second transfer would spend, and a priced run spends neither. It moves no bytes
	// and is never recorded, so it leaves the copy it was refused on account of exactly
	// as it found it -- which makes it the way to check a staged dataset still prices,
	// still reads, and still reaches the copy service as intended.
	if covered := coveringCopy(existing); covered != nil && !req.dryRun {
		return &alreadyStagedError{datasetURI: req.datasetURI, copy: covered}
	}

	keyArn, err := s.locateDestination(ctx, req.datasetURI, req.hotCacheBucket)
	if err != nil {
		return err
	}

	submission := dataset.SubmitRequest{
		DatasetURI:        req.datasetURI,
		DestinationBucket: req.hotCacheBucket,
		KeyArn:            keyArn,
		Objects:           req.objects,
		DryRun:            req.dryRun,
		MaxCostUSD:        req.maxCostUSD,
	}

	if req.maxCostUSD != nil {
		estimate, err := s.priceTransfer(ctx, req, keyArn, *req.maxCostUSD)
		if err != nil {
			return err
		}
		submission.Estimate = estimate
	}

	submitted, err := s.submitter.Submit(ctx, submission)
	if err != nil {
		return err
	}

	reportSubmission(out, submitted, submission.Estimate)
	return nil
}

// locateDestination confirms that the bucket this dataset lands in and the key encrypting
// it are both deployed and usable, and returns the key's ARN.
//
// Both are reported together, because an operator who has not deployed a dataset's
// resources is missing both and should learn that from one run. The key's ARN is what
// comes back rather than the alias it was found by: a copy that cannot name its own key
// can never be purged, and an alias can be repointed at a different key.
func (s *stager) locateDestination(ctx context.Context, datasetURI, bucket string) (string, error) {
	report := &dataset.PrerequisiteReport{}

	if _, err := s.objects.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		report.Add(dataset.CacheBucketPrerequisite(bucket, s.accountID).Missing(fmt.Errorf(
			"looking for the bucket holding dataset %q. Its name is derived from the URI unless "+
				"it is pinned in configuration, so a URI spelled differently from the one the "+
				"deployment was given names a different bucket: %w", datasetURI, err)))
	}

	alias := hotCacheKeyAlias(bucket)
	keyArn := ""
	described, err := s.keys.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(alias)})
	prerequisite := dataset.DatasetKeyPrerequisite(alias, s.accountID)
	switch {
	case err != nil:
		report.Add(prerequisite.Missing(fmt.Errorf(
			"looking for the key encrypting %s: %w", bucket, err)))
	case described.KeyMetadata == nil || strings.TrimSpace(aws.ToString(described.KeyMetadata.Arn)) == "":
		report.Add(prerequisite.Missingf(
			"the alias resolved but named no key, and a copy that cannot name the key its data " +
				"is encrypted under can never be purged"))
	case described.KeyMetadata.KeyState != kmstypes.KeyStateEnabled:
		// Checked before the copy starts because the copy asks for a data key per object:
		// a key that cannot serve one fails the transfer partway through, after it may
		// already have run for hours.
		report.Add(prerequisite.Missingf(
			"the key is %s rather than enabled, so the copy would be refused the data keys it "+
				"needs to write into %s", described.KeyMetadata.KeyState, bucket))
	default:
		keyArn = aws.ToString(described.KeyMetadata.Arn)
	}

	if err := report.Err(); err != nil {
		return "", err
	}
	return keyArn, nil
}

// priceTransfer resolves the estimate a cost ceiling is checked against.
//
// The copy service takes no cost limit of its own, and produces its estimate inside a run
// after every object has been headed, so gating on cost is necessarily two invocations: a
// priced run that moves nothing, and then the copy. When no estimate exists yet the priced
// run is started here so the operator does not have to know that, and staging is refused —
// nothing has been staged, and a command that reported success for having priced a
// transfer would be indistinguishable from one that copied it.
func (s *stager) priceTransfer(ctx context.Context, req stageRequest, keyArn string, ceiling float64) (*dataset.CostEstimate, error) {
	estimate, err := s.submitter.EstimatedCostForDataset(ctx, req.datasetURI)
	if err == nil {
		return estimate, nil
	}

	var unavailable *dataset.EstimateUnavailableError
	if !errors.As(err, &unavailable) {
		return nil, err
	}

	// Priced against the same objects the copy will move. A run priced over a wildcard
	// and then copied from a list would estimate a different transfer from the one the
	// ceiling is meant to gate.
	priced, err := s.submitter.Submit(ctx, dataset.SubmitRequest{
		DatasetURI:        req.datasetURI,
		DestinationBucket: req.hotCacheBucket,
		KeyArn:            keyArn,
		Objects:           req.objects,
		DryRun:            true,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"dataset %q was not staged: a ceiling of %.2f USD needs the transfer priced first, "+
				"and the run that would have priced it could not be started: %w",
			req.datasetURI, ceiling, err)
	}

	return nil, fmt.Errorf(
		"dataset %q was not staged: a ceiling of %.2f USD was asked for, and the copy service "+
			"accepts no cost limit of its own, so the transfer has to be priced by a run of its "+
			"own first. That run has been started as %s and moves no data; once it has priced "+
			"every object, run the same command again and the estimate will be checked against "+
			"the ceiling. The estimate appears beneath %s",
		req.datasetURI, ceiling, priced.ExecutionArn, priced.WorkingPrefix)
}

// coveringCopy returns the copy that already accounts for this dataset, if one does.
//
// A transfer in flight and one that staged both count: either way the objects are in the
// bucket or on their way, so a second transfer would run for hours and be billed to
// deliver what is already there. A failed copy does not stand in the way, since it
// delivered nothing, and neither does a purged one, whose key is being destroyed and which
// therefore has to be transferred again to be readable at all.
//
// Whether anyone holds access to the covering copy makes no difference. Grants are records
// of their own, made and withdrawn against a copy that stays exactly where it is, so a copy
// nobody holds is as much a reason not to transfer the dataset again as one two
// researchers are reading.
func coveringCopy(copies []*dataset.Copy) *dataset.Copy {
	// Newest first, as the ledger returns them, so the most recent copy covering the
	// dataset is the one reported.
	for _, c := range copies {
		switch c.State {
		case dataset.CopyStateCopying, dataset.CopyStateStaged:
			return c
		}
	}
	return nil
}

// alreadyStagedError reports that the dataset is already accounted for, so no copy was
// started and nothing was written.
type alreadyStagedError struct {
	datasetURI string
	copy       *dataset.Copy
}

func (e *alreadyStagedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dataset %q is already covered by copy %s, which is %s",
		e.datasetURI, e.copy.CopyID, e.copy.State)

	if e.copy.State == dataset.CopyStateCopying {
		fmt.Fprintf(&b, ": a transfer of it is already running as %s, so no second copy was "+
			"started. Wait for that one to report, since another would spend hours transferring "+
			"the same objects", e.copy.CopyExecutionArn)
		return b.String()
	}
	fmt.Fprintf(&b, ": the data is already in %s, so no copy was started. Grant access to it as it "+
		"stands, or purge the dataset first if it has to be copied again", e.copy.HotCacheBucket)
	return b.String()
}

// reportSubmission describes what was started. A priced run and a real copy are reported
// differently on purpose: one has staged a dataset and the other has committed to nothing,
// and an operator reading the output afterwards should not have to work out which.
func reportSubmission(out io.Writer, submission *dataset.Submission, estimate *dataset.CostEstimate) {
	if submission.DryRun {
		fmt.Fprintf(out, "Priced run started; no data is being copied:\n")
		fmt.Fprintf(out, "  Dataset:   %s\n", submission.DatasetURI)
		fmt.Fprintf(out, "  Bucket:    %s\n", submission.Plan.Invocation.DestinationBucket)
		fmt.Fprintf(out, "  Execution: %s\n", submission.ExecutionArn)
		fmt.Fprintf(out, "  Estimate:  %s (written once every object has been priced)\n",
			submission.WorkingPrefix)
		fmt.Fprintf(out, "\nEvery source object is checked and the destination is written to, so the "+
			"run exercises both ends without staging anything.\n")
		return
	}

	// Described from the record the submission wrote rather than from the request, so the
	// report cannot say one thing while the ledger says another.
	recorded := submission.Copy
	if recorded == nil {
		fmt.Fprint(out, "Dataset staging started, but the submission reported no record of it.\n")
		return
	}

	fmt.Fprintf(out, "Dataset staging started:\n")
	fmt.Fprintf(out, "  Copy ID:      %s\n", recorded.CopyID)
	fmt.Fprintf(out, "  Dataset:      %s\n", submission.DatasetURI)
	fmt.Fprintf(out, "  Bucket:       %s\n", recorded.HotCacheBucket)
	fmt.Fprintf(out, "  Key:          %s\n", recorded.KeyArn)
	fmt.Fprintf(out, "  Execution:    %s\n", submission.ExecutionArn)
	fmt.Fprintf(out, "  Instructions: %s\n", submission.WorkingPrefix)
	fmt.Fprintf(out, "  State:        %s\n", recorded.State)
	if estimate != nil {
		fmt.Fprintf(out, "  Estimate:     %.2f USD for %d objects, %d bytes, priced %s\n",
			estimate.TotalUSD(), estimate.ObjectCount, estimate.TotalBytes,
			estimate.ObservedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(out, "\nThe copy runs for hours to days and reports its own outcome, which moves it "+
		"to staged or failed. Nothing is granted to anyone until it does.\n")
}

// Bucket-name derivation, mirroring what the per-dataset deployment does.
//
// The bucket is deployed out of band and this only locates it, so both sides have to
// arrive at the same name from the same inputs: the name prefix, the account, the region,
// and the dataset URI exactly as written. A dataset URI spelled differently here than in
// the deployment's own configuration derives a different name, and the tool would then
// look for a copied dataset in a bucket that holds nothing — which is why a bucket can
// be pinned by name in configuration, and why that is worth doing as soon as a bucket
// holds a copy someone is reading.
const (
	// maxDerivedBucketName is the longest name derivation produces. S3 permits 63
	// characters; the slack matches the deployment, which reserves it for the suffix on
	// the bucket receiving access logs.
	maxDerivedBucketName = 58

	// obscuredNameDigestChars and legibleNameDigestChars are how much of each digest is
	// kept. The obscured form is the whole of the name, so it carries more.
	obscuredNameDigestChars = 16
	legibleNameDigestChars  = 8
)

var (
	// datasetURISchemePattern matches the scheme a dataset URI may carry, which is
	// dropped before the rest is reduced to a bucket-name slug.
	datasetURISchemePattern = regexp.MustCompile(`^[a-zA-Z0-9+.-]+://`)

	// nonBucketNameRun matches any run of characters a bucket name cannot hold.
	nonBucketNameRun = regexp.MustCompile(`[^a-z0-9]+`)
)

// hotCacheBucketFor resolves the bucket a dataset is cached in, preferring a name pinned
// in configuration over a derived one.
func hotCacheBucketFor(cfg *config.Config, datasetURI string) (string, error) {
	if pinned := pinnedHotCacheBucket(cfg, datasetURI); pinned != "" {
		return pinned, nil
	}

	// Refuses rather than falling back to a legible name when the key is absent: the two
	// forms are different names, so a fallback would point at a bucket that holds nothing.
	hashKey, err := cfg.S3.HotCache.ResolveBucketNameHashKey()
	if err != nil {
		return "", err
	}

	return deriveHotCacheBucketName(hotCacheBucketNameInputs{
		namePrefix: cfg.S3.HotCache.NamePrefix,
		datasetURI: datasetURI,
		account:    strings.TrimSpace(cfg.Account),
		region:     strings.TrimSpace(cfg.Region),
		hashKey:    hashKey,
	})
}

// pinnedHotCacheBucket returns the bucket a dataset is pinned to in configuration, or the
// empty string where it is pinned to none.
//
// Both sides of the comparison are reduced to one form first. The configured keys are
// written by hand, so one of them differing from the dataset URI only by a trailing slash
// would otherwise miss the pin and fall through to derivation -- naming a bucket that was
// never deployed, for a dataset whose bucket is sitting right there.
func pinnedHotCacheBucket(cfg *config.Config, datasetURI string) string {
	wanted, err := dataset.CanonicalDatasetURI(datasetURI)
	if err != nil {
		return ""
	}
	for configured, bucket := range cfg.S3.HotCache.Buckets {
		canonical, err := dataset.CanonicalDatasetURI(configured)
		if err != nil {
			// Reported by the configuration check for this section rather than here,
			// where returning no pin is the same answer as having none.
			continue
		}
		if canonical == wanted {
			return strings.TrimSpace(bucket)
		}
	}
	return ""
}

// hotCacheBucketNameInputs are everything a derived bucket name depends on.
type hotCacheBucketNameInputs struct {
	namePrefix string
	datasetURI string
	account    string
	region     string
	// hashKey derives an obscured name, and is empty when a legible one was asked for.
	hashKey string
}

// deriveHotCacheBucketName derives a dataset's bucket name.
//
// With a hash key the name is the prefix and a keyed digest, which discloses nothing about
// what the bucket holds. Without one it is a legible slug of the dataset URI with a short
// digest appended, so that two URIs whose slugs truncate to the same thing still get
// distinct names. Either digest covers the account and region as well as the URI, so the
// same dataset in another account or region is a different bucket.
func deriveHotCacheBucketName(in hotCacheBucketNameInputs) (string, error) {
	scoped := in.account + "/" + in.region + "/" + strings.TrimSpace(in.datasetURI)
	head := in.namePrefix + "-dataset-"

	if in.hashKey != "" {
		mac := hmac.New(sha256.New, []byte(in.hashKey))
		mac.Write([]byte(scoped))
		return head + hex.EncodeToString(mac.Sum(nil))[:obscuredNameDigestChars], nil
	}

	sum := sha256.Sum256([]byte(scoped))
	tail := "-" + hex.EncodeToString(sum[:])[:legibleNameDigestChars]

	room := maxDerivedBucketName - len(head) - len(tail)
	if room <= 0 {
		return "", fmt.Errorf(
			"the configured cache bucket name prefix %q leaves no room for a derived name within "+
				"%d characters; shorten it, or pin this dataset's bucket by name in configuration",
			in.namePrefix, maxDerivedBucketName)
	}

	slug := slugifyDatasetURI(in.datasetURI)
	if len(slug) > room {
		slug = slug[:room]
	}
	if slug == "" {
		slug = "unnamed"
	}
	return head + slug + tail, nil
}

// slugifyDatasetURI reduces a dataset URI to the characters a bucket name may hold.
func slugifyDatasetURI(datasetURI string) string {
	slug := datasetURISchemePattern.ReplaceAllString(strings.TrimSpace(datasetURI), "")
	slug = nonBucketNameRun.ReplaceAllString(strings.ToLower(slug), "-")
	return strings.Trim(slug, "-")
}

// hotCacheKeyAlias is the alias the per-dataset deployment gives the key encrypting one
// cache bucket. Naming the key by alias is what lets the key be found from the bucket
// alone, without the deployment having to hand its identifier over.
func hotCacheKeyAlias(bucket string) string {
	return "alias/" + bucket
}
