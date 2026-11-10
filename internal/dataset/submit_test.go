package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sfn"

	"github.com/umccr/data-releaser/internal/awstest"
)

// Fixtures for building and submitting one copy. Held apart from the policy and ledger
// fixtures so that a change to either cannot quietly move what these assert.
const (
	submitDatasetURI    = "s3://ceph-source/datasets/cohort-a/"
	submitSourceBucket  = "ceph-source"
	submitDatasetPrefix = "datasets/cohort-a/"
	submitDestination   = "hot-cache-cohort-a"
	submitWorkingBucket = "copy-service-working"
	submitWorkingPrefix = "copy"
	submitStateMachine  = "arn:aws:states:ap-southeast-2:916218008751:stateMachine:copy"
	submitRunID         = "01JCOPYRUN0000000000000AAA"
	submitKeyArn        = "arn:aws:kms:ap-southeast-2:916218008751:key/1111aaaa-2222-bbbb-3333-cccc4444dddd"
	submitExecutionArn  = "arn:aws:states:ap-southeast-2:916218008751:execution:copy:" + submitRunID
)

// submitClock pins the clock, so a recorded timestamp and the timestamped name a priced
// run is given are both something a test can state exactly.
var submitClock = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

func submitConfig() CopyConfig {
	return CopyConfig{
		AccountID:           "916218008751",
		Region:              "ap-southeast-2",
		StateMachineARN:     submitStateMachine,
		WorkingBucket:       submitWorkingBucket,
		WorkingBucketPrefix: submitWorkingPrefix,
		Source: SourceStore{
			EndpointURL:       "https://ceph.example.org:8080",
			Region:            "us-east-1",
			CredentialsSecret: "source-store/ceph",
		},
		Performance: CopyPerformance{
			LargeCopyConcurrency: 4,
			SmallCopyBatchSize:   8,
		},
	}
}

// The copy service reads an absent performance field as "use your default" and a present
// zero as the number zero, so what is left unset has to be left out rather than sent as
// zero. It also renamed these settings once already, dropping the ones this tool used to
// send without anything failing -- the request was simply ignored and every default
// applied. So the wire format is asserted here rather than assumed: a copy that quietly
// runs at defaults against a source that cannot take them is the failure this guards.
func TestSubmitSendsOnlyTheConfiguredPerformanceSettings(t *testing.T) {
	var started string
	copies := pinnedCopies(submitClock)
	base := copies.StartExecutionFn
	copies.StartExecutionFn = func(ctx context.Context, params *sfn.StartExecutionInput) (*sfn.StartExecutionOutput, error) {
		started = aws.ToString(params.Input)
		return base(ctx, params)
	}

	submitter := newTestSubmitter(t, &awstest.S3{}, copies, &ledgerFake{})
	if _, err := submitter.Submit(context.Background(), submissionOf()); err != nil {
		t.Fatalf("submitting the copy: %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(started), &document); err != nil {
		t.Fatalf("the copy was started with something that is not a document: %v", err)
	}

	performance, ok := document["performance"].(map[string]any)
	if !ok {
		t.Fatalf("no performance block was sent; the copy would run at every default:\n%s", started)
	}
	if want := map[string]any{
		"largeCopyConcurrency": float64(4),
		"smallCopyBatchSize":   float64(8),
	}; !reflect.DeepEqual(performance, want) {
		t.Errorf("performance sent as %v, want %v; an unset setting must be omitted rather than sent as zero", performance, want)
	}
}

// An installation that configures nothing sends no performance block at all, rather than
// one full of zeroes that would ask the copy service for no concurrency whatsoever.
func TestSubmitSendsNoPerformanceBlockWhenNoneIsConfigured(t *testing.T) {
	var started string
	copies := pinnedCopies(submitClock)
	base := copies.StartExecutionFn
	copies.StartExecutionFn = func(ctx context.Context, params *sfn.StartExecutionInput) (*sfn.StartExecutionOutput, error) {
		started = aws.ToString(params.Input)
		return base(ctx, params)
	}

	config := submitConfig()
	config.Performance = CopyPerformance{}
	submitter := NewSubmitter(config, &awstest.S3{}, copies, &ledgerFake{})
	submitter.now = func() time.Time { return submitClock }
	if _, err := submitter.Submit(context.Background(), submissionOf()); err != nil {
		t.Fatalf("submitting the copy: %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(started), &document); err != nil {
		t.Fatalf("the copy was started with something that is not a document: %v", err)
	}
	if _, present := document["performance"]; present {
		t.Errorf("a performance block was sent although none is configured:\n%s", started)
	}
}

// submitWorkingRunPrefix is where in the working bucket a given run's own artefacts sit.
func submitWorkingRunPrefix(runID string) string {
	return submitWorkingPrefix + "/" + runID + "/"
}

// copyOf returns a plain request for the fixture dataset, which each test then adjusts to
// the one thing it is about.
func copyOf(datasetURI string) CopyRequest {
	return CopyRequest{
		RunID:             submitRunID,
		DatasetURI:        datasetURI,
		DestinationBucket: submitDestination,
	}
}

// submissionOf is the equivalent for a submitted copy.
func submissionOf() SubmitRequest {
	return SubmitRequest{
		RunID:             submitRunID,
		DatasetURI:        submitDatasetURI,
		DestinationBucket: submitDestination,
		KeyArn:            submitKeyArn,
	}
}

// ledgerFake is the slice of the ledger a submission touches. It records what it was
// asked to write, and can refuse, so both the ordering guarantee and what a failed write
// leaves behind are observable.
type ledgerFake struct {
	created []Copy
	err     error
	// onWrite runs before the record is kept, for tests observing the order of writes
	// across the three services.
	onWrite func()
}

func (l *ledgerFake) Record(_ context.Context, c *Copy) error {
	if l.onWrite != nil {
		l.onWrite()
	}
	l.created = append(l.created, *c)
	return l.err
}

func newTestSubmitter(t *testing.T, objects *awstest.S3, copies *awstest.StepFunctions, ledger CopyRecorder) *Submitter {
	t.Helper()

	submitter := NewSubmitter(submitConfig(), objects, copies, ledger)
	submitter.now = func() time.Time { return submitClock }
	return submitter
}

// pinnedCopies is a copy service whose executions start at a stated instant, so a
// recorded creation time can be compared against a constant.
func pinnedCopies(startedAt time.Time) *awstest.StepFunctions {
	return &awstest.StepFunctions{
		StartExecutionFn: func(_ context.Context, params *sfn.StartExecutionInput) (*sfn.StartExecutionOutput, error) {
			started := startedAt
			return &sfn.StartExecutionOutput{
				ExecutionArn: aws.String("arn:aws:states:ap-southeast-2:916218008751:execution:copy:" + aws.ToString(params.Name)),
				StartDate:    &started,
			}, nil
		},
	}
}

// assertNothingSubmitted fails unless a refusal was made before anything was written: no
// instruction document uploaded, no copy started, and nothing recorded.
func assertNothingSubmitted(t *testing.T, objects *awstest.S3, copies *awstest.StepFunctions, ledger *ledgerFake) {
	t.Helper()

	if len(objects.PutCalls) != 0 {
		t.Errorf("expected no instruction document to be uploaded, got %d upload(s)", len(objects.PutCalls))
	}
	if len(copies.StartCalls) != 0 {
		t.Errorf("expected no copy to be started, got %d execution(s)", len(copies.StartCalls))
	}
	if ledger != nil && len(ledger.created) != 0 {
		t.Errorf("expected nothing to be recorded, got %d record(s)", len(ledger.created))
	}
}

// decodeInstructionLines reads the instruction document back, checking as it goes that it
// is the line-per-object form the copy service expects: one compact object per line, each
// line terminated.
func decodeInstructionLines(t *testing.T, document []byte) []CopyInstruction {
	t.Helper()

	if len(document) == 0 {
		t.Fatal("the instruction document is empty")
	}
	if document[len(document)-1] != '\n' {
		t.Error("the instruction document does not end in a newline, so its last line is unterminated")
	}

	var decoded []CopyInstruction
	for i, line := range strings.Split(strings.TrimSuffix(string(document), "\n"), "\n") {
		if line == "" || strings.TrimSpace(line) != line {
			t.Errorf("line %d is not a single compact object: %q", i+1, line)
			continue
		}
		var instruction CopyInstruction
		if err := json.Unmarshal([]byte(line), &instruction); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i+1, err)
		}
		decoded = append(decoded, instruction)
	}
	return decoded
}

