// This file is the side-effecting half of one copy of one dataset. Building the
// submission is done elsewhere and touches nothing; here the instruction document is
// uploaded, the copy execution is started, and the copy it is performing is recorded in
// the ledger.
//
// The cost ceiling needs stating plainly, because the copy service does not accept one.
// Its invocation has no cost argument and the state machine has no way to stop itself
// when a transfer turns out to be expensive. The estimate is produced inside a run,
// after every object has been headed, and is written per object into that run's own
// artefacts in the working bucket; the totals appear only in an HTML report, which is
// not something to parse. Gating on cost is therefore not one call but two runs: a dry
// run, which heads every object and prices the whole transfer without moving a byte, and
// then -- only if the estimate comes in under the ceiling -- a second, real run. Submit
// accordingly refuses to start a real copy under a ceiling it has not been handed an
// estimate for, rather than starting the transfer an operator asked to have gated.
//
// A dry run is also the reachability and writability check in its own right: the copy
// service confirms every source object exists and writes its own start marker into the
// destination, so both ends are exercised. It writes no dataset objects.

package dataset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/oklog/ulid/v2"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// instructionsContentType describes the JSONL instruction document. The copy service
// reads the bytes regardless; the type is set so the object is legible to anything else
// that finds it.
const instructionsContentType = "application/x-ndjson"

// copySetFileNames are the per-object documents the copy service writes once it has
// headed every object and sorted them by size and storage class. Each line carries the
// object's size and its cost estimate, which makes these four files the only
// machine-readable form of the estimate the run produces.
var copySetFileNames = []string{
	"small.jsonl",
	"large.jsonl",
	"smallThaw.jsonl",
	"largeThaw.jsonl",
}

// CopyRecorder is the slice of the ledger a submission needs: write the record for the
// copy that has just been started, under the identifier that copy is already running
// under. Deliberately one method, so that the store this is satisfied by can grow whatever
// else it needs without this file caring, and so that submitting cannot reach a grant.
type CopyRecorder interface {
	Record(ctx context.Context, c *Copy) error
}

// Submitter submits copies to the copy service and records what it started.
type Submitter struct {
	objects internalaws.S3Client
	copies  internalaws.StepFunctionsClient
	ledger  CopyRecorder
	cfg     CopyConfig

	// now is the clock, injectable so that a test can assert on a recorded timestamp.
	now func() time.Time
}

