package dataset

import (
	"errors"
	"strings"
	"testing"
)

const testAccountID = "916218008751"

// awsAPIError stands in for an AWS SDK error, so the tests can check it is still
// reachable through the report.
type awsAPIError struct{ msg string }

func (e *awsAPIError) Error() string { return e.msg }

func TestMissingPrerequisiteError_Message(t *testing.T) {
	tests := []struct {
		name        string
		prereq      Prerequisite
		cause       error
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:   "names the resource, account, owner, remedy and cause",
			prereq: CacheBucketPrerequisite("hot-cache-a1b2c3", testAccountID),
			cause:  &awsAPIError{msg: "NoSuchBucket: the specified bucket does not exist"},
			wantContain: []string{
				"dataset cache bucket",
				`"hot-cache-a1b2c3"`,
				"account " + testAccountID,
				"per-dataset deployment",
				"before staging it",
				"NoSuchBucket",
			},
		},
		{
			name:   "omits the cause fragment when there is no underlying error",
			prereq: CopyStateMachinePrerequisite("arn:aws:states:ap-southeast-2:916218008751:stateMachine:copy", testAccountID),
			cause:  nil,
			wantContain: []string{
				"copy state machine",
				"account " + testAccountID,
				"copy service",
			},
			wantAbsent: []string{"underlying error"},
		},
		{
			name:   "renders without an identifier",
			prereq: Prerequisite{Kind: "copy working bucket", AccountID: testAccountID},
			cause:  nil,
			wantContain: []string{
				"copy working bucket is missing or unusable in account " + testAccountID,
			},
			wantAbsent: []string{`""`},
		},
		{
			name:        "falls back to a generic noun when the kind is unset",
			prereq:      Prerequisite{},
			cause:       nil,
			wantContain: []string{"prerequisite is missing or unusable"},
			wantAbsent:  []string{"in account", "provided by"},
		},
		{
			name:   "omits an account for a prerequisite that has none",
			prereq: SourceEndpointPrerequisite("https://ceph.example.org"),
			cause:  &awsAPIError{msg: "dial tcp: i/o timeout"},
			wantContain: []string{
				"source store endpoint",
				`"https://ceph.example.org"`,
				"external source object store",
				"i/o timeout",
			},
			wantAbsent: []string{"in account"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.prereq.Missing(tt.cause).Error()
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("message %q does not contain %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("message %q unexpectedly contains %q", got, absent)
				}
			}
		})
	}
}

// TestMissingPrerequisiteError_DoesNotSurfaceCauseAlone guards the point of this helper:
// an operator must be told which resource and which account, not only what the API said.
func TestMissingPrerequisiteError_DoesNotSurfaceCauseAlone(t *testing.T) {
	cause := &awsAPIError{msg: "AccessDenied: User is not authorized to perform kms:DescribeKey"}
	err := DatasetKeyPrerequisite("alias/dataset-a1b2c3", testAccountID).Missing(cause)

	got := err.Error()
	if got == cause.Error() {
		t.Fatal("report is the bare underlying error")
	}
	if !strings.Contains(got, "dataset encryption key") || !strings.Contains(got, testAccountID) {
		t.Errorf("report %q must name the resource and the owning account", got)
	}
	if !strings.HasPrefix(got, "dataset encryption key") {
		t.Errorf("report %q should lead with the resource, not the API error", got)
	}
}

func TestMissingPrerequisiteError_UnwrapsToCause(t *testing.T) {
	cause := &awsAPIError{msg: "ResourceNotFoundException"}
	err := LedgerTablePrerequisite("data-releaser-ShareRecords", testAccountID).Missing(cause)

	if !errors.Is(err, cause) {
		t.Error("errors.Is should reach the underlying error")
	}
	var api *awsAPIError
	if !errors.As(err, &api) {
		t.Fatal("errors.As should reach the underlying error type")
	}
	if api.msg != cause.msg {
		t.Errorf("unwrapped error: got %q, want %q", api.msg, cause.msg)
	}
}

func TestPrerequisite_Missingf(t *testing.T) {
	err := ManifestBucketPrerequisite("", testAccountID).Missingf("manifest.bucket is not set in %s", "config.yaml")

	got := err.Error()
	if !strings.Contains(got, "manifest.bucket is not set in config.yaml") {
		t.Errorf("message %q does not contain the formatted cause", got)
	}
	if !strings.Contains(got, "manifest bucket") {
		t.Errorf("message %q does not name the resource", got)
	}
}