// startedInvocation decodes the document the state machine was started with.
func startedInvocation(t *testing.T, call sfn.StartExecutionInput) CopyInvocation {
	t.Helper()

	var invocation CopyInvocation
	if err := json.Unmarshal([]byte(aws.ToString(call.Input)), &invocation); err != nil {
		t.Fatalf("the copy was started with something that is not an invocation document: %v", err)
	}
	return invocation
}

// A dataset lives in a store this process holds no credentials for, so enumerating it
// here is not possible: the default is one wildcard the copy service expands with its own
// access.
//
// The wildcard carries no root folder key. The copy service refuses an instruction
// stating both, so an instruction that named the root here would be rejected before a
// single object was read -- and the folder is already named by the wildcard.
func TestPlanCopyLeavesEnumerationToTheCopyServiceByDefault(t *testing.T) {
	plan, err := PlanCopy(submitConfig(), copyOf(submitDatasetURI))
	if err != nil {
		t.Fatalf("planning the copy: %v", err)
	}

	want := []CopyInstruction{{
		SourceBucket: submitSourceBucket,
		SourceKey:    submitDatasetPrefix + "*",
	}}
	if !reflect.DeepEqual(plan.Instructions, want) {
		t.Errorf("instructions: expected %+v, got %+v", want, plan.Instructions)
	}
	if got := decodeInstructionLines(t, plan.InstructionsDocument); !reflect.DeepEqual(got, want) {
		t.Errorf("the document does not carry the instructions it was built from: got %+v", got)
	}
	if plan.Source != (DatasetLocation{Bucket: submitSourceBucket, Prefix: submitDatasetPrefix}) {
		t.Errorf("resolved source: got %+v", plan.Source)
	}
}

// Naming the objects is the only way to assert a checksum, since a checksum describes one
// object rather than a folder. The document's order comes from the keys, not from the
// order the caller happened to discover them in.
func TestPlanCopyNamesEachObjectRelativeToTheDatasetPrefix(t *testing.T) {
	req := copyOf(submitDatasetURI)
	req.Objects = []SourceObject{
		{Key: "sample2/reads.bam"},
		{Key: "sample1/reads.bam", Sums: "sha256:e3b0c44298fc1c14"},
		{Key: "notes&drafts/readme.txt"},
	}

	plan, err := PlanCopy(submitConfig(), req)
	if err != nil {
		t.Fatalf("planning the copy: %v", err)
	}

	want := []CopyInstruction{
		{
			SourceBucket:        submitSourceBucket,
			SourceKey:           submitDatasetPrefix + "notes&drafts/readme.txt",
			SourceRootFolderKey: submitDatasetPrefix,
		},
		{
			SourceBucket:        submitSourceBucket,
			SourceKey:           submitDatasetPrefix + "sample1/reads.bam",
			SourceRootFolderKey: submitDatasetPrefix,
			Sums:                "sha256:e3b0c44298fc1c14",
		},
		{
			SourceBucket:        submitSourceBucket,
			SourceKey:           submitDatasetPrefix + "sample2/reads.bam",
			SourceRootFolderKey: submitDatasetPrefix,
		},
	}
	if !reflect.DeepEqual(plan.Instructions, want) {
		t.Errorf("instructions: expected %+v, got %+v", want, plan.Instructions)
	}

	lines := decodeInstructionLines(t, plan.InstructionsDocument)
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("the document does not carry the instructions it was built from: got %+v", lines)
	}
	// Object keys are read against a bucket listing, so they are written as they are
	// rather than with their ampersands escaped.
	if !strings.Contains(string(plan.InstructionsDocument), "notes&drafts/readme.txt") {
		t.Errorf("expected the key to appear unescaped, got %s", plan.InstructionsDocument)
	}
	// Every instruction states the root of the structure being copied. Without it the
	// copy places objects under their base name alone, which flattens the dataset and
	// collides two files of the same name in different folders.
	for _, instruction := range plan.Instructions {
		if instruction.SourceRootFolderKey != submitDatasetPrefix {
			t.Errorf("%s: expected root folder %q, got %q",
				instruction.SourceKey, submitDatasetPrefix, instruction.SourceRootFolderKey)
		}
	}
}

// TestPlanCopyNeverStatesARootFolderAlongsideAWildcard guards the one combination the
// copy service refuses outright.
//
// It is worth its own test because the refusal is total and arrives late: the instruction
// is uploaded, the execution starts, and the run fails in the step that heads the source
// objects, before a single byte moves. Nothing this side can tell the two apart from a
// credentials problem or an unreachable endpoint, so the invariant is asserted here
// rather than diagnosed there.
func TestPlanCopyNeverStatesARootFolderAlongsideAWildcard(t *testing.T) {
	plans := map[string]*CopyPlan{}

	wildcard, err := PlanCopy(submitConfig(), copyOf(submitDatasetURI))
	if err != nil {
		t.Fatalf("planning the wildcard copy: %v", err)
	}
	plans["expanded by the copy service"] = wildcard

	req := copyOf(submitDatasetURI)
	req.Objects = []SourceObject{{Key: "reads/sample.bam"}, {Key: "reads/sample.bam.bai"}}
	listed, err := PlanCopy(submitConfig(), req)
	if err != nil {
		t.Fatalf("planning the enumerated copy: %v", err)
	}
	plans["enumerated by the caller"] = listed

	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			for _, instruction := range plan.Instructions {
				wildcarded := strings.HasSuffix(instruction.SourceKey, "*")
				switch {
				case wildcarded && instruction.SourceRootFolderKey != "":
					t.Errorf("%s states a root folder of %q alongside a wildcard; the copy "+
						"service refuses the two together",
						instruction.SourceKey, instruction.SourceRootFolderKey)
				case !wildcarded && instruction.SourceRootFolderKey == "":
					t.Errorf("%s states no root folder, so the copy would place it under its "+
						"base name alone and flatten the dataset", instruction.SourceKey)
				}
			}

			// The absence has to survive marshalling as an absence rather than as an
			// empty string, since the document is what the copy service reads.
			if plan.Instructions[0].SourceRootFolderKey == "" &&
				strings.Contains(string(plan.InstructionsDocument), "sourceRootFolderKey") {
				t.Errorf("the document carries a root folder key that was meant to be absent: %s",
					plan.InstructionsDocument)
			}
		})
	}
}

