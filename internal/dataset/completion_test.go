package dataset

// Tests for closing the loop on a copy: matching one execution's terminal state to the
// copy it was performing, and recording what it reported about what it moved.
//
// Two behaviours run through most of what follows. An event is matched to a copy by the
// execution's name, and the reference recorded when the copy was submitted is then
// checked against the event, so an outcome cannot be credited to a copy that was waiting
// on a different execution. And an error is reserved for a ledger read or write that
// another attempt could get past: everything else -- a copy that has not finished, an
// execution no copy is recorded under, a copy that has already reported, a summary that
// could not be read -- is an outcome rather than a failure, because no number of retries
// would change it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"

	"github.com/umccr/data-releaser/internal/awstest"
)

// Fixtures for one finished copy. The execution's name is the copy identifier verbatim,
// which is what makes the outcome resolvable without an index on the execution reference;
// the ARN merely happens to contain the same identifier.
const (
	completionCopyID        = "01JSTAGERUN000000000000AAA"
	completionExecutionArn  = "arn:aws:states:ap-southeast-2:999988887777:execution:copy:" + completionCopyID
	completionEarlierArn    = "arn:aws:states:ap-southeast-2:999988887777:execution:copy:earlier-attempt"
	completionStateMachine  = "arn:aws:states:ap-southeast-2:999988887777:stateMachine:copy"
	completionDatasetURI    = "s3://ceph-source/datasets/cohort-a/"
	completionCacheBucket   = "hot-cache-cohort-a"
	completionCacheKey      = ControlPrefix + "/summary.csv"
	completionWorkingBucket = "copy-service-working"
	completionWorkingKey    = "copy/" + completionCopyID + "/summary.csv"
)

// completionWorkingURI and completionCacheURI are the two places the copy writes the same
// summary. Only the first is ever read: the second is encrypted with the dataset's own
// key, which this path deliberately holds no access to.
const (
	completionWorkingURI = "s3://" + completionWorkingBucket + "/" + completionWorkingKey
	completionCacheURI   = "s3://" + completionCacheBucket + "/" + completionCacheKey
)

// Nothing wires the copy store into the handler yet -- the function still hands over a
// release store -- so this is what keeps the store answering the three calls recording an
// outcome makes until it does.
var _ CopyLedger = (*CopyStore)(nil)

// completionLedgerWrite is one attempt to move a copy to a new state.
type completionLedgerWrite struct {
	copyID  string
	to      CopyState
	outcome CopyOutcome
}

// completionLedger is the slice of the ledger a copy's outcome touches.
//
// It enforces the same rule the real store does -- an outcome may only be recorded
// against a copy that is still transferring -- so a second delivery of an event already
// acted on, or an event for a dataset purged while its transfer ran, is refused here
// exactly as the conditional write would refuse it.
type completionLedger struct {
	stored map[string]*Copy

	// getErr and writeErr stand in for a ledger that could not be reached, as opposed
	// to one that refused what it was asked.
	getErr   error
	writeErr error

	// lookups records the identifiers the ledger was asked about, so a test can assert
	// what the event resolved to, or that the ledger was not consulted at all.
	lookups []string
	// attempts holds every write asked for, writes only those that went through.
	attempts []completionLedgerWrite
	writes   []completionLedgerWrite
}

func (l *completionLedger) Get(_ context.Context, copyID string) (*Copy, error) {
	l.lookups = append(l.lookups, copyID)
	if l.getErr != nil {
		return nil, l.getErr
	}
	recorded, found := l.stored[copyID]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrCopyNotFound, copyID)
	}
	held := *recorded
	return &held, nil
}

func (l *completionLedger) MarkStaged(_ context.Context, copyID string, outcome CopyOutcome) (*Copy, error) {
	return l.record(copyID, CopyStateStaged, outcome)
}

func (l *completionLedger) MarkFailed(_ context.Context, copyID string, outcome CopyOutcome) (*Copy, error) {
	return l.record(copyID, CopyStateFailed, outcome)
}

func (l *completionLedger) record(copyID string, to CopyState, outcome CopyOutcome) (*Copy, error) {
	l.attempts = append(l.attempts, completionLedgerWrite{copyID: copyID, to: to, outcome: outcome})
	if l.writeErr != nil {
		return nil, l.writeErr
	}

	recorded, found := l.stored[copyID]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrCopyNotFound, copyID)
	}
	if recorded.State != CopyStateCopying {
		return nil, &ForbiddenCopyTransitionError{
			CopyID:        copyID,
			Current:       recorded.State,
			Requested:     to,
			PermittedFrom: []CopyState{CopyStateCopying},
		}
	}

	recorded.State = to
	recorded.ObjectCount = outcome.ObjectCount
	recorded.TotalBytes = outcome.TotalBytes
	recorded.CopySummaryLocation = outcome.SummaryLocation
	l.writes = append(l.writes, completionLedgerWrite{copyID: copyID, to: to, outcome: outcome})

	held := *recorded
	return &held, nil
}

