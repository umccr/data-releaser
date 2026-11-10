package dataset

// This file closes the loop on a copy. Submitting one starts an execution that runs for
// hours or days, so nothing waits on it: the copy service announces its own terminal
// state on the event bus, and what arrives here is that announcement. The copy record is
// moved to staged or failed, and whatever the copy reported about what it moved is
// recorded alongside.
//
// Only the copy is touched. Whether anyone may read what arrived is a separate record
// made later against a copy that staged, so an outcome written here grants nothing and
// withdraws nothing -- which is what lets this run unattended in a Lambda.
//
// # How an event is matched to a copy
//
// By the execution's name. The ledger has no index on the execution reference, and adding
// one to answer a question asked once per copy would be an index maintained on every
// write for the benefit of a single read. It is not needed, because the identifier a copy
// is recorded under is also the name its execution runs under -- the submitter mints the
// identifier first and names the run after it. So the name is the copy identifier
// verbatim, with nothing derived and nothing searched.
//
// The execution reference recorded when the copy was submitted is then checked against
// the event, which is what stops an outcome being attributed to a copy that was performed
// by a different execution.
//
// An estimate run needs no special handling despite sharing the state machine: it is
// named after the dataset rather than after a copy, so it resolves to no record and is
// reported as an execution nothing is waiting on -- which is exactly what it is, since an
// estimate transfers nothing.
//
// # Why the summary is read from the working bucket and not the cache bucket
//
// The copy writes its account of the run twice: into the dataset's cache bucket, and into
// its own working bucket. The two are identical, and only the second is read here.
//
// The copy in the cache bucket is encrypted with that dataset's key. Reading it would
// mean this handler holding decrypt permission on every dataset key that will ever exist
// -- a grant that has to be extended each time a dataset is added, on the one component
// whose whole purpose is to write six numbers into a table. The copy in the working
// bucket is encrypted with nothing of the sort, so reading it needs one permission on one
// bucket, fixed for the life of the deployment. It is also the copy that survives a purge
// of the dataset, which is the reason the submitter asks for it in the first place.
//
// Where that copy staged is not computed here either. The copy service reports the bucket
// and key it wrote in its own output, so the location recorded in the ledger is the one
// the copy service named rather than one this code reconstructed from the layout it
// expects. A reconstruction would drift silently the moment either side changed.
//
// # What a failed copy records
//
// The state, and nothing else. A run that did not succeed never reached the step that
// summarises it, so there is no account of the run to read and no location to point at;
// an execution that Step Functions did not complete carries no output at all. What an
// operator has is the failed state, which refuses to be granted, and the execution
// reference recorded when the copy was submitted.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// ExecutionStatus is where a copy execution got to, in the copy service's own spelling.
type ExecutionStatus string

const (
	// ExecutionRunning and ExecutionPendingRedrive are the states a copy passes
	// through. Both are announced on the event bus and neither says anything about an
	// outcome.
	ExecutionRunning        ExecutionStatus = "RUNNING"
	ExecutionPendingRedrive ExecutionStatus = "PENDING_REDRIVE"

	// ExecutionSucceeded means every object was transferred and the run summarised
	// itself.
	ExecutionSucceeded ExecutionStatus = "SUCCEEDED"

	// ExecutionFailed, ExecutionTimedOut, and ExecutionAborted are the three ways a copy
	// ends without delivering the dataset. They are distinct to the copy service and
	// identical here: none of them staged anything.
	ExecutionFailed   ExecutionStatus = "FAILED"
	ExecutionTimedOut ExecutionStatus = "TIMED_OUT"
	ExecutionAborted  ExecutionStatus = "ABORTED"
)

// terminalStatuses are the statuses a copy does not move on from.
var terminalStatuses = []ExecutionStatus{
	ExecutionSucceeded,
	ExecutionFailed,
	ExecutionTimedOut,
	ExecutionAborted,
}

// IsTerminal reports whether the copy has finished, one way or the other.
func (s ExecutionStatus) IsTerminal() bool {
	return slices.Contains(terminalStatuses, s)
}