// NewSubmitter returns a Submitter. The recorder may be nil for a caller that only ever
// submits dry runs, since those record nothing.
func NewSubmitter(cfg CopyConfig, objects internalaws.S3Client, copies internalaws.StepFunctionsClient, ledger CopyRecorder) *Submitter {
	return &Submitter{
		objects: objects,
		copies:  copies,
		ledger:  ledger,
		cfg:     cfg,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SubmitRequest is one submission: which dataset, where it lands, and what limit applies
// to the transfer.
type SubmitRequest struct {
	// RunID identifies this copy's own folder in the working bucket. Optional: a
	// recorded copy is given a fresh identifier, which becomes the copy's identifier in
	// the ledger too, so a record is enough to find the copy's artefacts; a dry run is
	// given the dataset's estimate identifier, so the estimate can be found again without
	// the operator carrying anything between two invocations.
	RunID string

	// DatasetURI identifies the objects in the source store to copy.
	DatasetURI string

	// DestinationBucket is the cache bucket for this dataset, and KeyArn is the key
	// encrypting it. The key is recorded rather than used: nothing here encrypts
	// anything, but a copy that cannot name its key cannot later be purged.
	DestinationBucket string
	KeyArn            string

	// Objects optionally names the dataset's objects explicitly, leaving enumeration to
	// the copy service when empty.
	Objects []SourceObject

	// DryRun prices and checks the transfer without performing it. No copy is recorded,
	// because nothing has been staged.
	DryRun bool

	// MaxCostUSD is the most this transfer may be estimated to cost, in US dollars. Nil
	// means no ceiling. A ceiling requires Estimate, since the copy service accepts no
	// cost limit of its own and the estimate can only come from a preceding dry run.
	MaxCostUSD *float64

	// Estimate is the estimate the ceiling is checked against, as read back from a
	// finished dry run of this dataset.
	Estimate *CostEstimate
}

// Submission is what one submitted copy left behind.
type Submission struct {
	// RunID is the identifier the copy's artefacts sit under, and for a recorded copy
	// the identifier of the ledger record as well.
	RunID string

	// DatasetURI is the dataset that was submitted.
	DatasetURI string

	// Plan is the submission as it was built, including the instruction document that
	// was uploaded and the artefact locations the copy will write to.
	Plan *CopyPlan

	// WorkingPrefix is where in the working bucket this run's instructions and outputs
	// live.
	WorkingPrefix string

	// ExecutionArn and ExecutionName reference the started execution.
	ExecutionArn  string
	ExecutionName string

	// StartedAt is when the execution started, as the copy service reported it.
	StartedAt time.Time

	// Copy is the ledger record created for this transfer, and is nil for a dry run.
	Copy *Copy

	// DryRun records which kind of run this was.
	DryRun bool
}

// Submit uploads the instruction document, starts the copy execution, and records the
// copy that is now under way.
//
// The order is chosen for what it leaves behind when a step fails. Everything that can
// be checked is checked before the first write, so a malformed request or a blown cost
// ceiling costs nothing. The upload comes next, and doubles as the check that the
// working bucket is writable -- a stray instruction document is the worst a failure
// after it can leave. The execution is started before the record is written, because a
// record has to carry the execution it is waiting on: a record written first and never
// matched by an execution would claim a copy that does not exist, whereas an execution
// started and not recorded is a copy nobody is waiting on, into a bucket only we can
// read, granting nothing. The second is recoverable and is reported as such.
//
// A dry run records nothing. It stages no data, so a record in the copying state would
// be waiting for a transfer that will never deliver anything.
func (s *Submitter) Submit(ctx context.Context, req SubmitRequest) (*Submission, error) {
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		if req.DryRun {
			runID = EstimateRunID(req.DatasetURI)
		} else {
			runID = mintCopyID()
		}
	}

	plan, err := PlanCopy(s.cfg, CopyRequest{
		RunID:             runID,
		DatasetURI:        req.DatasetURI,
		DestinationBucket: req.DestinationBucket,
		Objects:           req.Objects,
		DryRun:            req.DryRun,
	})
	if err != nil {
		return nil, err
	}

	if err := checkCostCeiling(req); err != nil {
		return nil, err
	}

	// A recorded copy needs somewhere to record it, and finding that out after the
	// execution has started would leave exactly the orphan the ordering below exists to
	// avoid.
	if !req.DryRun && s.ledger == nil {
		return nil, fmt.Errorf(
			"cannot stage dataset %q: this submitter has no access to the ledger, so the "+
				"copy could not be recorded once started", req.DatasetURI)
	}

	if err := s.uploadInstructions(ctx, plan); err != nil {
		return nil, err
	}

	execution, err := s.startExecution(ctx, plan, runID, req.DryRun)
	if err != nil {
		return nil, err
	}

	submission := &Submission{
		RunID:         runID,
		DatasetURI:    req.DatasetURI,
		Plan:          plan,
		WorkingPrefix: s.runPrefix(runID),
		ExecutionArn:  execution.arn,
		ExecutionName: execution.name,
		StartedAt:     execution.startedAt,
		DryRun:        req.DryRun,
	}
	if req.DryRun {
		return submission, nil
	}

	recorded := &Copy{
		CopyID:           runID,
		DatasetURI:       req.DatasetURI,
		HotCacheBucket:   req.DestinationBucket,
		KeyArn:           strings.TrimSpace(req.KeyArn),
		State:            CopyStateCopying,
		CreatedAt:        execution.startedAt,
		CopyExecutionArn: execution.arn,
	}
	if err := s.ledger.Record(ctx, recorded); err != nil {
		return nil, &UnrecordedCopyError{
			DatasetURI:   req.DatasetURI,
			ExecutionArn: execution.arn,
			Cause:        err,
		}
	}

	submission.Copy = recorded
	return submission, nil
}

// uploadInstructions puts the instruction document where the copy service will read it.
// A failure here is reported against the working bucket, since that bucket belongs to
// the copy service's installation and a denial is far more likely to mean it was
// deployed elsewhere than that the document is wrong.
func (s *Submitter) uploadInstructions(ctx context.Context, plan *CopyPlan) error {
	_, err := s.objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(plan.InstructionsBucket),
		Key:         aws.String(plan.InstructionsKey),
		Body:        bytes.NewReader(plan.InstructionsDocument),
		ContentType: aws.String(instructionsContentType),
	})
	if err != nil {
		return CopyWorkingBucketPrerequisite(plan.InstructionsBucket, s.cfg.copyAccount()).
			Missing(fmt.Errorf("uploading copy instructions to %s: %w",
				s3URI(plan.InstructionsBucket, plan.InstructionsKey), err))
	}
	return nil
}

