package dataset

import (
	"fmt"
	"strings"
)

// Prerequisite identifies a resource the releaser depends on, together with who is
// responsible for providing it.
//
// It exists so that an absent resource is reported as the thing that is missing and the
// party who has to supply it, rather than as whichever AWS call happened to notice.
// "bucket not found" tells an operator nothing about whether they should deploy a stack,
// chase another team, or fix a config field; naming the resource and its owner does.
type Prerequisite struct {
	// Kind is what the resource is, in an operator's words -- "cache bucket", "copy
	// state machine". Lower case, since it is used mid-sentence.
	Kind string
	// Identifier is the name or ARN the resource is addressed by. Optional, for the
	// cases where nothing more specific than the kind is known yet.
	Identifier string
	// AccountID is the AWS account that holds the resource.
	AccountID string
	// Owner names the deployment or party responsible for providing it, so the report
	// points somewhere actionable. Optional.
	Owner string
	// Remedy is what the operator should do about it. Optional.
	Remedy string
}

// String renders the resource as "<kind> \"<identifier>\"", or just the kind when no
// identifier is known.
func (p Prerequisite) String() string {
	kind := strings.TrimSpace(p.Kind)
	if kind == "" {
		kind = "prerequisite"
	}
	if id := strings.TrimSpace(p.Identifier); id != "" {
		return fmt.Sprintf("%s %q", kind, id)
	}
	return kind
}

// Missing reports the prerequisite as absent or unusable. cause may be nil; when it is
// present it is wrapped and reported as supporting detail, never as the whole message.
func (p Prerequisite) Missing(cause error) *MissingPrerequisiteError {
	return &MissingPrerequisiteError{Prerequisite: p, Cause: cause}
}

// Missingf reports the prerequisite as absent or unusable with a described cause, for
// the failures that come from inspecting configuration rather than from an API call.
func (p Prerequisite) Missingf(format string, args ...any) *MissingPrerequisiteError {
	return p.Missing(fmt.Errorf(format, args...))
}

// MissingPrerequisiteError is an unmet prerequisite: what is missing, which account
// holds it, who provides it, and what the operator can do, with any underlying error
// carried along as detail rather than standing in for the explanation.
type MissingPrerequisiteError struct {
	Prerequisite Prerequisite
	// Cause is the error that revealed the absence, if any.
	Cause error
}

// Error renders the report. Every part except the resource itself is optional, so a
// caller that knows only the kind still produces a sentence.
func (e *MissingPrerequisiteError) Error() string {
	var b strings.Builder
	b.WriteString(e.Prerequisite.String())
	b.WriteString(" is missing or unusable")
	if account := strings.TrimSpace(e.Prerequisite.AccountID); account != "" {
		fmt.Fprintf(&b, " in account %s", account)
	}
	if owner := strings.TrimSpace(e.Prerequisite.Owner); owner != "" {
		fmt.Fprintf(&b, "; provided by %s", owner)
	}
	if remedy := strings.TrimSpace(e.Prerequisite.Remedy); remedy != "" {
		fmt.Fprintf(&b, "; %s", remedy)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, " (underlying error: %v)", e.Cause)
	}
	return b.String()
}

// Unwrap exposes the underlying error so callers can still test for specific AWS error
// types, while operators read the account-and-owner report instead of that error.
func (e *MissingPrerequisiteError) Unwrap() error {
	return e.Cause
}

// PrerequisiteReport accumulates unmet prerequisites so a preflight or validation pass
// reports all of them at once instead of stopping at the first, which is what makes a
// single run enough to fix everything.
//
// A report is not safe for concurrent use; give each check sequence its own.
type PrerequisiteReport struct {
	missing []*MissingPrerequisiteError
}

// Record notes the prerequisite as unmet when cause is non-nil, and reports whether it
// was satisfied. A nil cause records nothing, so a check reads as
// `if !report.Record(prereq, err) { return }`.
func (r *PrerequisiteReport) Record(p Prerequisite, cause error) bool {
	if cause == nil {
		return true
	}
	r.missing = append(r.missing, p.Missing(cause))
	return false
}

// Add appends an already-built report, for callers that construct it themselves.
// A nil argument is ignored.
func (r *PrerequisiteReport) Add(missing *MissingPrerequisiteError) {
	if missing == nil {
		return
	}
	r.missing = append(r.missing, missing)
}

// Missing returns the failures collected so far, in the order they were recorded.
func (r *PrerequisiteReport) Missing() []*MissingPrerequisiteError {
	return r.missing
}

// Err returns nil when every prerequisite was satisfied, and otherwise one error
// listing all of them.
func (r *PrerequisiteReport) Err() error {
	if len(r.missing) == 0 {
		return nil
	}
	return &UnmetPrerequisitesError{Missing: r.missing}
}

// UnmetPrerequisitesError reports every prerequisite a single pass found unmet.
type UnmetPrerequisitesError struct {
	Missing []*MissingPrerequisiteError
}