// Delivered reports whether the copy put the dataset in the cache bucket. Only a success
// did; every other terminal status leaves the copy unable to be granted at all.
func (s ExecutionStatus) Delivered() bool {
	return s == ExecutionSucceeded
}

func (s ExecutionStatus) String() string {
	return string(s)
}

// CopyExecutionEvent is one copy execution's status change, as the event bus delivers it.
// It is the detail of the event rather than the whole envelope, since the envelope says
// nothing this needs.
type CopyExecutionEvent struct {
	// ExecutionArn identifies the execution that changed state.
	ExecutionArn string `json:"executionArn"`

	// StateMachineArn identifies the copy state machine it ran on.
	StateMachineArn string `json:"stateMachineArn"`

	// Name is the execution's name, which for a recorded copy is that copy's own
	// identifier.
	Name string `json:"name"`

	// Status is where the execution got to.
	Status ExecutionStatus `json:"status"`

	// Output is what the execution returned, present only for one that succeeded. It is
	// a pointer because the event carries an explicit null for the executions that have
	// none.
	Output *string `json:"output"`

	// OutputDetails says whether Output is the whole of it. The event bus drops a
	// payload above its own size limit and says so here rather than delivering a
	// truncated document.
	OutputDetails struct {
		Included bool `json:"included"`
	} `json:"outputDetails"`

	// Error and Cause are what a failed execution reported, and are absent from one that
	// succeeded. They are the only account of why a copy failed that reaches this code:
	// the per-object detail is written into the working bucket, and the execution's own
	// history does not persist indefinitely.
	Error string `json:"error"`
	Cause string `json:"cause"`
}

// ParseCopyExecutionEvent reads the detail of a copy execution status change.
//
// The three fields it insists on are the ones an event is useless without: without a
// status there is nothing to record, and without a name and an execution reference there
// is no way to tell which copy it concerns.
func ParseCopyExecutionEvent(detail []byte) (CopyExecutionEvent, error) {
	var event CopyExecutionEvent
	if err := json.Unmarshal(detail, &event); err != nil {
		return CopyExecutionEvent{}, fmt.Errorf("reading copy execution event: %w", err)
	}

	var missing []string
	for _, required := range []struct {
		field string
		value string
	}{
		{"executionArn", event.ExecutionArn},
		{"name", event.Name},
		{"status", string(event.Status)},
	} {
		if strings.TrimSpace(required.value) == "" {
			missing = append(missing, required.field)
		}
	}
	if len(missing) > 0 {
		return CopyExecutionEvent{}, fmt.Errorf(
			"copy execution event names no %s, so there is no way to tell which copy it "+
				"reports on", strings.Join(missing, " and no "))
	}

	event.ExecutionArn = strings.TrimSpace(event.ExecutionArn)
	event.StateMachineArn = strings.TrimSpace(event.StateMachineArn)
	event.Name = strings.TrimSpace(event.Name)
	return event, nil
}

// copyOutputDocument is what the copy service's summarising step returns, and so what a
// succeeded execution's output holds. Only the two locations of its own summary are read;
// the rest of the document describes the report, which is ours to look at and not
// something to record.
type copyOutputDocument struct {
	// DestinationBucket is the dataset's cache bucket, and CsvKey the summary written
	// into it, encrypted with that dataset's key.
	DestinationBucket string `json:"destinationBucket"`
	CsvKey            string `json:"csvKey"`

	// WorkingBucket and WorkingCsvKey are the retained copy of that same summary, in
	// the copy service's own bucket. This is the one that is read.
	WorkingBucket string `json:"workingBucket"`
	WorkingCsvKey string `json:"workingCsvKey"`
}

// CopyLedger is the slice of the ledger recording a copy's outcome needs: find the copy
// the execution was performing, and move it to whichever state the execution reached.
//
// It is the three calls and no more, so that the component whose whole purpose is to
// write six numbers into a table cannot reach anything else in the ledger -- a grant
// least of all.
type CopyLedger interface {
	Get(ctx context.Context, copyID string) (*Copy, error)
	MarkStaged(ctx context.Context, copyID string, outcome CopyOutcome) (*Copy, error)
	MarkFailed(ctx context.Context, copyID string, outcome CopyOutcome) (*Copy, error)
}