// startedExecution is what starting a copy told us about it.
type startedExecution struct {
	arn       string
	name      string
	startedAt time.Time
}

// startExecution starts the copy and reports what was started.
//
// The execution is named, and the name is what decides whether a resubmission can copy
// twice. A recorded copy takes the run identifier verbatim, so submitting the same run
// again cannot produce a second execution. A dry run takes a timestamped name instead,
// because an estimate is expected to be taken again -- against a dataset that may have
// grown since -- and reusing the name would silently hand back the previous run.
func (s *Submitter) startExecution(ctx context.Context, plan *CopyPlan, runID string, dryRun bool) (startedExecution, error) {
	input, err := plan.InvocationJSON()
	if err != nil {
		return startedExecution{}, err
	}

	name := runID
	if dryRun {
		name = runID + "-" + s.now().UTC().Format("20060102T150405Z")
	}

	started, err := s.copies.StartExecution(ctx, &sfn.StartExecutionInput{
		StateMachineArn: aws.String(plan.StateMachineARN),
		Name:            aws.String(name),
		Input:           aws.String(string(input)),
	})
	if err != nil {
		return startedExecution{}, CopyStateMachinePrerequisite(plan.StateMachineARN, s.cfg.copyAccount()).
			Missing(fmt.Errorf("starting the copy of dataset %q: %w", plan.DatasetURI, err))
	}

	startedAt := aws.ToTime(started.StartDate)
	if startedAt.IsZero() {
		startedAt = s.now().UTC()
	}
	return startedExecution{
		arn:       aws.ToString(started.ExecutionArn),
		name:      name,
		startedAt: startedAt.UTC(),
	}, nil
}

// runPrefix is where in the working bucket a run's instructions and outputs live.
func (s *Submitter) runPrefix(runID string) string {
	return terminatePrefix(s.cfg.WorkingBucketPrefix) + runID + "/"
}

// EstimateRunID returns the working-bucket run identifier an estimate of the named
// dataset is kept under.
//
// It is derived from the dataset URI so the estimate can be found by dataset alone: the
// dry run that produces an estimate and the real run it gates are separate invocations,
// and an operator should not have to carry an opaque identifier between them. The hash
// identifies rather than conceals -- the working bucket is ours, and a dataset URI has
// nothing in it to hide from ourselves.
//
// Estimating the same dataset again writes beneath the same prefix, which is intended:
// an estimate records nothing that has to be kept. Each run lands in its own folder
// there, so the reader takes the most recent and ignores the rest.
func EstimateRunID(datasetURI string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(datasetURI)))
	return "estimate-" + hex.EncodeToString(sum[:8])
}