// Error lists each unmet prerequisite on its own line, matching how configuration
// failures are already reported.
func (e *UnmetPrerequisitesError) Error() string {
	var b strings.Builder
	b.WriteString("unmet prerequisites:")
	for _, missing := range e.Missing {
		b.WriteString("\n  - ")
		b.WriteString(missing.Error())
	}
	return b.String()
}

// Unwrap exposes each failure, so a caller can reach an individual report and the error
// beneath it.
func (e *UnmetPrerequisitesError) Unwrap() []error {
	errs := make([]error, len(e.Missing))
	for i, missing := range e.Missing {
		errs[i] = missing
	}
	return errs
}

// Who provides what. Held here rather than at each call site so that every check
// attributes the same resource to the same owner.
const (
	// ownerPerDatasetDeployment provisions one cache bucket and one key per dataset.
	ownerPerDatasetDeployment = "this project's per-dataset deployment"
	// ownerSharedDeployment provisions the resources shared across every dataset and
	// both release mechanisms.
	ownerSharedDeployment = "this project's shared deployment"
	// ownerCopyService is installed and operated separately; this project only submits
	// work to it.
	ownerCopyService = "the separately deployed copy service"
	// ownerSourceStore operates the external object store holding the authoritative
	// copy of the data.
	ownerSourceStore = "the operator of the external source object store"
	// ownerResearcherAccount is responsible for everything on the reading side.
	ownerResearcherAccount = "the researcher account"
)

// CacheBucketPrerequisite describes the bucket holding the copied objects for one
// dataset.
func CacheBucketPrerequisite(bucket, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "dataset cache bucket",
		Identifier: bucket,
		AccountID:  accountID,
		Owner:      ownerPerDatasetDeployment,
		Remedy:     "deploy the per-dataset resources for this dataset before staging it",
	}
}

// DatasetKeyPrerequisite describes the customer-managed key that encrypts exactly one
// dataset's cache bucket.
func DatasetKeyPrerequisite(keyID, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "dataset encryption key",
		Identifier: keyID,
		AccountID:  accountID,
		Owner:      ownerPerDatasetDeployment,
		Remedy:     "deploy the per-dataset resources for this dataset before staging it",
	}
}

// LedgerTablePrerequisite describes the table holding both mechanisms' records.
func LedgerTablePrerequisite(table, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "release ledger table",
		Identifier: table,
		AccountID:  accountID,
		Owner:      ownerSharedDeployment,
		Remedy:     "deploy the shared resources and point the configured table name at their output",
	}
}

// ManifestBucketPrerequisite describes the bucket the researcher-readable access
// manifests are published to.
func ManifestBucketPrerequisite(bucket, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "manifest bucket",
		Identifier: bucket,
		AccountID:  accountID,
		Owner:      ownerSharedDeployment,
		Remedy:     "deploy the shared resources and point the configured bucket name at their output",
	}
}

// CopyStateMachinePrerequisite describes the state machine that performs the transfer
// from the source store into a cache bucket.
func CopyStateMachinePrerequisite(stateMachineARN, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "copy state machine",
		Identifier: stateMachineARN,
		AccountID:  accountID,
		Owner:      ownerCopyService,
		Remedy:     "install the copy service in this account, allowing it to write into the account it is installed in",
	}
}

// CopyWorkingBucketPrerequisite describes the bucket the copy service reads its
// instructions from and writes its reports to.
func CopyWorkingBucketPrerequisite(bucket, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "copy working bucket",
		Identifier: bucket,
		AccountID:  accountID,
		Owner:      ownerCopyService,
		Remedy:     "point the configured working bucket at the one the copy service installation created",
	}
}

// SourceCredentialsPrerequisite describes the secret holding the credentials for the
// external source store. The secret is held here; the credentials in it are not ours to
// issue.
func SourceCredentialsPrerequisite(secretID, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "source store credentials secret",
		Identifier: secretID,
		AccountID:  accountID,
		Owner:      ownerSourceStore,
		Remedy:     "store the source store credentials in this secret and grant the copy service read access to it",
	}
}

// SourceEndpointPrerequisite describes the network route to the external source store.
func SourceEndpointPrerequisite(endpoint string) Prerequisite {
	return Prerequisite{
		Kind:       "source store endpoint",
		Identifier: endpoint,
		Owner:      ownerSourceStore,
		Remedy:     "confirm the endpoint resolves and is routable from the subnets the copy service runs in",
	}
}

// ResearcherVpcPrerequisite describes the reading side of a release: the VPC access is
// restricted to, its S3 endpoint, and the permissions the researcher's own principals
// need. None of it is provisioned here, so a denial on that side is reported rather than
// fixed.
func ResearcherVpcPrerequisite(vpcID, accountID string) Prerequisite {
	return Prerequisite{
		Kind:       "researcher VPC access path",
		Identifier: vpcID,
		AccountID:  accountID,
		Owner:      ownerResearcherAccount,
		Remedy:     "have the researcher confirm the VPC identifier, its S3 endpoint, and that their principals are permitted to read",
	}
}