// completionLedgerIn returns a ledger holding one copy in the given state, performed by
// the fixture execution.
func completionLedgerIn(state CopyState) *completionLedger {
	return &completionLedger{stored: map[string]*Copy{
		completionCopyID: {
			CopyID:           completionCopyID,
			DatasetURI:       completionDatasetURI,
			HotCacheBucket:   completionCacheBucket,
			KeyArn:           "arn:aws:kms:ap-southeast-2:999988887777:key/1111aaaa-2222-bbbb-3333-cccc4444dddd",
			State:            state,
			CopyExecutionArn: completionExecutionArn,
		},
	}}
}

// completionObjects serves one summary document and records every read, so a test can
// assert which copy of the summary was read -- or that none was.
type completionObjects struct {
	*awstest.S3
	reads []s3.GetObjectInput
}

func newCompletionObjects(document string, failure error) *completionObjects {
	objects := &completionObjects{S3: &awstest.S3{}}
	objects.GetObjectFn = func(_ context.Context, params *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		objects.reads = append(objects.reads, *params)
		if failure != nil {
			return nil, failure
		}
		return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(document))}, nil
	}
	return objects
}

// completionCopies stands in for the copy service, recording the executions it was asked
// about so the fallback path is observable.
type completionCopies struct {
	*awstest.StepFunctions
	describes []sfn.DescribeExecutionInput
}

func newCompletionCopies(output string, failure error) *completionCopies {
	copies := &completionCopies{StepFunctions: &awstest.StepFunctions{}}
	copies.DescribeExecutionFn = func(_ context.Context, params *sfn.DescribeExecutionInput) (*sfn.DescribeExecutionOutput, error) {
		copies.describes = append(copies.describes, *params)
		if failure != nil {
			return nil, failure
		}
		return &sfn.DescribeExecutionOutput{
			ExecutionArn: params.ExecutionArn,
			Output:       aws.String(output),
		}, nil
	}
	return copies
}

// completionReportedLocations is what the copy's summarising step reports: the same
// summary in the dataset's bucket and in the copy service's own.
func completionReportedLocations() copyOutputDocument {
	return copyOutputDocument{
		DestinationBucket: completionCacheBucket,
		CsvKey:            completionCacheKey,
		WorkingBucket:     completionWorkingBucket,
		WorkingCsvKey:     completionWorkingKey,
	}
}

// completionOutput encodes what the copy reported, as an execution's output carries it.
func completionOutput(t *testing.T, reported copyOutputDocument) *string {
	t.Helper()

	encoded, err := json.Marshal(reported)
	if err != nil {
		t.Fatalf("encoding what the copy reported: %v", err)
	}
	output := string(encoded)
	return &output
}

// completionEvent is one status change for the fixture copy. A succeeded execution
// carries its output, which is the case the event bus delivers unless the document is too
// large for it.
func completionEvent(t *testing.T, status ExecutionStatus) CopyExecutionEvent {
	t.Helper()

	event := CopyExecutionEvent{
		ExecutionArn:    completionExecutionArn,
		StateMachineArn: completionStateMachine,
		Name:            completionCopyID,
		Status:          status,
	}
	if status.Delivered() {
		event.Output = completionOutput(t, completionReportedLocations())
		event.OutputDetails.Included = true
	}
	return event
}

// completionSummaryRow is one object's line in the copy's account of a run.
type completionSummaryRow struct {
	name   string
	status string
	bytes  string
}

// completionSummaryCSV renders a summary in the canonical column order. Tests about where
// the columns sit build their own document.
func completionSummaryCSV(rows ...completionSummaryRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s,%s,%s\n", summaryNameColumn, summaryStatusColumn, summaryBytesColumn)
	for _, row := range rows {
		fmt.Fprintf(&b, "%s,%s,%s\n", row.name, row.status, row.bytes)
	}
	return b.String()
}

// assertNothingRecorded fails unless the event was reported as nothing to do, without the
// ledger being written.
func assertNothingRecorded(t *testing.T, completion *CopyCompletion, ledger *completionLedger) {
	t.Helper()

	if completion.Skipped == "" {
		t.Error("expected a stated reason nothing was recorded")
	}
	if completion.Written() {
		t.Errorf("expected nothing to be written, got a copy in state %q", completion.Copy.State)
	}
	if len(ledger.writes) != 0 {
		t.Errorf("expected the ledger to be left alone, got %d write(s)", len(ledger.writes))
	}
}