// mintCopyID generates the identifier one copy is recorded, executed, and writes its
// artefacts under.
func mintCopyID() string {
	return ulid.Make().String()
}

// CostEstimate is what the copy service estimated one transfer would cost, totalled
// across every object it priced.
//
// The three components are the copy service's own division of the cost, kept separate
// because they behave differently: retrieval is charged for objects that have to be
// thawed first, cross-region transfer only when the two ends are in different regions,
// and compute is what the copying itself consumes. Every figure is an estimate the copy
// service describes as suitable for planning rather than billing, so a ceiling
// comparison against it is a judgement about the order of magnitude of a transfer, not
// an accounting control.
type CostEstimate struct {
	CrossRegionUSD          float64
	ColdStorageRetrievalUSD float64
	ComputeUSD              float64

	// ObjectCount and TotalBytes describe the transfer that was priced. They come from
	// the same documents as the cost, and are what a report needs to say how much data
	// the figure is for.
	ObjectCount int64
	TotalBytes  int64

	// ObservedAt is when the run that produced this estimate wrote it, so a caller can
	// judge whether it still describes the dataset.
	ObservedAt time.Time

	// Sources are the documents the totals were read from, for a report that has to show
	// its working.
	Sources []string
}

// TotalUSD is the whole estimated cost of the transfer.
func (e *CostEstimate) TotalUSD() float64 {
	if e == nil {
		return 0
	}
	return e.CrossRegionUSD + e.ColdStorageRetrievalUSD + e.ComputeUSD
}

// EstimatedCostForDataset reads back the estimate left by the most recent dry run of the
// named dataset.
func (s *Submitter) EstimatedCostForDataset(ctx context.Context, datasetURI string) (*CostEstimate, error) {
	return s.EstimatedCost(ctx, EstimateRunID(datasetURI))
}

// EstimatedCost totals the per-object estimates a run left in the working bucket.
//
// The estimate exists only once the run has headed every object and sorted them, which
// is well before it finishes copying but is not immediate. Until then there is nothing
// to read, and that is reported as such rather than as an estimate of zero -- a transfer
// that has not been priced must never look free to a ceiling check.
func (s *Submitter) EstimatedCost(ctx context.Context, runID string) (*CostEstimate, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("a run identifier is required to find an estimate")
	}
	prefix := s.runPrefix(runID)

	found, err := s.listCopySets(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, &EstimateUnavailableError{
			RunID:    runID,
			Location: s3URI(s.cfg.WorkingBucket, prefix),
		}
	}

	newest := newestRun(found)
	estimate := &CostEstimate{}
	for _, object := range newest {
		document, err := s.readObject(ctx, object.key)
		if err != nil {
			return nil, err
		}
		if err := addCopySetEstimate(estimate, object.key, document); err != nil {
			return nil, err
		}
		estimate.Sources = append(estimate.Sources, s3URI(s.cfg.WorkingBucket, object.key))
		if object.modified.After(estimate.ObservedAt) {
			estimate.ObservedAt = object.modified.UTC()
		}
	}
	if estimate.ObjectCount == 0 {
		return nil, &EstimateUnavailableError{
			RunID:    runID,
			Location: s3URI(s.cfg.WorkingBucket, prefix),
			Cause:    errors.New("the run priced no objects, so the dataset it was given may be empty"),
		}
	}
	return estimate, nil
}

// copySetObject is one located estimate document.
type copySetObject struct {
	key      string
	folder   string
	modified time.Time
}