// The copy's own bookkeeping shares the destination bucket with the data, so where it is
// placed is the whole of what keeps it out of the researcher's way. The cost report is
// not placed there at all.
func TestPlanCopyKeepsTheCopysOwnArtefactsUnderTheControlPrefix(t *testing.T) {
	cfg := submitConfig()
	plan, err := PlanCopy(cfg, copyOf(submitDatasetURI))
	if err != nil {
		t.Fatalf("planning the copy: %v", err)
	}

	control := ControlPrefix + "/"
	for _, artefact := range []struct {
		what string
		key  string
	}{
		{"start marker", plan.Invocation.StartMarkerKey},
		{"summary", plan.Invocation.SummaryCsvKey},
		{"cost report", plan.Invocation.HTMLReportKey},
	} {
		if !strings.HasPrefix(artefact.key, control) {
			t.Errorf("%s key %q does not sit under %q", artefact.what, artefact.key, control)
		}
	}

	// The artefact keys are relative to the destination prefix, so the objects have to be
	// anchored at the bucket root for the control prefix to be a sibling of them.
	if plan.Invocation.DestinationPrefix != "" {
		t.Errorf("destination prefix: expected the bucket root, got %q", plan.Invocation.DestinationPrefix)
	}
	if plan.Invocation.HTMLReport {
		t.Error("the cost report must not be written into a bucket a researcher reads")
	}
	if !plan.Invocation.RetainHTMLReport {
		t.Error("expected the cost report to be kept in the working bucket instead")
	}
	if !plan.Invocation.RetainSummaryCsv {
		t.Error("expected a copy of the summary in the working bucket, which a purge of the dataset does not reach")
	}
	if plan.Invocation.ContinueOnError {
		t.Error("a dataset is released as a unit, so a run that cannot copy every object must fail")
	}
	if got, want := plan.Invocation.DestinationRequiredRegion, cfg.Region; got != want {
		t.Errorf("required region: expected %q, got %q", want, got)
	}

	if got, want := plan.StartMarkerURI, "s3://"+submitDestination+"/"+control+startMarkerFileName; got != want {
		t.Errorf("start marker: expected %q, got %q", want, got)
	}
	if got, want := plan.SummaryCsvURI, "s3://"+submitDestination+"/"+control+summaryCsvFileName; got != want {
		t.Errorf("summary: expected %q, got %q", want, got)
	}

	working := submitWorkingRunPrefix(submitRunID)
	if got, want := plan.RetainedSummaryCsvURI, "s3://"+submitWorkingBucket+"/"+working+control+summaryCsvFileName; got != want {
		t.Errorf("retained summary: expected %q, got %q", want, got)
	}
	if got, want := plan.RetainedHTMLReportURI, "s3://"+submitWorkingBucket+"/"+working+control+htmlReportFileName; got != want {
		t.Errorf("retained cost report: expected %q, got %q", want, got)
	}
}

// The instruction document is addressed twice over: absolutely, for the upload, and
// relative to the prefix the copy service was deployed with, for the invocation. Getting
// either wrong points the copy at a document that is not there.
func TestPlanCopyAddressesTheInstructionDocumentBothWays(t *testing.T) {
	plan, err := PlanCopy(submitConfig(), copyOf(submitDatasetURI))
	if err != nil {
		t.Fatalf("planning the copy: %v", err)
	}

	if plan.InstructionsBucket != submitWorkingBucket {
		t.Errorf("upload bucket: expected %q, got %q", submitWorkingBucket, plan.InstructionsBucket)
	}
	if got, want := plan.InstructionsKey, submitWorkingRunPrefix(submitRunID)+instructionsFileName; got != want {
		t.Errorf("upload key: expected %q, got %q", want, got)
	}
	if got, want := plan.Invocation.InstructionsPrefix, submitRunID+"/"; got != want {
		t.Errorf("invocation prefix: expected %q relative to the deployed prefix, got %q", want, got)
	}
	if got, want := plan.Invocation.InstructionsKey, instructionsFileName; got != want {
		t.Errorf("invocation key: expected %q, got %q", want, got)
	}
	if plan.StateMachineARN != submitStateMachine {
		t.Errorf("state machine: expected %q, got %q", submitStateMachine, plan.StateMachineARN)
	}
}

// Access is configured per bucket name, so only the source store gets an endpoint and a
// credential reference. The credentials themselves are named and never held.
func TestPlanCopyReachesTheSourceStoreAndNothingElseThroughIt(t *testing.T) {
	cfg := submitConfig()
	plan, err := PlanCopy(cfg, copyOf(submitDatasetURI))
	if err != nil {
		t.Fatalf("planning the copy: %v", err)
	}

	definitions := plan.Invocation.BucketDefinitions
	if len(definitions) != 1 {
		t.Fatalf("expected the source bucket alone to be configured, got %d definition(s): %+v",
			len(definitions), definitions)
	}
	source, ok := definitions[submitSourceBucket]
	if !ok {
		t.Fatalf("expected a definition for %q, got %+v", submitSourceBucket, definitions)
	}
	if source.CredentialProvider != credentialProviderSecret {
		t.Errorf("credential provider: expected %q, got %q", credentialProviderSecret, source.CredentialProvider)
	}
	if source.Secret != cfg.Source.CredentialsSecret { // pragma: allowlist secret
		t.Errorf("secret: expected %q, got %q", cfg.Source.CredentialsSecret, source.Secret)
	}
	if source.EndpointURL != cfg.Source.EndpointURL {
		t.Errorf("endpoint: expected %q, got %q", cfg.Source.EndpointURL, source.EndpointURL)
	}
	if source.Region != cfg.Source.Region {
		t.Errorf("region: expected %q, got %q", cfg.Source.Region, source.Region)
	}
	if !source.S3Compatible {
		t.Error("expected the source store to be addressed in S3-compatible mode")
	}

	// The region the source is declared to be in and the region the run requires it to be
	// in have to be the same value. The copy service defaults the requirement to the
	// region it was installed in, so leaving it unstated refuses any source configured as
	// being anywhere else -- through a check this code never made and could not explain.
	if plan.Invocation.SourceRequiredRegion != source.Region {
		t.Errorf("the run requires the source in region %q while declaring it in %q; the two "+
			"come from one configured value and must agree",
			plan.Invocation.SourceRequiredRegion, source.Region)
	}
	if _, present := definitions[submitDestination]; present {
		t.Error("the destination must not be reached through the source store's endpoint and credentials")
	}
}