// The ordinary case: an execution delivered the dataset, and the copy it was performing
// becomes staged with the figures it reported about what it moved.
func TestHandleRecordsASucceededExecutionAgainstTheCopyItPerformed(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)
	objects := newCompletionObjects(completionSummaryCSV(
		completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "1024"},
		completionSummaryRow{"cohort-a/sample1/reads.bam.bai", "OK", "96"},
	), nil)
	copies := newCompletionCopies("", nil)

	handler := NewCopyCompletionHandler(ledger, copies, objects)
	completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}

	if completion.Shortfall != nil {
		t.Errorf("expected a complete account of the copy, got a shortfall: %v", completion.Shortfall)
	}
	if completion.Skipped != "" {
		t.Errorf("expected the outcome to be recorded, got skipped: %s", completion.Skipped)
	}
	if !completion.Written() {
		t.Fatal("expected the outcome to be written to the ledger")
	}
	if completion.Copy.State != CopyStateStaged {
		t.Errorf("state: expected %q, got %q", CopyStateStaged, completion.Copy.State)
	}

	want := CopyOutcome{ObjectCount: 2, TotalBytes: 1120, SummaryLocation: completionWorkingURI}
	if completion.Recorded != want {
		t.Errorf("recorded: expected %+v, got %+v", want, completion.Recorded)
	}

	// Resolution is by the execution's name, which is the copy identifier verbatim,
	// rather than by anything derived from the ARN.
	if len(ledger.lookups) != 1 || ledger.lookups[0] != completionCopyID {
		t.Errorf("expected the ledger to be asked about %q, got %v", completionCopyID, ledger.lookups)
	}
	if completion.CopyID != completionCopyID {
		t.Errorf("matched copy: expected %q, got %q", completionCopyID, completion.CopyID)
	}

	// The summary is read from the copy service's own bucket. Reading the copy in the
	// dataset's bucket would need decrypt permission on every dataset key that will ever
	// exist.
	if len(objects.reads) != 1 {
		t.Fatalf("expected one summary to be read, got %d", len(objects.reads))
	}
	if bucket := aws.ToString(objects.reads[0].Bucket); bucket != completionWorkingBucket {
		t.Errorf("summary read from bucket %q, expected %q", bucket, completionWorkingBucket)
	}
	if key := aws.ToString(objects.reads[0].Key); key != completionWorkingKey {
		t.Errorf("summary read from key %q, expected %q", key, completionWorkingKey)
	}

	// The event carried the output, so there was nothing to ask the copy service.
	if len(copies.describes) != 0 {
		t.Errorf("expected the copy service not to be asked, got %d call(s)", len(copies.describes))
	}
}

// A copy that did not deliver never reached the step that summarises a run, so there is
// no account of it to read and nothing to point an operator at beyond the state and the
// execution reference recorded when the copy was submitted.
func TestHandleRecordsACopyThatDidNotDeliverWithNoAccountOfWhatItMoved(t *testing.T) {
	for _, status := range []ExecutionStatus{ExecutionFailed, ExecutionTimedOut, ExecutionAborted} {
		t.Run(status.String(), func(t *testing.T) {
			ledger := completionLedgerIn(CopyStateCopying)
			objects := newCompletionObjects(completionSummaryCSV(), nil)
			copies := newCompletionCopies("", nil)

			handler := NewCopyCompletionHandler(ledger, copies, objects)
			completion, err := handler.Handle(context.Background(), completionEvent(t, status))
			if err != nil {
				t.Fatalf("recording the outcome: %v", err)
			}

			if !completion.Written() {
				t.Fatal("expected the failure to be written to the ledger")
			}
			if completion.Copy.State != CopyStateFailed {
				t.Errorf("state: expected %q, got %q", CopyStateFailed, completion.Copy.State)
			}
			if completion.Recorded != (CopyOutcome{}) {
				t.Errorf("expected nothing to be recorded about what was moved, got %+v", completion.Recorded)
			}
			if completion.Shortfall != nil {
				t.Errorf("expected no shortfall for a copy that summarised nothing, got %v", completion.Shortfall)
			}
			if len(objects.reads) != 0 {
				t.Errorf("expected no summary to be read, got %d read(s)", len(objects.reads))
			}
			if len(copies.describes) != 0 {
				t.Errorf("expected the copy service not to be asked, got %d call(s)", len(copies.describes))
			}
		})
	}
}

// A copy still running says nothing about an outcome, so the ledger is not even consulted.
func TestHandleLeavesACopyThatHasNotFinishedAlone(t *testing.T) {
	for _, status := range []ExecutionStatus{ExecutionRunning, ExecutionPendingRedrive} {
		t.Run(status.String(), func(t *testing.T) {
			ledger := completionLedgerIn(CopyStateCopying)

			handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), newCompletionObjects("", nil))
			completion, err := handler.Handle(context.Background(), completionEvent(t, status))
			if err != nil {
				t.Fatalf("expected a running copy to be an outcome rather than a failure: %v", err)
			}

			assertNothingRecorded(t, completion, ledger)
			if !strings.Contains(completion.Skipped, status.String()) {
				t.Errorf("expected the reason to name the status, got %q", completion.Skipped)
			}
			if len(ledger.lookups) != 0 {
				t.Errorf("expected the ledger not to be consulted, got %v", ledger.lookups)
			}
		})
	}
}