// CopyCompletionHandler records the outcome of a finished execution against the copy it
// was performing.
type CopyCompletionHandler struct {
	ledger  CopyLedger
	copies  internalaws.StepFunctionsClient
	objects internalaws.S3Client
}

// NewCopyCompletionHandler returns a handler writing to the given ledger.
//
// The copy client is used only when an event arrives without the execution's output, and
// the object client only to read the copy's summary of itself. Either may be nil for a
// caller that accepts the outcome being recorded without those figures.
func NewCopyCompletionHandler(
	ledger CopyLedger,
	copies internalaws.StepFunctionsClient,
	objects internalaws.S3Client,
) *CopyCompletionHandler {
	return &CopyCompletionHandler{ledger: ledger, copies: copies, objects: objects}
}

// CopyCompletion reports what one status change led to.
type CopyCompletion struct {
	// ExecutionArn and Status are the copy the event reported on.
	ExecutionArn string
	Status       ExecutionStatus

	// CopyID is the copy the event was matched to, empty when it matched none.
	CopyID string

	// Recorded is what was written about what the copy moved.
	Recorded CopyOutcome

	// Copy is the record as it stands after the write, and is nil when nothing was
	// written.
	Copy *Copy

	// Skipped says why nothing was written, and is empty when something was. Every
	// reason it holds is a settled one -- an execution no copy is recorded under, a copy
	// that has already reported -- so a skipped event is finished with rather than
	// deferred.
	Skipped string

	// Shortfall says why the recorded figures are less than a full account of the copy:
	// the summary could not be read, or it reported objects the copy could not move. It
	// is nil when the account is complete, and never a reason to withhold the state.
	Shortfall error
}

// Written reports whether the completion was written to the ledger.
func (c *CopyCompletion) Written() bool {
	return c.Copy != nil
}

// String renders the completion as one line, for a caller that logs it.
func (c *CopyCompletion) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "copy %s reported %s", c.ExecutionArn, c.Status)
	if c.CopyID != "" {
		fmt.Fprintf(&b, " for copy %q", c.CopyID)
	}
	switch {
	case c.Skipped != "":
		fmt.Fprintf(&b, ": nothing to do, %s", c.Skipped)
	case c.Copy != nil:
		fmt.Fprintf(&b, ": recorded as %s with %d objects and %d bytes",
			c.Copy.State, c.Recorded.ObjectCount, c.Recorded.TotalBytes)
		if c.Recorded.SummaryLocation != "" {
			fmt.Fprintf(&b, ", summarised at %s", c.Recorded.SummaryLocation)
		}
	}
	if c.Shortfall != nil {
		fmt.Fprintf(&b, " (%v)", c.Shortfall)
	}
	return b.String()
}

// HandleEvent records the outcome of the copy the given event detail reports on.
func (h *CopyCompletionHandler) HandleEvent(ctx context.Context, detail []byte) (*CopyCompletion, error) {
	event, err := ParseCopyExecutionEvent(detail)
	if err != nil {
		return nil, err
	}
	return h.Handle(ctx, event)
}