func TestPlanCopyRefusals(t *testing.T) {
	underControlPrefix := copyOf(submitDatasetURI)
	underControlPrefix.Objects = []SourceObject{{Key: ControlPrefix + "/ENDED_COPY.csv"}}

	wildcardKey := copyOf(submitDatasetURI)
	wildcardKey.Objects = []SourceObject{{Key: "sample*/reads.bam"}}

	absoluteKey := copyOf(submitDatasetURI)
	absoluteKey.Objects = []SourceObject{{Key: "/sample1/reads.bam"}}

	relativeKey := copyOf(submitDatasetURI)
	relativeKey.Objects = []SourceObject{{Key: "sample1/../../etc/passwd"}}

	duplicateKey := copyOf(submitDatasetURI)
	duplicateKey.Objects = []SourceObject{{Key: "sample1/reads.bam"}, {Key: " sample1/reads.bam "}}

	sharedBucket := copyOf("s3://" + submitDestination + "/datasets/cohort-a/")

	noRunID := copyOf(submitDatasetURI)
	noRunID.RunID = ""

	unusableRunID := copyOf(submitDatasetURI)
	unusableRunID.RunID = "run id/with slashes"

	tests := []struct {
		name         string
		req          CopyRequest
		wantContains string
	}{
		{
			// The destination mirrors these keys, so an object here would land where the
			// researcher is denied and could overwrite the copy's own summary.
			name:         "an object key under the control prefix",
			req:          underControlPrefix,
			wantContains: "reserved for the copy's own artefacts",
		},
		{
			name:         "a wildcard in a named object",
			req:          wildcardKey,
			wantContains: "must not contain wildcards",
		},
		{
			name:         "an object key that is not relative to the dataset",
			req:          absoluteKey,
			wantContains: "cannot begin with a slash",
		},
		{
			name:         "an object key climbing out of the dataset",
			req:          relativeKey,
			wantContains: "relative path segment",
		},
		{
			name:         "the same object named twice",
			req:          duplicateKey,
			wantContains: "named more than once",
		},
		{
			// Access is per bucket name, so a shared name would route the destination
			// through the source store's endpoint and credentials.
			name:         "a source bucket that is also the destination",
			req:          sharedBucket,
			wantContains: "must not share a bucket name",
		},
		{
			name:         "no run identifier",
			req:          noRunID,
			wantContains: "a run identifier is required",
		},
		{
			name:         "a run identifier that cannot be a key prefix",
			req:          unusableRunID,
			wantContains: "must be alphanumeric",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PlanCopy(submitConfig(), tt.req)
			if err == nil {
				t.Fatalf("expected a refusal, got plan %+v", plan)
			}
			if !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("expected the refusal to mention %q, got: %v", tt.wantContains, err)
			}
		})
	}
}

// Nothing configured means nothing is reachable, and an operator should learn all of it
// from one run rather than one resource at a time.
func TestPlanCopyReportsEveryAbsentPrerequisiteAtOnce(t *testing.T) {
	_, err := PlanCopy(CopyConfig{AccountID: "916218008751"}, CopyRequest{
		RunID:      submitRunID,
		DatasetURI: submitDatasetURI,
	})
	if err == nil {
		t.Fatal("expected a report of the absent prerequisites")
	}

	var unmet *UnmetPrerequisitesError
	if !errors.As(err, &unmet) {
		t.Fatalf("expected the prerequisites to be reported together, got %T: %v", err, err)
	}
	for _, kind := range []string{
		"copy state machine",
		"copy working bucket",
		"source store endpoint",
		"source store credentials secret",
		"dataset cache bucket",
	} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("expected %q to be named, got: %v", kind, err)
		}
	}
}