// An estimate run shares the state machine but is named after the dataset rather than
// after a copy, so it resolves to no record. That is the correct answer for it, and
// retrying would not change it, so it is an outcome and not a failure.
func TestHandleReportsAnExecutionNoCopyIsRecordedUnder(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)
	objects := newCompletionObjects(completionSummaryCSV(), nil)

	event := completionEvent(t, ExecutionSucceeded)
	event.Name = "estimate-cohort-a-20260506"
	event.ExecutionArn = "arn:aws:states:ap-southeast-2:999988887777:execution:copy:" + event.Name

	handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), objects)
	completion, err := handler.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("expected an unmatched execution to be an outcome rather than a failure: %v", err)
	}

	assertNothingRecorded(t, completion, ledger)
	if !strings.Contains(completion.Skipped, event.Name) {
		t.Errorf("expected the reason to name the execution, got %q", completion.Skipped)
	}
	if completion.CopyID != "" {
		t.Errorf("expected no copy to be matched, got %q", completion.CopyID)
	}
	if len(ledger.attempts) != 0 {
		t.Errorf("expected no write to be attempted, got %+v", ledger.attempts)
	}
	if len(objects.reads) != 0 {
		t.Errorf("expected no summary to be read, got %d read(s)", len(objects.reads))
	}
}

// Two executions started under one identifier would otherwise let this outcome be
// credited to a copy waiting for the other one.
func TestHandleWillNotCreditACopyWaitingOnADifferentExecution(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)
	ledger.stored[completionCopyID].CopyExecutionArn = completionEarlierArn

	handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), newCompletionObjects(completionSummaryCSV(), nil))
	completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
	if err != nil {
		t.Fatalf("expected a mismatched copy to be an outcome rather than a failure: %v", err)
	}

	assertNothingRecorded(t, completion, ledger)
	if !strings.Contains(completion.Skipped, completionEarlierArn) {
		t.Errorf("expected the reason to name the execution the copy is waiting on, got %q", completion.Skipped)
	}
	if completion.CopyID != completionCopyID {
		t.Errorf("matched copy: expected %q, got %q", completionCopyID, completion.CopyID)
	}
	if got := ledger.stored[completionCopyID].State; got != CopyStateCopying {
		t.Errorf("expected the copy to be left transferring, got %q", got)
	}
}

// A copy recorded without an execution reference -- submitted by a transfer that started
// but whose reference could not be obtained -- still accepts the outcome, since there is
// nothing to contradict.
func TestHandleRecordsAgainstACopyWithNoRecordedExecutionReference(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)
	ledger.stored[completionCopyID].CopyExecutionArn = ""

	handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), newCompletionObjects(completionSummaryCSV(), nil))
	completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}

	if !completion.Written() {
		t.Fatalf("expected the outcome to be written, got skipped: %s", completion.Skipped)
	}
	if completion.Copy.State != CopyStateStaged {
		t.Errorf("state: expected %q, got %q", CopyStateStaged, completion.Copy.State)
	}
}

// The event bus can deliver the same event twice, and a dataset can be purged while its
// copy is still running. Either way the record says what became of the copy, and an event
// that has been overtaken must not rewrite it.
func TestHandleTreatsAnOvertakenEventAsAlreadySettled(t *testing.T) {
	t.Run("a repeated delivery of an event already acted on", func(t *testing.T) {
		ledger := completionLedgerIn(CopyStateCopying)
		objects := newCompletionObjects(completionSummaryCSV(
			completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "1024"},
		), nil)
		handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), objects)

		first, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
		if err != nil {
			t.Fatalf("recording the first delivery: %v", err)
		}
		if !first.Written() {
			t.Fatalf("expected the first delivery to be recorded, got skipped: %s", first.Skipped)
		}

		second, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
		if err != nil {
			t.Fatalf("expected a repeated event to be an outcome rather than a failure: %v", err)
		}

		if second.Skipped == "" {
			t.Error("expected a stated reason the repeat was not recorded")
		}
		if second.Written() {
			t.Error("expected the repeat to write nothing")
		}
		if !strings.Contains(second.Skipped, string(CopyStateStaged)) {
			t.Errorf("expected the reason to name the state the copy is in, got %q", second.Skipped)
		}
		if len(ledger.writes) != 1 {
			t.Errorf("expected exactly one write across both deliveries, got %d", len(ledger.writes))
		}
	})

	t.Run("a dataset purged while its copy was still running", func(t *testing.T) {
		ledger := completionLedgerIn(CopyStatePurged)

		handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), newCompletionObjects(completionSummaryCSV(), nil))
		completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
		if err != nil {
			t.Fatalf("expected a settled copy to be an outcome rather than a failure: %v", err)
		}

		assertNothingRecorded(t, completion, ledger)
		if got := ledger.stored[completionCopyID].State; got != CopyStatePurged {
			t.Errorf("expected the copy to stay purged, got %q", got)
		}
	})
}