// Handle records the outcome of one finished execution against the copy it was
// performing.
//
// An error is returned only for a failure that another attempt could get past -- a ledger
// read or write that did not go through. Everything else is an outcome: an event for an
// execution no copy is recorded under, or for a copy that has already reported, is
// reported as nothing to do rather than as a failure, because no number of retries will
// change it. A summary that could not be read is likewise not an error: the state is what
// gates granting the dataset and grants nothing on its own, so it is worth strictly more
// than the figures that describe it, and withholding it to preserve them would leave a
// staged dataset that nobody can be given.
func (h *CopyCompletionHandler) Handle(ctx context.Context, event CopyExecutionEvent) (*CopyCompletion, error) {
	completion := &CopyCompletion{
		ExecutionArn: event.ExecutionArn,
		Status:       event.Status,
	}

	if !event.Status.IsTerminal() {
		completion.Skipped = fmt.Sprintf(
			"the copy has not finished, and %s says nothing about its outcome", event.Status)
		return completion, nil
	}

	// The execution's name is the copy identifier; see the note at the top of this file.
	// An estimate run, named after a dataset instead, lands here too and resolves to
	// nothing, which is the correct answer for it.
	recorded, err := h.ledger.Get(ctx, event.Name)
	if errors.Is(err, ErrCopyNotFound) {
		completion.Skipped = fmt.Sprintf(
			"no copy is recorded under the name %q, so this execution transferred nothing "+
				"that has to be recorded", event.Name)
		return completion, nil
	}
	if err != nil {
		return completion, fmt.Errorf(
			"finding the copy execution %s was performing: %w", event.ExecutionArn, err)
	}
	completion.CopyID = recorded.CopyID

	// A copy records the execution performing it. A name that matches while the reference
	// does not means two executions were started under one identifier, and recording this
	// outcome would credit the copy with a transfer that was not the one it is waiting
	// on.
	if waiting := strings.TrimSpace(recorded.CopyExecutionArn); waiting != "" && waiting != event.ExecutionArn {
		completion.Skipped = fmt.Sprintf(
			"copy %q is waiting on execution %s, not this one", recorded.CopyID, waiting)
		return completion, nil
	}

	completion.Recorded, completion.Shortfall = h.readOutcome(ctx, event)
	if !event.Status.Delivered() && completion.Shortfall == nil {
		completion.Shortfall = copyFailureReason(event)
	}

	record := h.ledger.MarkFailed
	if event.Status.Delivered() {
		record = h.ledger.MarkStaged
	}

	updated, err := record(ctx, recorded.CopyID, completion.Recorded)
	var settled *ForbiddenCopyTransitionError
	if errors.As(err, &settled) {
		// The copy has already reported -- most often because this event is a second
		// delivery of one already acted on, and otherwise because the dataset was purged
		// while the transfer was still running. Either way the record says what became of
		// it and must not be rewritten from an event that has been overtaken.
		completion.Skipped = settled.Error()
		return completion, nil
	}
	if err != nil {
		return completion, fmt.Errorf(
			"recording the outcome of execution %s against copy %q: %w",
			event.ExecutionArn, recorded.CopyID, err)
	}

	completion.Copy = updated
	return completion, nil
}

// readOutcome gathers what the copy reported about what it moved, returning whatever it
// could establish together with the reason it is not the whole account.
//
// A copy that did not deliver the dataset is not asked: it never reached the step that
// summarises a run, so there is nothing written to read.
func (h *CopyCompletionHandler) readOutcome(ctx context.Context, event CopyExecutionEvent) (CopyOutcome, error) {
	if !event.Status.Delivered() {
		return CopyOutcome{}, nil
	}

	reported, err := h.reportedLocations(ctx, event)
	if err != nil {
		return CopyOutcome{}, err
	}

	retainedBucket := strings.TrimSpace(reported.WorkingBucket)
	retainedKey := strings.TrimSpace(reported.WorkingCsvKey)
	if retainedBucket == "" || retainedKey == "" {
		// The copy kept no summary outside the dataset's own bucket. The one inside it
		// is still recorded, since the copy has just written it and an operator can read
		// it with the dataset's key, but it is not read here.
		outcome := CopyOutcome{}
		if bucket, key := strings.TrimSpace(reported.DestinationBucket), strings.TrimSpace(reported.CsvKey); bucket != "" && key != "" {
			outcome.SummaryLocation = s3URI(bucket, key)
		}
		return outcome, errors.New(
			"the copy retained no summary outside the dataset's own bucket, so what it " +
				"transferred was not counted")
	}

	// Recorded whether or not it can be read, since the location is what an operator
	// needs once the execution's own history has aged out.
	outcome := CopyOutcome{SummaryLocation: s3URI(retainedBucket, retainedKey)}

	document, err := h.readSummary(ctx, retainedBucket, retainedKey)
	if err != nil {
		return outcome, err
	}
	totals, err := totalCopySummary(document)
	if err != nil {
		return outcome, fmt.Errorf("reading %s: %w", outcome.SummaryLocation, err)
	}

	outcome.ObjectCount = totals.objects
	outcome.TotalBytes = totals.bytes
	if totals.errored > 0 {
		return outcome, fmt.Errorf(
			"the copy reported %d object(s) it could not transfer, which are excluded from "+
				"the %d counted; %s lists them", totals.errored, totals.objects,
			outcome.SummaryLocation)
	}
	return outcome, nil
}