func TestParseDatasetURI(t *testing.T) {
	tests := []struct {
		name         string
		uri          string
		want         DatasetLocation
		wantContains string
	}{
		{
			name: "a bucket and a terminated prefix",
			uri:  "s3://ceph-source/datasets/cohort-a/",
			want: DatasetLocation{Bucket: "ceph-source", Prefix: "datasets/cohort-a/"},
		},
		{
			// A prefix is concatenated with an object's relative key, so it is
			// normalised to end in a slash whether the caller wrote one or not.
			name: "a prefix the caller did not terminate",
			uri:  "s3://ceph-source/datasets/cohort-a",
			want: DatasetLocation{Bucket: "ceph-source", Prefix: "datasets/cohort-a/"},
		},
		{
			name: "no scheme at all",
			uri:  "ceph-source/datasets/cohort-a/",
			want: DatasetLocation{Bucket: "ceph-source", Prefix: "datasets/cohort-a/"},
		},
		{
			name: "a whole bucket as the dataset",
			uri:  "s3://ceph-source",
			want: DatasetLocation{Bucket: "ceph-source"},
		},
		{
			name:         "nothing",
			uri:          "   ",
			wantContains: "a dataset URI is required",
		},
		{
			name:         "another scheme",
			uri:          "gs://ceph-source/datasets/cohort-a/",
			wantContains: "only s3 is understood",
		},
		{
			name:         "a wildcard",
			uri:          "s3://ceph-source/datasets/cohort-*/",
			wantContains: "must not contain wildcards",
		},
		{
			name:         "no bucket",
			uri:          "s3://",
			wantContains: "names no bucket",
		},
		{
			name:         "an empty path segment",
			uri:          "s3://ceph-source/datasets//cohort-a/",
			wantContains: "empty path segment",
		},
		{
			name:         "a segment climbing out of the dataset",
			uri:          "s3://ceph-source/datasets/../secrets/",
			wantContains: "relative path segment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDatasetURI(tt.uri)
			if tt.wantContains != "" {
				if err == nil {
					t.Fatalf("expected a refusal, got %+v", got)
				}
				if !strings.Contains(err.Error(), tt.wantContains) {
					t.Errorf("expected the refusal to mention %q, got: %v", tt.wantContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsing %q: %v", tt.uri, err)
			}
			if got != tt.want {
				t.Errorf("expected %+v, got %+v", tt.want, got)
			}
		})
	}
}

// A record has to carry the execution it is waiting on, so the copy is started first. The
// reverse order would allow a record claiming a copy that does not exist.
func TestSubmitStartsTheCopyBeforeRecordingIt(t *testing.T) {
	var order []string
	objects := &awstest.S3{
		PutObjectFn: func(_ context.Context, _ *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			order = append(order, "upload")
			return &s3.PutObjectOutput{}, nil
		},
	}
	copies := pinnedCopies(submitClock)
	base := copies.StartExecutionFn
	copies.StartExecutionFn = func(ctx context.Context, params *sfn.StartExecutionInput) (*sfn.StartExecutionOutput, error) {
		order = append(order, "start")
		return base(ctx, params)
	}
	ledger := &ledgerFake{onWrite: func() { order = append(order, "record") }}

	submitter := newTestSubmitter(t, objects, copies, ledger)
	submission, err := submitter.Submit(context.Background(), submissionOf())
	if err != nil {
		t.Fatalf("submitting the copy: %v", err)
	}

	if want := []string{"upload", "start", "record"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order of writes: expected %v, got %v", want, order)
	}

	if len(objects.PutCalls) != 1 {
		t.Fatalf("expected one instruction document to be uploaded, got %d", len(objects.PutCalls))
	}
	upload := objects.PutCalls[0]
	if got, want := aws.ToString(upload.Bucket), submitWorkingBucket; got != want {
		t.Errorf("upload bucket: expected %q, got %q", want, got)
	}
	if got, want := aws.ToString(upload.Key), submitWorkingRunPrefix(submitRunID)+instructionsFileName; got != want {
		t.Errorf("upload key: expected %q, got %q", want, got)
	}
	if got, want := aws.ToString(upload.ContentType), instructionsContentType; got != want {
		t.Errorf("content type: expected %q, got %q", want, got)
	}

	if len(copies.StartCalls) != 1 {
		t.Fatalf("expected one copy to be started, got %d", len(copies.StartCalls))
	}
	start := copies.StartCalls[0]
	if got, want := aws.ToString(start.StateMachineArn), submitStateMachine; got != want {
		t.Errorf("state machine: expected %q, got %q", want, got)
	}
	// Named after the run, so resubmitting the same run cannot produce a second copy.
	if got, want := aws.ToString(start.Name), submitRunID; got != want {
		t.Errorf("execution name: expected %q, got %q", want, got)
	}
	if invocation := startedInvocation(t, start); invocation.DryRun {
		t.Error("expected a real copy, but the invocation asked for a dry run")
	}

	if len(ledger.created) != 1 {
		t.Fatalf("expected one record, got %d", len(ledger.created))
	}
	// A submission records a transfer and only a transfer: nothing here names a
	// researcher, because nothing has been given to one.
	assertCopiesEqual(t, &Copy{
		CopyID:           submitRunID,
		DatasetURI:       submitDatasetURI,
		HotCacheBucket:   submitDestination,
		KeyArn:           submitKeyArn,
		State:            CopyStateCopying,
		CreatedAt:        submitClock,
		CopyExecutionArn: submitExecutionArn,
	}, &ledger.created[0])

	if submission.Copy == nil {
		t.Fatal("expected the submission to report the record it created")
	}
	assertCopiesEqual(t, &ledger.created[0], submission.Copy)
	if submission.RunID != submitRunID {
		t.Errorf("run identifier: expected %q, got %q", submitRunID, submission.RunID)
	}
	if got, want := submission.WorkingPrefix, submitWorkingRunPrefix(submitRunID); got != want {
		t.Errorf("working prefix: expected %q, got %q", want, got)
	}
	if submission.ExecutionArn != submitExecutionArn {
		t.Errorf("execution: expected %q, got %q", submitExecutionArn, submission.ExecutionArn)
	}
	if !submission.StartedAt.Equal(submitClock) {
		t.Errorf("start time: expected %s, got %s", submitClock, submission.StartedAt)
	}
	if submission.DryRun {
		t.Error("expected the submission to report a real copy")
	}
}

// A copy identified by the run cannot be started twice, so a recorded copy takes the run
// identifier verbatim as its name.
func TestSubmitMintsARunIdentifierWhenNoneIsGiven(t *testing.T) {
	copies := pinnedCopies(submitClock)
	ledger := &ledgerFake{}
	submitter := newTestSubmitter(t, &awstest.S3{}, copies, ledger)

	req := submissionOf()
	req.RunID = ""
	submission, err := submitter.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("submitting the copy: %v", err)
	}

	if submission.RunID == "" {
		t.Fatal("expected a run identifier to be minted")
	}
	if !runIDPattern.MatchString(submission.RunID) {
		t.Errorf("the minted run identifier %q cannot be used as a key prefix", submission.RunID)
	}
	// The run identifier is the copy's identifier too, so a ledger record alone is
	// enough to find the copy's artefacts.
	if got := ledger.created[0].CopyID; got != submission.RunID {
		t.Errorf("copy identifier: expected the run identifier %q, got %q", submission.RunID, got)
	}
	if got, want := aws.ToString(copies.StartCalls[0].Name), submission.RunID; got != want {
		t.Errorf("execution name: expected %q, got %q", want, got)
	}
}

// A priced run stages nothing, so a record in the copying state would be waiting on a
// transfer that will never deliver anything.
func TestSubmitDryRunRecordsNothing(t *testing.T) {
	copies := pinnedCopies(submitClock)
	ledger := &ledgerFake{}
	submitter := newTestSubmitter(t, &awstest.S3{}, copies, ledger)

	req := submissionOf()
	req.RunID = ""
	req.DryRun = true
	submission, err := submitter.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("submitting the dry run: %v", err)
	}

	if len(ledger.created) != 0 {
		t.Errorf("expected nothing to be recorded, got %+v", ledger.created)
	}
	if submission.Copy != nil {
		t.Errorf("expected no record to be reported, got %+v", submission.Copy)
	}
	if !submission.DryRun {
		t.Error("expected the submission to report a dry run")
	}

	// Found by dataset alone, so an operator does not have to carry an identifier
	// between the run that prices the transfer and the one that performs it.
	if got, want := submission.RunID, EstimateRunID(submitDatasetURI); got != want {
		t.Errorf("run identifier: expected %q, got %q", want, got)
	}
	if invocation := startedInvocation(t, copies.StartCalls[0]); !invocation.DryRun {
		t.Error("expected the invocation to ask for a dry run, so no bytes are moved")
	}
}

// Estimating a dataset again must price it again, so a priced run's name carries the
// instant it was submitted rather than only the run it belongs to.
func TestSubmitDryRunIsNamedAfreshEachTime(t *testing.T) {
	copies := pinnedCopies(submitClock)
	submitter := newTestSubmitter(t, &awstest.S3{}, copies, nil)

	req := submissionOf()
	req.RunID = ""
	req.DryRun = true

	if _, err := submitter.Submit(context.Background(), req); err != nil {
		t.Fatalf("submitting the first dry run: %v", err)
	}
	later := submitClock.Add(time.Hour)
	submitter.now = func() time.Time { return later }
	if _, err := submitter.Submit(context.Background(), req); err != nil {
		t.Fatalf("submitting the second dry run: %v", err)
	}

	runID := EstimateRunID(submitDatasetURI)
	want := []string{
		runID + "-" + submitClock.Format("20060102T150405Z"),
		runID + "-" + later.Format("20060102T150405Z"),
	}
	var got []string
	for _, call := range copies.StartCalls {
		got = append(got, aws.ToString(call.Name))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("execution names: expected %v, got %v", want, got)
	}
}