// A ledger that could not be read or written is the one failure another attempt could get
// past, so it is the only one reported as an error.
func TestHandleReportsALedgerFailureAsWorthAnotherAttempt(t *testing.T) {
	unreachable := errors.New("the ledger is unreachable")

	t.Run("reading the copy", func(t *testing.T) {
		ledger := completionLedgerIn(CopyStateCopying)
		ledger.getErr = unreachable

		handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), newCompletionObjects(completionSummaryCSV(), nil))
		_, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
		if !errors.Is(err, unreachable) {
			t.Fatalf("expected the ledger failure to be reported, got %v", err)
		}
	})

	t.Run("writing the outcome", func(t *testing.T) {
		ledger := completionLedgerIn(CopyStateCopying)
		ledger.writeErr = unreachable

		handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), newCompletionObjects(completionSummaryCSV(), nil))
		completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
		if !errors.Is(err, unreachable) {
			t.Fatalf("expected the ledger failure to be reported, got %v", err)
		}
		if completion.Written() {
			t.Error("expected nothing to be reported as written")
		}
	})
}

// The state is what gates granting the dataset and grants nothing on its own, so it is
// worth strictly more than the figures describing the copy. Withholding it to preserve
// them would leave a staged dataset that nobody can be given.
func TestHandleRecordsTheStateEvenWhenTheSummaryCannotBeRead(t *testing.T) {
	unreadable := errors.New("access denied")

	tests := []struct {
		name string
		// event is the delivery to record, built per case because what is missing from
		// it is usually the point.
		event            func(t *testing.T) CopyExecutionEvent
		objects          *completionObjects
		copies           *completionCopies
		wantLocation     string
		wantShortfall    string
		wantNoReads      bool
		wantPrerequisite bool
	}{
		{
			name:             "the retained summary could not be read",
			event:            func(t *testing.T) CopyExecutionEvent { return completionEvent(t, ExecutionSucceeded) },
			objects:          newCompletionObjects("", unreadable),
			copies:           newCompletionCopies("", nil),
			wantLocation:     completionWorkingURI,
			wantShortfall:    completionWorkingKey,
			wantPrerequisite: true,
		},
		{
			name: "the copy kept no summary outside the dataset's own bucket",
			event: func(t *testing.T) CopyExecutionEvent {
				reported := completionReportedLocations()
				reported.WorkingBucket = ""
				reported.WorkingCsvKey = ""
				event := completionEvent(t, ExecutionSucceeded)
				event.Output = completionOutput(t, reported)
				return event
			},
			objects: newCompletionObjects(completionSummaryCSV(), nil),
			copies:  newCompletionCopies("", nil),
			// The copy in the dataset's bucket is still pointed at, since the copy has
			// just written it and an operator holding the dataset's key can read it.
			wantLocation:  completionCacheURI,
			wantShortfall: "retained no summary",
			wantNoReads:   true,
		},
		{
			name: "the copy reported nowhere at all",
			event: func(t *testing.T) CopyExecutionEvent {
				event := completionEvent(t, ExecutionSucceeded)
				empty := ""
				event.Output = &empty
				return event
			},
			objects:       newCompletionObjects(completionSummaryCSV(), nil),
			copies:        newCompletionCopies("", nil),
			wantShortfall: "reported no output",
			wantNoReads:   true,
		},
		{
			// The copy service is only asked when the event withholds the output, which
			// is also how a document too large for the bus arrives.
			name: "the copy service could not be asked what it reported",
			event: func(t *testing.T) CopyExecutionEvent {
				event := completionEvent(t, ExecutionSucceeded)
				event.Output = nil
				return event
			},
			objects:          newCompletionObjects(completionSummaryCSV(), nil),
			copies:           newCompletionCopies("", unreadable),
			wantShortfall:    "copy state machine",
			wantNoReads:      true,
			wantPrerequisite: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger := completionLedgerIn(CopyStateCopying)

			handler := NewCopyCompletionHandler(ledger, test.copies, test.objects)
			completion, err := handler.Handle(context.Background(), test.event(t))
			if err != nil {
				t.Fatalf("expected an unreadable summary to be recorded rather than reported: %v", err)
			}

			if !completion.Written() {
				t.Fatalf("expected the state to be recorded anyway, got skipped: %s", completion.Skipped)
			}
			if completion.Copy.State != CopyStateStaged {
				t.Errorf("state: expected %q, got %q", CopyStateStaged, completion.Copy.State)
			}
			if completion.Shortfall == nil {
				t.Fatal("expected the incomplete account to be reported as a shortfall")
			}
			if !strings.Contains(completion.Shortfall.Error(), test.wantShortfall) {
				t.Errorf("shortfall %q does not mention %q", completion.Shortfall, test.wantShortfall)
			}
			if completion.Recorded.ObjectCount != 0 || completion.Recorded.TotalBytes != 0 {
				t.Errorf("expected no figures to be invented, got %+v", completion.Recorded)
			}
			if completion.Recorded.SummaryLocation != test.wantLocation {
				t.Errorf("summary location: expected %q, got %q",
					test.wantLocation, completion.Recorded.SummaryLocation)
			}
			if test.wantNoReads && len(test.objects.reads) != 0 {
				t.Errorf("expected no summary to be read, got %d read(s)", len(test.objects.reads))
			}
			if test.wantPrerequisite {
				// An operator gets the resource and its owner, not the bare AWS error.
				var missing *MissingPrerequisiteError
				if !errors.As(completion.Shortfall, &missing) {
					t.Errorf("expected the failure to name the prerequisite behind it, got %v", completion.Shortfall)
				}
			}
		})
	}
}