// listCopySets finds the estimate documents beneath a run's prefix.
func (s *Submitter) listCopySets(ctx context.Context, prefix string) ([]copySetObject, error) {
	var found []copySetObject
	var token *string
	for {
		page, err := s.objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.WorkingBucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, CopyWorkingBucketPrerequisite(s.cfg.WorkingBucket, s.cfg.copyAccount()).
				Missing(fmt.Errorf("listing the copy's outputs beneath %s: %w",
					s3URI(s.cfg.WorkingBucket, prefix), err))
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if !isCopySetKey(key) {
				continue
			}
			found = append(found, copySetObject{
				key:      key,
				folder:   path.Dir(key),
				modified: aws.ToTime(object.LastModified),
			})
		}
		token = page.NextContinuationToken
		if !aws.ToBool(page.IsTruncated) || aws.ToString(token) == "" {
			return found, nil
		}
	}
}

// isCopySetKey reports whether a key is one of the estimate documents.
func isCopySetKey(key string) bool {
	base := path.Base(key)
	for _, name := range copySetFileNames {
		if base == name {
			return true
		}
	}
	return false
}

// newestRun narrows located documents to those of a single run, the most recently
// written one.
//
// Each run writes its four documents into a folder of its own, so grouping by folder
// groups by run. This matters because a dataset's estimates all share one prefix:
// without narrowing, a second estimate would be added to the first and the total would
// price the dataset twice.
func newestRun(found []copySetObject) []copySetObject {
	newestFolder := ""
	var newestAt time.Time
	for _, object := range found {
		switch {
		case newestFolder == "",
			object.modified.After(newestAt),
			object.modified.Equal(newestAt) && object.folder > newestFolder:
			newestFolder, newestAt = object.folder, object.modified
		}
	}

	var members []copySetObject
	for _, object := range found {
		if object.folder == newestFolder {
			members = append(members, object)
		}
	}
	return members
}

// readObject fetches one of the copy's own documents from the working bucket.
func (s *Submitter) readObject(ctx context.Context, key string) ([]byte, error) {
	output, err := s.objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.WorkingBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, CopyWorkingBucketPrerequisite(s.cfg.WorkingBucket, s.cfg.copyAccount()).
			Missing(fmt.Errorf("reading %s: %w", s3URI(s.cfg.WorkingBucket, key), err))
	}
	if output.Body == nil {
		return nil, nil
	}
	defer func() { _ = output.Body.Close() }()

	document, err := io.ReadAll(output.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s3URI(s.cfg.WorkingBucket, key), err)
	}
	return document, nil
}

// copySetEntry is one line of an estimate document: an object the run priced.
type copySetEntry struct {
	Size         int64 `json:"size"`
	CostEstimate struct {
		CrossRegionUSD          float64 `json:"crossRegionCostUSD"`
		ColdStorageRetrievalUSD float64 `json:"coldStorageRetrievalCostUSD"`
		ComputeUSD              float64 `json:"computeCostUSD"`
	} `json:"costEstimate"`
}

// addCopySetEstimate adds one document's objects to the running total. An empty document
// is normal: a run with nothing to thaw still writes a document for the objects it did
// not find.
func addCopySetEstimate(estimate *CostEstimate, key string, document []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	for line := 1; ; line++ {
		var entry copySetEntry
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading the estimate for object %d of %s: %w", line, key, err)
		}
		estimate.ObjectCount++
		estimate.TotalBytes += entry.Size
		estimate.CrossRegionUSD += entry.CostEstimate.CrossRegionUSD
		estimate.ColdStorageRetrievalUSD += entry.CostEstimate.ColdStorageRetrievalUSD
		estimate.ComputeUSD += entry.CostEstimate.ComputeUSD
	}
}