// Learning there is nowhere to record the copy after starting it leaves exactly the
// orphan the ordering exists to avoid, so it is checked before anything is written.
func TestSubmitRefusesARecordedCopyItHasNoLedgerFor(t *testing.T) {
	objects, copies := &awstest.S3{}, &awstest.StepFunctions{}
	submitter := newTestSubmitter(t, objects, copies, nil)

	_, err := submitter.Submit(context.Background(), submissionOf())
	if err == nil {
		t.Fatal("expected the submission to be refused")
	}
	if !strings.Contains(err.Error(), "no access to the ledger") {
		t.Errorf("expected the refusal to name the ledger, got: %v", err)
	}
	assertNothingSubmitted(t, objects, copies, nil)
}

// A cost ceiling cannot be handed to the copy service, so it is applied here — and every
// way of failing to satisfy it has to refuse before anything is written, because an
// operator who asked for the transfer to be gated has not agreed to pay for it.
func TestSubmitRefusesAnUnsatisfiedCostCeilingBeforeWritingAnything(t *testing.T) {
	ceiling := func(usd float64) *float64 { return &usd }

	noEstimate := submissionOf()
	noEstimate.MaxCostUSD = ceiling(10)

	overCeiling := submissionOf()
	overCeiling.MaxCostUSD = ceiling(10)
	overCeiling.Estimate = &CostEstimate{
		CrossRegionUSD:          6,
		ColdStorageRetrievalUSD: 4,
		ComputeUSD:              1,
		ObjectCount:             12,
		TotalBytes:              4096,
	}

	ceilingOnDryRun := submissionOf()
	ceilingOnDryRun.DryRun = true
	ceilingOnDryRun.MaxCostUSD = ceiling(10)

	impossibleCeiling := submissionOf()
	impossibleCeiling.MaxCostUSD = ceiling(-1)

	tests := []struct {
		name  string
		req   SubmitRequest
		check func(t *testing.T, err error)
	}{
		{
			// The estimate can only come from a preceding priced run, so a ceiling
			// without one is a gate that has not been applied.
			name: "a ceiling with no estimate to check against",
			req:  noEstimate,
			check: func(t *testing.T, err error) {
				var required *EstimateRequiredError
				if !errors.As(err, &required) {
					t.Fatalf("expected an estimate to be demanded, got %T: %v", err, err)
				}
				if got, want := required.EstimateRunID, EstimateRunID(submitDatasetURI); got != want {
					t.Errorf("expected the refusal to say where the estimate is kept (%q), got %q", want, got)
				}
				if required.CeilingUSD != 10 {
					t.Errorf("ceiling: expected 10, got %v", required.CeilingUSD)
				}
			},
		},
		{
			name: "an estimate above the ceiling",
			req:  overCeiling,
			check: func(t *testing.T, err error) {
				var exceeded *CostCeilingExceededError
				if !errors.As(err, &exceeded) {
					t.Fatalf("expected the ceiling to be reported as exceeded, got %T: %v", err, err)
				}
				if got := exceeded.Estimate.TotalUSD(); got != 11 {
					t.Errorf("estimated total: expected 11, got %v", got)
				}
				if !strings.Contains(err.Error(), "no copy was started") {
					t.Errorf("expected the refusal to say nothing was started, got: %v", err)
				}
			},
		},
		{
			// The priced run is what produces the estimate and commits to nothing, so
			// the ceiling belongs on the copy that follows it.
			name: "a ceiling on the run that would produce the estimate",
			req:  ceilingOnDryRun,
			check: func(t *testing.T, err error) {
				if !strings.Contains(err.Error(), "cannot be applied to a dry run") {
					t.Errorf("expected the refusal to explain the ordering, got: %v", err)
				}
			},
		},
		{
			name: "a ceiling no transfer could meet",
			req:  impossibleCeiling,
			check: func(t *testing.T, err error) {
				if !strings.Contains(err.Error(), "cannot be met by any transfer") {
					t.Errorf("expected the refusal to reject the ceiling itself, got: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects, copies, ledger := &awstest.S3{}, &awstest.StepFunctions{}, &ledgerFake{}
			submitter := newTestSubmitter(t, objects, copies, ledger)

			submission, err := submitter.Submit(context.Background(), tt.req)
			if err == nil {
				t.Fatalf("expected the submission to be refused, got %+v", submission)
			}
			tt.check(t, err)
			assertNothingSubmitted(t, objects, copies, ledger)
		})
	}
}

// The gate refuses a transfer priced above the ceiling, not one priced at it.
func TestSubmitProceedsWhenTheEstimateMeetsTheCeiling(t *testing.T) {
	copies := pinnedCopies(submitClock)
	ledger := &ledgerFake{}
	submitter := newTestSubmitter(t, &awstest.S3{}, copies, ledger)

	req := submissionOf()
	ceiling := 10.0
	req.MaxCostUSD = &ceiling
	req.Estimate = &CostEstimate{CrossRegionUSD: 7, ComputeUSD: 3}

	if _, err := submitter.Submit(context.Background(), req); err != nil {
		t.Fatalf("expected the copy to be submitted, got: %v", err)
	}
	if len(copies.StartCalls) != 1 {
		t.Errorf("expected one copy to be started, got %d", len(copies.StartCalls))
	}
	if len(ledger.created) != 1 {
		t.Errorf("expected one record, got %d", len(ledger.created))
	}
}

// A copy that was started but not recorded is the one failure nothing else will notice:
// it proceeds, and no record is waiting on its outcome. The report has to name the
// execution so an operator can go and find it.
func TestSubmitReportsACopyItStartedButCouldNotRecord(t *testing.T) {
	refusal := errors.New("provisioned throughput exceeded")
	copies := pinnedCopies(submitClock)
	ledger := &ledgerFake{err: refusal}
	submitter := newTestSubmitter(t, &awstest.S3{}, copies, ledger)

	submission, err := submitter.Submit(context.Background(), submissionOf())
	if err == nil {
		t.Fatalf("expected the failed record to be reported, got %+v", submission)
	}

	var unrecorded *UnrecordedCopyError
	if !errors.As(err, &unrecorded) {
		t.Fatalf("expected an unrecorded copy to be reported, got %T: %v", err, err)
	}
	if unrecorded.ExecutionArn != submitExecutionArn {
		t.Errorf("execution: expected %q, got %q", submitExecutionArn, unrecorded.ExecutionArn)
	}
	if unrecorded.DatasetURI != submitDatasetURI {
		t.Errorf("dataset: expected %q, got %q", submitDatasetURI, unrecorded.DatasetURI)
	}
	if !errors.Is(err, refusal) {
		t.Errorf("expected the underlying failure to stay reachable, got: %v", err)
	}
	if !strings.Contains(err.Error(), submitExecutionArn) {
		t.Errorf("expected the message to name the execution, got: %v", err)
	}
	// The copy really is running, which is why the report says to deal with it.
	if len(copies.StartCalls) != 1 {
		t.Errorf("expected the copy to have been started, got %d execution(s)", len(copies.StartCalls))
	}
}

// A denial on the working bucket almost always means the copy service was installed
// somewhere else, so it is reported against that installation rather than as a bare AWS
// error — and nothing is started off the back of a document that was never written.
func TestSubmitReportsAnUnwritableWorkingBucketAgainstTheCopyService(t *testing.T) {
	objects := &awstest.S3{
		PutObjectFn: func(_ context.Context, _ *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
			return nil, errors.New("AccessDenied: not authorized to perform s3:PutObject")
		},
	}
	copies, ledger := &awstest.StepFunctions{}, &ledgerFake{}
	submitter := newTestSubmitter(t, objects, copies, ledger)

	_, err := submitter.Submit(context.Background(), submissionOf())
	if err == nil {
		t.Fatal("expected the failed upload to be reported")
	}
	var missing *MissingPrerequisiteError
	if !errors.As(err, &missing) {
		t.Fatalf("expected the working bucket to be named as the prerequisite, got %T: %v", err, err)
	}
	if got, want := missing.Prerequisite.Kind, "copy working bucket"; got != want {
		t.Errorf("prerequisite: expected %q, got %q", want, got)
	}
	if missing.Prerequisite.Identifier != submitWorkingBucket {
		t.Errorf("identifier: expected %q, got %q", submitWorkingBucket, missing.Prerequisite.Identifier)
	}
	if len(copies.StartCalls) != 0 {
		t.Errorf("expected no copy to be started without its instructions, got %d", len(copies.StartCalls))
	}
	if len(ledger.created) != 0 {
		t.Errorf("expected nothing to be recorded, got %+v", ledger.created)
	}
}

// A copy that could not be started leaves a stray instruction document and nothing else.
func TestSubmitReportsAnUnreachableCopyServiceAndRecordsNothing(t *testing.T) {
	copies := &awstest.StepFunctions{
		StartExecutionFn: func(_ context.Context, _ *sfn.StartExecutionInput) (*sfn.StartExecutionOutput, error) {
			return nil, errors.New("StateMachineDoesNotExist")
		},
	}
	ledger := &ledgerFake{}
	submitter := newTestSubmitter(t, &awstest.S3{}, copies, ledger)

	_, err := submitter.Submit(context.Background(), submissionOf())
	if err == nil {
		t.Fatal("expected the failed start to be reported")
	}
	var missing *MissingPrerequisiteError
	if !errors.As(err, &missing) {
		t.Fatalf("expected the state machine to be named as the prerequisite, got %T: %v", err, err)
	}
	if got, want := missing.Prerequisite.Kind, "copy state machine"; got != want {
		t.Errorf("prerequisite: expected %q, got %q", want, got)
	}
	if len(ledger.created) != 0 {
		t.Errorf("expected nothing to be recorded, got %+v", ledger.created)
	}
}

// estimateDocument is one of the per-object cost documents a run leaves in the working
// bucket.
type estimateDocument struct {
	body     string
	modified time.Time
}

// pricedObject renders one line of such a document.
func pricedObject(size int64, crossRegion, retrieval, compute float64) string {
	return fmt.Sprintf(
		`{"size":%d,"costEstimate":{"crossRegionCostUSD":%g,"coldStorageRetrievalCostUSD":%g,"computeCostUSD":%g}}`+"\n",
		size, crossRegion, retrieval, compute)
}

// estimateStore is an S3 double serving a fixed set of the copy's own documents. Fetching
// a key it was not given fails, so a test can prove a document was ignored rather than
// merely unaccounted for.
func estimateStore(documents map[string]estimateDocument) *awstest.S3 {
	return &awstest.S3{
		ListObjectsV2Fn: func(_ context.Context, params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			prefix := aws.ToString(params.Prefix)
			var contents []s3types.Object
			for key, document := range documents {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				modified := document.modified
				contents = append(contents, s3types.Object{Key: aws.String(key), LastModified: &modified})
			}
			return &s3.ListObjectsV2Output{Contents: contents}, nil
		},
		GetObjectFn: func(_ context.Context, params *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			key := aws.ToString(params.Key)
			document, ok := documents[key]
			if !ok {
				return nil, fmt.Errorf("nothing at %q should have been read", key)
			}
			return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(document.body))}, nil
		},
	}
}