// A caller that accepts the state without the figures may supply neither client, which
// the handler treats as one more incomplete account rather than as a failure.
func TestHandleRecordsTheStateWithNoClientsToGatherFiguresWith(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)

	handler := NewCopyCompletionHandler(ledger, nil, nil)
	completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}

	if !completion.Written() {
		t.Fatalf("expected the state to be recorded anyway, got skipped: %s", completion.Skipped)
	}
	if completion.Copy.State != CopyStateStaged {
		t.Errorf("state: expected %q, got %q", CopyStateStaged, completion.Copy.State)
	}
	if completion.Shortfall == nil {
		t.Error("expected the missing figures to be reported as a shortfall")
	}
	// The location is still recorded: it is what an operator needs once the execution's
	// own history has aged out.
	if completion.Recorded.SummaryLocation != completionWorkingURI {
		t.Errorf("summary location: expected %q, got %q",
			completionWorkingURI, completion.Recorded.SummaryLocation)
	}
}

// The event bus drops an output above its own size limit and says so, and an event
// assembled by something else may omit it entirely. Either way the execution itself still
// knows where it summarised the run.
func TestHandleAsksTheCopyServiceWhenTheEventDoesNotCarryTheOutput(t *testing.T) {
	tests := []struct {
		name    string
		without func(event *CopyExecutionEvent)
	}{
		{
			name:    "the event carries no output",
			without: func(event *CopyExecutionEvent) { event.Output = nil },
		},
		{
			name:    "the output was too large for the event bus",
			without: func(event *CopyExecutionEvent) { event.OutputDetails.Included = false },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger := completionLedgerIn(CopyStateCopying)
			objects := newCompletionObjects(completionSummaryCSV(
				completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "2048"},
			), nil)
			copies := newCompletionCopies(*completionOutput(t, completionReportedLocations()), nil)

			event := completionEvent(t, ExecutionSucceeded)
			test.without(&event)

			handler := NewCopyCompletionHandler(ledger, copies, objects)
			completion, err := handler.Handle(context.Background(), event)
			if err != nil {
				t.Fatalf("recording the outcome: %v", err)
			}

			if completion.Shortfall != nil {
				t.Errorf("expected a complete account of the copy, got a shortfall: %v", completion.Shortfall)
			}
			if len(copies.describes) != 1 {
				t.Fatalf("expected the copy service to be asked once, got %d call(s)", len(copies.describes))
			}
			if arn := aws.ToString(copies.describes[0].ExecutionArn); arn != completionExecutionArn {
				t.Errorf("asked about %q, expected %q", arn, completionExecutionArn)
			}

			want := CopyOutcome{ObjectCount: 1, TotalBytes: 2048, SummaryLocation: completionWorkingURI}
			if completion.Recorded != want {
				t.Errorf("recorded: expected %+v, got %+v", want, completion.Recorded)
			}
		})
	}
}

// Objects the copy could not transfer are excluded from the totals and reported, so a
// count that silently omitted them cannot be mistaken for the whole dataset.
func TestHandleReportsObjectsTheCopyCouldNotTransfer(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)
	objects := newCompletionObjects(completionSummaryCSV(
		completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "1024"},
		completionSummaryRow{"cohort-a/sample2/reads.bam", summaryStatusError, "0"},
	), nil)

	handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), objects)
	completion, err := handler.Handle(context.Background(), completionEvent(t, ExecutionSucceeded))
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}

	if !completion.Written() {
		t.Fatalf("expected the outcome to be recorded, got skipped: %s", completion.Skipped)
	}
	want := CopyOutcome{ObjectCount: 1, TotalBytes: 1024, SummaryLocation: completionWorkingURI}
	if completion.Recorded != want {
		t.Errorf("recorded: expected %+v, got %+v", want, completion.Recorded)
	}
	if completion.Shortfall == nil {
		t.Fatal("expected the objects that could not be transferred to be reported")
	}
	if !strings.Contains(completion.Shortfall.Error(), completionWorkingURI) {
		t.Errorf("expected the report to point at the summary listing them, got %v", completion.Shortfall)
	}
}