func TestPrerequisiteReport_SatisfiedPrerequisites(t *testing.T) {
	var report PrerequisiteReport

	if ok := report.Record(CacheBucketPrerequisite("hot-cache-a1b2c3", testAccountID), nil); !ok {
		t.Error("Record with no cause should report the prerequisite as satisfied")
	}
	report.Add(nil)

	if len(report.Missing()) != 0 {
		t.Errorf("expected no failures, got %d", len(report.Missing()))
	}
	if err := report.Err(); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// TestPrerequisiteReport_CollectsEveryFailure covers the convention that one pass reports
// everything it found, so a single run is enough to fix all of it.
func TestPrerequisiteReport_CollectsEveryFailure(t *testing.T) {
	var report PrerequisiteReport

	if ok := report.Record(CopyWorkingBucketPrerequisite("copy-working", testAccountID),
		&awsAPIError{msg: "NoSuchBucket"}); ok {
		t.Error("Record with a cause should report the prerequisite as unsatisfied")
	}
	report.Record(SourceCredentialsPrerequisite("source/ceph", testAccountID),
		&awsAPIError{msg: "ResourceNotFoundException"})
	report.Add(ResearcherVpcPrerequisite("vpc-0123456789abcdef0", "534840902377").
		Missingf("no VPC identifier supplied"))

	missing := report.Missing()
	if len(missing) != 3 {
		t.Fatalf("expected 3 failures, got %d", len(missing))
	}
	// Order is the order the checks ran in, so the output follows the check sequence.
	wantKinds := []string{"copy working bucket", "source store credentials secret", "researcher VPC access path"}
	for i, want := range wantKinds {
		if missing[i].Prerequisite.Kind != want {
			t.Errorf("failure %d: got kind %q, want %q", i, missing[i].Prerequisite.Kind, want)
		}
	}

	err := report.Err()
	if err == nil {
		t.Fatal("expected an error for a report with failures")
	}
	got := err.Error()
	for _, want := range []string{
		"unmet prerequisites:",
		"copy working bucket",
		"source store credentials secret",
		"researcher VPC access path",
		"534840902377",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("aggregate message %q does not contain %q", got, want)
		}
	}
	if lines := strings.Count(got, "\n  - "); lines != 3 {
		t.Errorf("expected 3 listed failures, got %d in %q", lines, got)
	}
}

func TestUnmetPrerequisitesError_UnwrapsToEachFailure(t *testing.T) {
	notFound := &awsAPIError{msg: "ResourceNotFoundException"}

	var report PrerequisiteReport
	report.Record(CopyWorkingBucketPrerequisite("copy-working", testAccountID), errors.New("boom"))
	report.Record(DatasetKeyPrerequisite("alias/dataset-a1b2c3", testAccountID), notFound)

	err := report.Err()
	if !errors.Is(err, notFound) {
		t.Error("errors.Is should reach a cause held by one of the collected failures")
	}
	var missing *MissingPrerequisiteError
	if !errors.As(err, &missing) {
		t.Fatal("errors.As should reach a collected failure")
	}
	if missing.Prerequisite.Kind != "copy working bucket" {
		t.Errorf("errors.As found %q, want the first collected failure", missing.Prerequisite.Kind)
	}
}

// TestPrerequisiteConstructors_AttributeAnOwner checks that every named prerequisite
// carries the identifier, account, and an owner, since a report without an owner leaves
// the operator with nowhere to go.
func TestPrerequisiteConstructors_AttributeAnOwner(t *testing.T) {
	tests := []struct {
		name       string
		prereq     Prerequisite
		wantID     string
		wantAcct   string
		ownerMatch string
	}{
		{"cache bucket", CacheBucketPrerequisite("hot-cache-a1b2c3", testAccountID), "hot-cache-a1b2c3", testAccountID, "per-dataset"},
		{"dataset key", DatasetKeyPrerequisite("alias/dataset-a1b2c3", testAccountID), "alias/dataset-a1b2c3", testAccountID, "per-dataset"},
		{"ledger table", LedgerTablePrerequisite("data-releaser-ShareRecords", testAccountID), "data-releaser-ShareRecords", testAccountID, "shared deployment"},
		{"manifest bucket", ManifestBucketPrerequisite("manifests", testAccountID), "manifests", testAccountID, "shared deployment"},
		{"copy state machine", CopyStateMachinePrerequisite("arn:aws:states:ap-southeast-2:916218008751:stateMachine:copy", testAccountID), "arn:aws:states:ap-southeast-2:916218008751:stateMachine:copy", testAccountID, "copy service"},
		{"copy working bucket", CopyWorkingBucketPrerequisite("copy-working", testAccountID), "copy-working", testAccountID, "copy service"},
		{"source credentials", SourceCredentialsPrerequisite("source/ceph", testAccountID), "source/ceph", testAccountID, "source object store"},
		{"source endpoint", SourceEndpointPrerequisite("https://ceph.example.org"), "https://ceph.example.org", "", "source object store"},
		{"researcher VPC", ResearcherVpcPrerequisite("vpc-0123456789abcdef0", "534840902377"), "vpc-0123456789abcdef0", "534840902377", "researcher account"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prereq.Kind == "" {
				t.Error("prerequisite has no kind")
			}
			if tt.prereq.Identifier != tt.wantID {
				t.Errorf("identifier: got %q, want %q", tt.prereq.Identifier, tt.wantID)
			}
			if tt.prereq.AccountID != tt.wantAcct {
				t.Errorf("account: got %q, want %q", tt.prereq.AccountID, tt.wantAcct)
			}
			if !strings.Contains(tt.prereq.Owner, tt.ownerMatch) {
				t.Errorf("owner %q does not mention %q", tt.prereq.Owner, tt.ownerMatch)
			}
			if tt.prereq.Remedy == "" {
				t.Error("prerequisite has no remedy")
			}
		})
	}
}

func TestPrerequisite_String(t *testing.T) {
	tests := []struct {
		name   string
		prereq Prerequisite
		want   string
	}{
		{"kind and identifier", Prerequisite{Kind: "manifest bucket", Identifier: "manifests"}, `manifest bucket "manifests"`},
		{"kind only", Prerequisite{Kind: "manifest bucket"}, "manifest bucket"},
		{"blank identifier", Prerequisite{Kind: "manifest bucket", Identifier: "   "}, "manifest bucket"},
		{"empty", Prerequisite{}, "prerequisite"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.prereq.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