// Every estimate of a dataset shares one prefix, so the reader has to take one run's
// documents alone. Adding two runs together would price the dataset twice.
func TestEstimatedCostTotalsTheMostRecentRunOnly(t *testing.T) {
	runID := EstimateRunID(submitDatasetURI)
	prefix := submitWorkingRunPrefix(runID)
	earlier := submitClock.Add(-2 * time.Hour)
	latest := submitClock.Add(30 * time.Minute)

	objects := estimateStore(map[string]estimateDocument{
		prefix + "older/small.jsonl": {
			body:     pricedObject(999, 90, 5, 5),
			modified: earlier,
		},
		prefix + "newer/small.jsonl": {
			body:     pricedObject(1024, 0.10, 0, 0.30) + pricedObject(2048, 0.20, 0, 0.40),
			modified: submitClock,
		},
		prefix + "newer/largeThaw.jsonl": {
			body:     pricedObject(8192, 1, 2.5, 0.5),
			modified: latest,
		},
		// Not one of the copy's per-object documents. It is priced extravagantly so that
		// reading it anyway would show up in the total rather than pass unnoticed.
		prefix + "newer/" + htmlReportFileName: {
			body:     pricedObject(1<<40, 1000, 1000, 1000),
			modified: latest,
		},
	})

	submitter := newTestSubmitter(t, objects, &awstest.StepFunctions{}, nil)
	estimate, err := submitter.EstimatedCost(context.Background(), runID)
	if err != nil {
		t.Fatalf("reading the estimate: %v", err)
	}

	if estimate.ObjectCount != 3 {
		t.Errorf("objects priced: expected 3, got %d", estimate.ObjectCount)
	}
	if estimate.TotalBytes != 1024+2048+8192 {
		t.Errorf("bytes priced: expected %d, got %d", 1024+2048+8192, estimate.TotalBytes)
	}
	if got, want := fmt.Sprintf("%.2f", estimate.TotalUSD()), "5.00"; got != want {
		t.Errorf("estimated total: expected %s, got %s", want, got)
	}
	if got, want := fmt.Sprintf("%.2f", estimate.CrossRegionUSD), "1.30"; got != want {
		t.Errorf("transfer: expected %s, got %s", want, got)
	}
	if got, want := fmt.Sprintf("%.2f", estimate.ColdStorageRetrievalUSD), "2.50"; got != want {
		t.Errorf("retrieval: expected %s, got %s", want, got)
	}
	if got, want := fmt.Sprintf("%.2f", estimate.ComputeUSD), "1.20"; got != want {
		t.Errorf("compute: expected %s, got %s", want, got)
	}
	if !estimate.ObservedAt.Equal(latest) {
		t.Errorf("observed at: expected %s, got %s", latest, estimate.ObservedAt)
	}

	// The report has to be able to show its working, and must not cite the run it
	// ignored.
	if len(estimate.Sources) != 2 {
		t.Errorf("sources: expected the two documents of one run, got %v", estimate.Sources)
	}
	for _, source := range estimate.Sources {
		if strings.Contains(source, "/older/") {
			t.Errorf("expected the superseded run to be left out, got %v", estimate.Sources)
		}
		if !strings.HasPrefix(source, "s3://"+submitWorkingBucket+"/") {
			t.Errorf("expected a working bucket location, got %q", source)
		}
	}
}