// reportedLocations reads the copy's own account of where it wrote its summary.
//
// The event normally carries the execution's output, and the execution is asked directly
// only when it does not -- either because the output exceeded what the event bus will
// carry, or because the event was assembled by something that left it out.
func (h *CopyCompletionHandler) reportedLocations(ctx context.Context, event CopyExecutionEvent) (copyOutputDocument, error) {
	output := ""
	if event.Output != nil && event.OutputDetails.Included {
		output = strings.TrimSpace(*event.Output)
	}

	if output == "" {
		if h.copies == nil {
			return copyOutputDocument{}, errors.New(
				"the event carried no output for the copy and there is no way to ask the " +
					"copy service for it, so what it transferred was not counted")
		}
		described, err := h.copies.DescribeExecution(ctx, &sfn.DescribeExecutionInput{
			ExecutionArn: aws.String(event.ExecutionArn),
		})
		if err != nil {
			return copyOutputDocument{}, CopyStateMachinePrerequisite(event.StateMachineArn, "").
				Missing(fmt.Errorf("asking copy %s what it reported: %w", event.ExecutionArn, err))
		}
		output = strings.TrimSpace(aws.ToString(described.Output))
	}

	if output == "" {
		return copyOutputDocument{}, fmt.Errorf(
			"copy %s reported no output, so where it summarised itself is unknown",
			event.ExecutionArn)
	}

	var reported copyOutputDocument
	if err := json.Unmarshal([]byte(output), &reported); err != nil {
		return copyOutputDocument{}, fmt.Errorf(
			"reading what copy %s reported: %w", event.ExecutionArn, err)
	}
	return reported, nil
}

// readSummary fetches the copy's summary of one run from the copy service's bucket.
func (h *CopyCompletionHandler) readSummary(ctx context.Context, bucket, key string) ([]byte, error) {
	if h.objects == nil {
		return nil, fmt.Errorf(
			"there is no way to read %s, so what the copy transferred was not counted",
			s3URI(bucket, key))
	}

	output, err := h.objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, CopyWorkingBucketPrerequisite(bucket, "").
			Missing(fmt.Errorf("reading %s: %w", s3URI(bucket, key), err))
	}
	if output.Body == nil {
		return nil, fmt.Errorf("%s is empty", s3URI(bucket, key))
	}
	defer func() { _ = output.Body.Close() }()

	document, err := io.ReadAll(output.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s3URI(bucket, key), err)
	}
	return document, nil
}

// Columns of the copy's summary. It carries one row per object with the transfer's speed
// and duration as well; those describe how the copy went rather than what it delivered,
// so they are not read.
const (
	summaryNameColumn   = "OBJECTNAME"
	summaryStatusColumn = "TRANSFERSTATUS"
	summaryBytesColumn  = "BYTESTRANSFERRED"
)

// summaryStatusError is the status the copy gives an object it could not transfer.
const summaryStatusError = "ERROR"

// summaryTotals is what one run's summary adds up to.
type summaryTotals struct {
	// objects is how many objects the copy accounted for and delivered.
	objects int64

	// bytes is how many bytes it moved to deliver them. It is not the size of the
	// dataset: an object already present at the destination with a matching checksum is
	// delivered without being transferred, and contributes nothing here.
	bytes int64

	// errored is how many objects the summary says could not be transferred, and which
	// are counted in neither figure above.
	errored int64
}