// The whole path from the raw event detail, which is what an EventBridge delivery hands
// over.
func TestHandleEventRecordsFromTheRawEventDetail(t *testing.T) {
	ledger := completionLedgerIn(CopyStateCopying)
	objects := newCompletionObjects(completionSummaryCSV(
		completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "512"},
	), nil)

	detail, err := json.Marshal(map[string]any{
		"executionArn":    completionExecutionArn,
		"stateMachineArn": completionStateMachine,
		"name":            completionCopyID,
		"status":          string(ExecutionSucceeded),
		"output":          *completionOutput(t, completionReportedLocations()),
		"outputDetails":   map[string]any{"included": true},
	})
	if err != nil {
		t.Fatalf("encoding the event detail: %v", err)
	}

	handler := NewCopyCompletionHandler(ledger, newCompletionCopies("", nil), objects)
	completion, err := handler.HandleEvent(context.Background(), detail)
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}

	if !completion.Written() {
		t.Fatalf("expected the outcome to be recorded, got skipped: %s", completion.Skipped)
	}
	if completion.Copy.State != CopyStateStaged {
		t.Errorf("state: expected %q, got %q", CopyStateStaged, completion.Copy.State)
	}
	if completion.Recorded.ObjectCount != 1 || completion.Recorded.TotalBytes != 512 {
		t.Errorf("recorded: got %+v", completion.Recorded)
	}
	// The one line a caller logs should say what happened to which copy.
	if line := completion.String(); !strings.Contains(line, completionCopyID) ||
		!strings.Contains(line, string(CopyStateStaged)) {
		t.Errorf("the reported line says too little: %s", line)
	}
}

// An event missing the fields that identify it cannot be acted on at all, and is
// rejected rather than guessed at.
func TestParseCopyExecutionEvent(t *testing.T) {
	t.Run("a well-formed detail is read and trimmed", func(t *testing.T) {
		event, err := ParseCopyExecutionEvent([]byte(`{
			"executionArn": "  ` + completionExecutionArn + `  ",
			"stateMachineArn": " ` + completionStateMachine + ` ",
			"name": "  ` + completionCopyID + `  ",
			"status": "SUCCEEDED",
			"output": null,
			"outputDetails": {"included": false}
		}`))
		if err != nil {
			t.Fatalf("reading the event: %v", err)
		}
		if event.ExecutionArn != completionExecutionArn {
			t.Errorf("execution: expected %q, got %q", completionExecutionArn, event.ExecutionArn)
		}
		if event.StateMachineArn != completionStateMachine {
			t.Errorf("state machine: expected %q, got %q", completionStateMachine, event.StateMachineArn)
		}
		if event.Name != completionCopyID {
			t.Errorf("name: expected %q, got %q", completionCopyID, event.Name)
		}
		if event.Status != ExecutionSucceeded {
			t.Errorf("status: expected %q, got %q", ExecutionSucceeded, event.Status)
		}
		if event.Output != nil {
			t.Errorf("expected no output, got %q", *event.Output)
		}
	})

	tests := []struct {
		name   string
		detail string
		want   string
	}{
		{
			name:   "not a document at all",
			detail: `not json`,
			want:   "reading copy execution event",
		},
		{
			name:   "no execution reference",
			detail: `{"name": "` + completionCopyID + `", "status": "SUCCEEDED"}`,
			want:   "executionArn",
		},
		{
			name:   "no name to resolve a copy by",
			detail: `{"executionArn": "` + completionExecutionArn + `", "status": "SUCCEEDED"}`,
			want:   "name",
		},
		{
			name:   "a name of nothing but spaces",
			detail: `{"executionArn": "` + completionExecutionArn + `", "name": "   ", "status": "SUCCEEDED"}`,
			want:   "name",
		},
		{
			name:   "no status to record",
			detail: `{"executionArn": "` + completionExecutionArn + `", "name": "` + completionCopyID + `"}`,
			want:   "status",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseCopyExecutionEvent([]byte(test.detail))
			if err == nil {
				t.Fatal("expected the event to be rejected")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected the complaint to mention %q, got %v", test.want, err)
			}
		})
	}

	t.Run("an unreadable detail is a failure rather than an outcome", func(t *testing.T) {
		handler := NewCopyCompletionHandler(completionLedgerIn(CopyStateCopying), nil, nil)
		completion, err := handler.HandleEvent(context.Background(), []byte(`{`))
		if err == nil {
			t.Fatal("expected the event to be rejected")
		}
		if completion != nil {
			t.Errorf("expected nothing to be reported, got %+v", completion)
		}
	})
}

// Which statuses end a copy, and which of those delivered the dataset. Everything the
// handler decides hangs off these two, so they are worth stating outright.
func TestExecutionStatusTerminalityAndDelivery(t *testing.T) {
	tests := []struct {
		status    ExecutionStatus
		terminal  bool
		delivered bool
	}{
		{ExecutionRunning, false, false},
		{ExecutionPendingRedrive, false, false},
		{ExecutionSucceeded, true, true},
		{ExecutionFailed, true, false},
		{ExecutionTimedOut, true, false},
		{ExecutionAborted, true, false},
	}

	for _, test := range tests {
		t.Run(test.status.String(), func(t *testing.T) {
			if got := test.status.IsTerminal(); got != test.terminal {
				t.Errorf("terminal: expected %t, got %t", test.terminal, got)
			}
			if got := test.status.Delivered(); got != test.delivered {
				t.Errorf("delivered: expected %t, got %t", test.delivered, got)
			}
		})
	}
}