// A transfer nobody has priced must never look free: an absent estimate is reported as
// absent rather than as zero, which a ceiling check would wave through.
func TestEstimatedCostReportsAnAbsentEstimateRatherThanZero(t *testing.T) {
	runID := EstimateRunID(submitDatasetURI)
	prefix := submitWorkingRunPrefix(runID)

	tests := []struct {
		name      string
		documents map[string]estimateDocument
	}{
		{
			name:      "the run has not priced anything yet",
			documents: map[string]estimateDocument{},
		},
		{
			// A run with nothing to thaw still writes a document for the objects it did
			// not find, so present-but-empty is not the same as priced.
			name: "the documents exist but price no objects",
			documents: map[string]estimateDocument{
				prefix + "small.jsonl":     {body: "", modified: submitClock},
				prefix + "largeThaw.jsonl": {body: "", modified: submitClock},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submitter := newTestSubmitter(t, estimateStore(tt.documents), &awstest.StepFunctions{}, nil)

			estimate, err := submitter.EstimatedCost(context.Background(), runID)
			if err == nil {
				t.Fatalf("expected the estimate to be reported as absent, got %+v", estimate)
			}
			if estimate != nil {
				t.Errorf("expected no estimate at all, got %+v", estimate)
			}
			var unavailable *EstimateUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("expected an absent estimate to be reported, got %T: %v", err, err)
			}
			if got, want := unavailable.Location, "s3://"+submitWorkingBucket+"/"+prefix; got != want {
				t.Errorf("expected the report to say where it looked (%q), got %q", want, got)
			}
			if unavailable.RunID != runID {
				t.Errorf("run: expected %q, got %q", runID, unavailable.RunID)
			}
		})
	}
}

// A truncated listing that stopped at the first page would understate the cost of the
// transfer, which is the one direction a gate must not err in.
func TestEstimatedCostReadsEveryPageOfTheRunsOutputs(t *testing.T) {
	runID := EstimateRunID(submitDatasetURI)
	prefix := submitWorkingRunPrefix(runID)
	firstKey := prefix + "small.jsonl"
	secondKey := prefix + "large.jsonl"

	bodies := map[string]string{
		firstKey:  pricedObject(100, 1, 0, 0),
		secondKey: pricedObject(200, 2, 0, 0),
	}
	pages := []s3.ListObjectsV2Output{
		{
			Contents:              []s3types.Object{{Key: aws.String(firstKey), LastModified: &submitClock}},
			IsTruncated:           aws.Bool(true),
			NextContinuationToken: aws.String("page-2"),
		},
		{
			Contents:    []s3types.Object{{Key: aws.String(secondKey), LastModified: &submitClock}},
			IsTruncated: aws.Bool(false),
		},
	}

	var requestedTokens []string
	objects := &awstest.S3{
		ListObjectsV2Fn: func(_ context.Context, params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			requestedTokens = append(requestedTokens, aws.ToString(params.ContinuationToken))
			page := pages[len(requestedTokens)-1]
			return &page, nil
		},
		GetObjectFn: func(_ context.Context, params *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(bodies[aws.ToString(params.Key)]))}, nil
		},
	}

	submitter := newTestSubmitter(t, objects, &awstest.StepFunctions{}, nil)
	estimate, err := submitter.EstimatedCost(context.Background(), runID)
	if err != nil {
		t.Fatalf("reading the estimate: %v", err)
	}

	if want := []string{"", "page-2"}; !reflect.DeepEqual(requestedTokens, want) {
		t.Errorf("listing: expected pages %v, got %v", want, requestedTokens)
	}
	if estimate.ObjectCount != 2 {
		t.Errorf("objects priced: expected both pages, got %d", estimate.ObjectCount)
	}
	if estimate.TotalBytes != 300 {
		t.Errorf("bytes priced: expected 300, got %d", estimate.TotalBytes)
	}
}

// The estimate is found by dataset, since the run that produces it and the copy it gates
// are separate invocations.
func TestEstimatedCostForDatasetLooksUnderTheDatasetsOwnRun(t *testing.T) {
	var prefixes []string
	objects := &awstest.S3{
		ListObjectsV2Fn: func(_ context.Context, params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			prefixes = append(prefixes, aws.ToString(params.Prefix))
			return &s3.ListObjectsV2Output{}, nil
		},
	}
	submitter := newTestSubmitter(t, objects, &awstest.StepFunctions{}, nil)

	// Nothing has been priced, so this reports as absent; where it looked is the point.
	if _, err := submitter.EstimatedCostForDataset(context.Background(), submitDatasetURI); err == nil {
		t.Fatal("expected the estimate to be reported as absent")
	}

	want := []string{submitWorkingRunPrefix(EstimateRunID(submitDatasetURI))}
	if !reflect.DeepEqual(prefixes, want) {
		t.Errorf("expected the dataset's own run to be read (%v), got %v", want, prefixes)
	}
}

func TestEstimatedCostNeedsARunToLookUnder(t *testing.T) {
	submitter := newTestSubmitter(t, &awstest.S3{}, &awstest.StepFunctions{}, nil)

	if _, err := submitter.EstimatedCost(context.Background(), "  "); err == nil {
		t.Fatal("expected a run identifier to be required")
	} else if !strings.Contains(err.Error(), "run identifier is required") {
		t.Errorf("expected the refusal to say what is missing, got: %v", err)
	}
}

func TestEstimateRunIDIdentifiesTheDataset(t *testing.T) {
	// Stable per dataset, so two invocations agree on where the estimate is without
	// anything being carried between them.
	if got, want := EstimateRunID("  "+submitDatasetURI+"  "), EstimateRunID(submitDatasetURI); got != want {
		t.Errorf("expected the same identifier for the same dataset, got %q and %q", got, want)
	}
	if EstimateRunID(submitDatasetURI) == EstimateRunID("s3://ceph-source/datasets/cohort-b/") {
		t.Error("expected two datasets to keep their estimates apart")
	}
	// It becomes a prefix in the working bucket.
	if id := EstimateRunID(submitDatasetURI); !runIDPattern.MatchString(id) {
		t.Errorf("the identifier %q cannot be used as a key prefix", id)
	}
}