// checkCostCeiling applies the ceiling before anything has been written.
func checkCostCeiling(req SubmitRequest) error {
	if req.MaxCostUSD == nil {
		return nil
	}
	ceiling := *req.MaxCostUSD

	if ceiling < 0 {
		return fmt.Errorf(
			"a cost ceiling of %.2f USD cannot be met by any transfer; give a positive "+
				"figure, or none at all for an unlimited one", ceiling)
	}
	if req.DryRun {
		return errors.New(
			"a cost ceiling cannot be applied to a dry run: the dry run is what produces " +
				"the estimate, and it commits to nothing, so the ceiling belongs on the copy " +
				"that follows it")
	}
	if req.Estimate == nil {
		return &EstimateRequiredError{
			DatasetURI:    req.DatasetURI,
			CeilingUSD:    ceiling,
			EstimateRunID: EstimateRunID(req.DatasetURI),
		}
	}
	if total := req.Estimate.TotalUSD(); total > ceiling {
		return &CostCeilingExceededError{
			DatasetURI: req.DatasetURI,
			CeilingUSD: ceiling,
			Estimate:   req.Estimate,
		}
	}
	return nil
}

// EstimateRequiredError reports that a copy was submitted under a cost ceiling with no
// estimate to check it against.
type EstimateRequiredError struct {
	DatasetURI    string
	CeilingUSD    float64
	EstimateRunID string
}

func (e *EstimateRequiredError) Error() string {
	return fmt.Sprintf(
		"dataset %q was submitted with a ceiling of %.2f USD but no estimate to check "+
			"against it; the copy service accepts no cost limit of its own, so the estimate "+
			"has to come from a dry run of this dataset -- submit one, and once it has "+
			"priced the objects submit the copy again (the estimate is kept under run %q)",
		e.DatasetURI, e.CeilingUSD, e.EstimateRunID)
}

// CostCeilingExceededError reports that the transfer was estimated to cost more than the
// operator allowed. Nothing was uploaded and no copy was started.
type CostCeilingExceededError struct {
	DatasetURI string
	CeilingUSD float64
	Estimate   *CostEstimate
}

func (e *CostCeilingExceededError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "copying dataset %q is estimated to cost %.2f USD, above the %.2f USD allowed",
		e.DatasetURI, e.Estimate.TotalUSD(), e.CeilingUSD)
	if e.Estimate != nil {

		fmt.Fprintf(&b, " (%d objects, %d bytes: %.2f transfer, %.2f retrieval, %.2f compute)",
			e.Estimate.ObjectCount, e.Estimate.TotalBytes,
			e.Estimate.CrossRegionUSD, e.Estimate.ColdStorageRetrievalUSD, e.Estimate.ComputeUSD)
	}
	b.WriteString("; no copy was started")
	return b.String()
}

// EstimateUnavailableError reports that no estimate has been written for a run yet.
// Either it has not reached the point of pricing the objects, or no dry run of this
// dataset has been submitted at all.
type EstimateUnavailableError struct {
	RunID    string
	Location string
	Cause    error
}

func (e *EstimateUnavailableError) Error() string {
	message := fmt.Sprintf(
		"no cost estimate has been written for run %q; it is produced once a run has "+
			"headed every object in the dataset, and would appear beneath %s",
		e.RunID, e.Location)
	if e.Cause != nil {
		message += " (" + e.Cause.Error() + ")"
	}
	return message
}

func (e *EstimateUnavailableError) Unwrap() error {
	return e.Cause
}

// UnrecordedCopyError reports a copy that was started but could not be written to the
// ledger.
//
// It is separated from every other failure because of what it leaves running. The copy
// proceeds and writes into the dataset's cache bucket, which nobody outside this account
// can read and which no grant gives access to, so nothing is exposed -- but no record is
// waiting on it, so its outcome will be ignored when it reports.
type UnrecordedCopyError struct {
	DatasetURI   string
	ExecutionArn string
	Cause        error
}

func (e *UnrecordedCopyError) Error() string {
	return fmt.Sprintf(
		"the copy of dataset %q was started as execution %s but could not be recorded, so "+
			"nothing is waiting on it and its outcome will be ignored; the data it copies is "+
			"readable only within this account and is granted to nobody. Stop that execution, "+
			"or let it finish, and then stage the dataset again: %v",
		e.DatasetURI, e.ExecutionArn, e.Cause)
}

func (e *UnrecordedCopyError) Unwrap() error {
	return e.Cause
}