// totalCopySummary adds up one run's summary.
//
// Columns are located by heading rather than by position, so the totals do not depend on
// the order the copy service happens to write them in. A row that cannot be read fails
// the whole document rather than being skipped: a total that quietly omits objects is
// worse than no total, and the caller records the summary's location either way, so
// nothing is lost by refusing to guess.
func totalCopySummary(document []byte) (summaryTotals, error) {
	reader := csv.NewReader(bytes.NewReader(document))
	// The row width is not fixed by this code, since the columns are found by name and
	// the copy service is free to add more.
	reader.FieldsPerRecord = -1

	heading, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return summaryTotals{}, errors.New("the copy's summary is empty")
	}
	if err != nil {
		return summaryTotals{}, fmt.Errorf("reading the summary's heading row: %w", err)
	}

	columns := make(map[string]int, len(heading))
	for i, name := range heading {
		columns[strings.ToUpper(strings.TrimSpace(name))] = i
	}

	var missing []string
	for _, required := range []string{summaryNameColumn, summaryStatusColumn, summaryBytesColumn} {
		if _, found := columns[required]; !found {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return summaryTotals{}, fmt.Errorf(
			"the copy's summary has no %s column", strings.Join(missing, " or "))
	}

	widest := 0
	for _, index := range columns {
		widest = max(widest, index)
	}

	var totals summaryTotals
	for row := 2; ; row++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return totals, nil
		}
		if err != nil {
			return summaryTotals{}, fmt.Errorf("reading row %d of the summary: %w", row, err)
		}
		if len(record) <= widest {
			return summaryTotals{}, fmt.Errorf(
				"row %d of the summary has %d fields where the heading names %d",
				row, len(record), widest+1)
		}

		if strings.EqualFold(strings.TrimSpace(record[columns[summaryStatusColumn]]), summaryStatusError) {
			totals.errored++
			continue
		}

		field := strings.TrimSpace(record[columns[summaryBytesColumn]])
		transferred, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return summaryTotals{}, fmt.Errorf(
				"reading the bytes transferred for object %q in row %d of the summary: %w",
				strings.TrimSpace(record[columns[summaryNameColumn]]), row, err)
		}

		totals.objects++
		totals.bytes += transferred
	}
}

// wildcardExpansionLimitError is the error a copy reports when one instruction covering a
// whole folder matched more objects than the copy service will expand it to.
//
// It is worth recognising by name because the copy is refused entirely rather than
// partly done -- so nothing was staged, nothing was granted, and the answer is to name
// the dataset's objects rather than to retry. Nothing else in the copy's account says
// that, and the per-object detail it would otherwise take to work it out is in a file in
// the working bucket that an operator has to know to go and read.
const wildcardExpansionLimitError = "WildcardExpansionMaximumError"

// wildcardExpansionEmptyError is its counterpart: the folder matched nothing at all.
const wildcardExpansionEmptyError = "WildcardExpansionEmptyError"

// copyFailureReason turns what a failed execution reported into something an operator can
// act on, or passes it through where there is nothing to add.
//
// A copy fails for reasons this code cannot enumerate, so the reported error and cause are
// carried through rather than interpreted. The two exceptions are the ways an instruction
// covering a whole folder can be refused, because both have a specific answer and neither
// says so.
func copyFailureReason(event CopyExecutionEvent) error {
	reported := strings.TrimSpace(event.Error)
	cause := strings.TrimSpace(event.Cause)

	switch {
	case strings.Contains(reported, wildcardExpansionLimitError) ||
		strings.Contains(cause, wildcardExpansionLimitError):
		return fmt.Errorf(
			"the dataset holds more objects than one instruction covering the whole folder is "+
				"expanded to, so the copy service refused the run rather than copying part of "+
				"it. Nothing was staged and nothing was granted. Stage the dataset again naming "+
				"its objects, which are headed one by one and carry no such limit: %s",
			firstNonEmpty(cause, reported))

	case strings.Contains(reported, wildcardExpansionEmptyError) ||
		strings.Contains(cause, wildcardExpansionEmptyError):
		return fmt.Errorf(
			"the dataset's folder in the source store matched no objects, so there was nothing "+
				"to copy. Check the dataset URI against the folder as the source store spells "+
				"it: %s", firstNonEmpty(cause, reported))

	case reported == "" && cause == "":
		return nil

	default:
		return fmt.Errorf("the copy reported %s", firstNonEmpty(
			strings.TrimSpace(reported+" "+cause), reported))
	}
}

// firstNonEmpty returns the first of its arguments that carries anything.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