// Columns are found by heading rather than by position, so the totals do not depend on
// the order the copy service writes them in, and a row that cannot be read fails the
// whole document rather than being quietly dropped from the count.
func TestTotalCopySummary(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     summaryTotals
		wantErr  string
	}{
		{
			name: "columns in an order of the copy service's choosing, with extras",
			document: "duration,BYTESTRANSFERRED,objectName,transferStatus,bandwidth\n" +
				"1.5,1024,cohort-a/sample1/reads.bam,OK,64\n" +
				"0.2,96,cohort-a/sample1/reads.bam.bai,OK,32\n",
			want: summaryTotals{objects: 2, bytes: 1120},
		},
		{
			name: "objects the copy could not transfer are excluded and counted",
			document: completionSummaryCSV(
				completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "1024"},
				completionSummaryRow{"cohort-a/sample2/reads.bam", summaryStatusError, "0"},
				completionSummaryRow{"cohort-a/sample3/reads.bam", "error", "0"},
			),
			want: summaryTotals{objects: 1, bytes: 1024, errored: 2},
		},
		{
			name:     "a run that accounted for nothing",
			document: completionSummaryCSV(),
			want:     summaryTotals{},
		},
		{
			name:     "an empty document",
			document: "",
			wantErr:  "empty",
		},
		{
			name:     "a heading missing the columns the totals come from",
			document: "OBJECTNAME,TRANSFERSTATUS\ncohort-a/sample1/reads.bam,OK\n",
			wantErr:  summaryBytesColumn,
		},
		{
			name: "a byte count that is not a number",
			document: completionSummaryCSV(
				completionSummaryRow{"cohort-a/sample1/reads.bam", "OK", "a lot"},
			),
			wantErr: "cohort-a/sample1/reads.bam",
		},
		{
			name:     "a row narrower than the heading",
			document: completionSummaryCSV() + "cohort-a/sample1/reads.bam,OK\n",
			wantErr:  "fields where the heading names",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			totals, err := totalCopySummary([]byte(test.document))
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("expected the summary to be refused, got %+v", totals)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Errorf("expected the complaint to mention %q, got %v", test.wantErr, err)
				}
				if totals != (summaryTotals{}) {
					t.Errorf("expected no partial totals to be returned, got %+v", totals)
				}
				return
			}
			if err != nil {
				t.Fatalf("reading the summary: %v", err)
			}
			if totals != test.want {
				t.Errorf("totals: expected %+v, got %+v", test.want, totals)
			}
		})
	}
}

// TestCopyFailureReasonExplainsTheWildcardRefusals covers the two ways an instruction
// covering a whole folder is refused.
//
// Both matter because the copy is refused entirely rather than partly done, and neither
// says so: what reaches the ledger otherwise is a copy in the failed state with no
// account of why, and the detail that would explain it sits in a file in the working
// bucket an operator has to know to go and read.
func TestCopyFailureReasonExplainsTheWildcardRefusals(t *testing.T) {
	tests := []struct {
		name     string
		event    CopyExecutionEvent
		wantText []string
	}{
		{
			name: "the folder held more objects than one instruction expands to",
			event: CopyExecutionEvent{
				Status: ExecutionFailed,
				Error:  "WildcardExpansionMaximumError",
				Cause:  "Expanding s3://source/datasets/cohort-a/* resulted in a number of objects that exceeds our safety limit of 256",
			},
			wantText: []string{"naming its objects", "Nothing was staged", "256"},
		},
		{
			name: "the folder matched nothing",
			event: CopyExecutionEvent{
				Status: ExecutionFailed,
				Error:  "WildcardExpansionEmptyError",
				Cause:  "Expanding s3://source/datasets/cohort-a/* resulted in no objects",
			},
			wantText: []string{"matched no objects", "dataset URI"},
		},
		{
			name: "anything else is carried through rather than interpreted",
			event: CopyExecutionEvent{
				Status: ExecutionFailed,
				Error:  "States.Timeout",
				Cause:  "the execution ran longer than its timeout",
			},
			wantText: []string{"States.Timeout", "longer than its timeout"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := copyFailureReason(tt.event)
			if err == nil {
				t.Fatal("expected a failed copy to carry a reason")
			}
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("expected the reason to carry %q, got: %v", want, err)
				}
			}
		})
	}

	// A failure the event says nothing about adds nothing, rather than inventing a
	// reason that would read as though something had been established.
	if err := copyFailureReason(CopyExecutionEvent{Status: ExecutionFailed}); err != nil {
		t.Errorf("expected no reason where the event reported none, got: %v", err)
	}
}
