package validate

// The checks in this file cover the mechanism that copies a dataset into this account and
// then grants researcher accounts direct read access to that copy.
//
// They exist because that mechanism's enforcement point is not in this process. Access is
// two policy documents that AWS evaluates on every request, and either one can be edited
// in a console. So the ledger is a declaration of intended access and the deployed
// documents are what a request is actually checked against, and the only way to know the
// two still agree is to read both and compare them. Drift in either direction is invisible
// from outside: a grant added by hand is data being read by someone no record mentions,
// and a grant removed by hand is a denial the researcher cannot account for.
//
// The ledger holds that intent in two kinds of record, and the comparison needs both. A
// copy says where a dataset's bytes staged -- which bucket, under which key -- and the
// grants against it say who may read them and from where. Neither alone describes a
// deployed document: the copy names the documents, the grants are their contents.
//
// Everything else here is a prerequisite a copy depends on. Each is checked because each
// fails late and expensively: a working bucket that cannot be written to is discovered
// after a copy has been started, and a source credential that was never granted surfaces
// hours into a transfer. Each is reported by name together with the party who provides it,
// since the useful answer to "the copy will not start" is which deployment to go to.
//
// Every check runs whatever the ones before it found. A deployment with three problems
// should take one run to discover, not three.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/dataset"
)

// DatasetCopyTargets locates the copy orchestration and the bucket it works in. Both
// belong to a separate installation, so both are reported against that installation
// rather than against this one.
type DatasetCopyTargets struct {
	// StateMachineARN is the orchestration a copy is started on.
	StateMachineARN string

	// AccountID is the account it is installed in. Empty means the account holding the
	// dataset buckets.
	AccountID string

	// WorkingBucket is where it reads its instructions from and writes its own outputs
	// to, and WorkingBucketPrefix the prefix within that bucket it was deployed with.
	WorkingBucket       string
	WorkingBucketPrefix string
}

// DatasetSourceStore describes the external object store datasets are copied out of.
type DatasetSourceStore struct {
	// EndpointURL is the store's custom S3 endpoint.
	EndpointURL string

	// CredentialsSecret names the secret the copy orchestration resolves the store's
	// credentials from. Only the identifier is held here; the credentials themselves
	// never pass through this tool.
	CredentialsSecret string
}

// DatasetCopyLedger is the read this pass makes of the copies: every transfer recorded,
// whatever state each is in.
//
// The failed and the purged are wanted as much as the staged. A copy that never delivered
// anything still named a bucket and a key, and a statement left deployed on that pair is
// access nothing accounts for -- which is exactly what a comparison that only looked at
// the healthy records would fail to notice.
type DatasetCopyLedger interface {
	ListAll(ctx context.Context) ([]*dataset.Copy, error)
}

// DatasetGrantLedger is the same read of the grants.
//
// It is deliberately the whole ledger rather than one account's grants, even when the pass
// has been narrowed to one researcher. The two documents are shared by every grant against
// a dataset, so a comparison made from one account's grants would be made against a
// rendered pair missing every other account's statements, and would report each of them as
// drift.
type DatasetGrantLedger interface {
	ListAll(ctx context.Context) ([]*dataset.Grant, error)
}

// ValidateDatasetOpts holds the dependencies for the direct-read checks.
type ValidateDatasetOpts struct {
	// Copies and Grants are the two halves of the ledger the intended access is read
	// from, and LedgerTable the table they share, carried separately so an unreadable
	// ledger can be reported by name.
	Copies      DatasetCopyLedger
	Grants      DatasetGrantLedger
	LedgerTable string

	S3Client            internalaws.S3Client
	KMSClient           internalaws.KMSClient
	StepFunctionsClient internalaws.StepFunctionsClient

	// PolicySettings must be the settings the deployed documents were applied with. The
	// documents each dataset is compared against are rendered rather than described, so a
	// comparison made with different settings would report a difference in everything.
	PolicySettings dataset.PolicySettings

	Copy   DatasetCopyTargets
	Source DatasetSourceStore

	// ResearcherAccountID narrows the comparison to the datasets one account holds a
	// grant against. Empty covers every dataset, including those granted to nobody.
	ResearcherAccountID string

	// Reconcile replaces the deployed documents of every dataset whose access differs
	// from the ledger's. It writes, so it is never implied: nothing here mutates unless
	// the operator asked for it by name.
	Reconcile bool

	// Now is the clock expiry and scheduled key deletion are judged against. Zero means
	// the current time.
	Now time.Time
}

// DatasetValidation is what one pass found, and what it changed.
type DatasetValidation struct {
	// Checks is every check that ran, in the order it ran.
	Checks []Check

	// Repairs is one result per dataset whose documents were re-applied, and is empty
	// unless reconciliation was asked for. They are kept apart from Checks because they
	// are not observations: each one wrote to a bucket or a key policy, and an operator
	// reading the output should not have to work out which of the two a line is.
	Repairs []Check
}

// Results returns every result the pass produced, for a caller summarising or deciding an
// exit status. A repair that failed counts as a failure like any other.
func (v DatasetValidation) Results() []Check {
	results := make([]Check, 0, len(v.Checks)+len(v.Repairs))
	results = append(results, v.Checks...)
	return append(results, v.Repairs...)
}

// ValidateDataset checks the prerequisites a copy depends on, compares the access the
// ledger describes for each dataset against the access its deployed documents grant, and
// reports the state of every key a dataset is encrypted under. Where reconciliation was
// asked for, it also replaces the documents of every dataset that differs.
//
// Nothing stops at the first failure. The ledger is read first because the comparisons and
// the key states all come from it, and because a copy that has already run is the best
// evidence available that the copy orchestration is the one configured.
func ValidateDataset(ctx context.Context, opts ValidateDatasetOpts) DatasetValidation {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	accountID := strings.TrimSpace(opts.PolicySettings.AccountID)

	var v DatasetValidation

	recorded, ledger := readLedger(ctx, opts, accountID)
	v.Checks = append(v.Checks,
		ledger,
		checkCopyOrchestration(ctx, opts.StepFunctionsClient, opts.Copy, recorded.copies, accountID),
		checkCopyWorkingBucket(ctx, opts.S3Client, opts.Copy, accountID, now),
		checkSourceCredentials(opts.Source, copyAccount(opts.Copy, accountID)),
		checkSourceEndpoint(opts.Source),
	)

	if len(recorded.copies) == 0 {
		v.Checks = append(v.Checks, skip(deployedAccessCheck,
			"no dataset copies are recorded, so there is no intended access to compare the "+
				"deployed policies against"))
		return v
	}

	differ := dataset.NewPolicyDiffer(opts.PolicySettings, opts.S3Client, opts.KMSClient)
	applier := dataset.NewPolicyApplier(opts.PolicySettings, opts.S3Client, opts.KMSClient)

	// Compared once per dataset, rather than once per copy or once per grant. The bucket
	// and the key are deployed per dataset and hold one pair of documents between them, so
	// every copy of that dataset and every grant made against those copies is enforced by
	// that same pair. Comparing against a pair rendered from any smaller unit -- one copy
	// of a dataset staged twice, one grant of a dataset released to two researchers --
	// reports everything the rest of the ledger describes as drift nobody accounts for, and
	// reconciliation then "repairs" it by writing that pair over grants doing exactly what
	// the ledger asks.
	for _, group := range recorded.groupedBy(func(c *dataset.Copy) string { return c.HotCacheBucket }) {
		check, diff := checkDeployedAccess(ctx, differ, group, now)
		v.Checks = append(v.Checks, check)

		if !opts.Reconcile || (diff != nil && diff.Matches()) {
			continue
		}
		if check.Skipped {
			// The dataset has nothing left to enforce, so there is nothing to put back.
			continue
		}
		v.Repairs = append(v.Repairs, reapplyLedgerPolicies(ctx, applier, group, diff))
	}

	for _, group := range recorded.groupedBy(func(c *dataset.Copy) string { return c.KeyArn }) {
		v.Checks = append(v.Checks, checkDatasetKey(ctx, opts.KMSClient, group, accountID, now))
	}

	return v
}

// Check names. The per-dataset checks name their dataset as well, so only the ones that
// stand alone are fixed here.
const (
	datasetLedgerCheck     = "Dataset ledger readable"
	copyOrchestrationCheck = "Copy orchestration reachability"
	workingBucketCheck     = "Copy working bucket writability"
	sourceCredentialsCheck = "Source store credentials secret"
	sourceEndpointCheck    = "Source store endpoint"
	deployedAccessCheck    = "Deployed access matches the ledger"
)

// datasetLedger is the intended access one pass read: the copies it covers, and every
// grant made against them.
type datasetLedger struct {
	copies []*dataset.Copy
	grants []*dataset.Grant
}

// readLedger reads the intended access from both halves of the ledger.
//
// A ledger that cannot be read is a failure of its own rather than one repeated against
// every dataset: nothing downstream of it can run, and reporting it once says so. Which of
// the two reads failed is named, since a table that answers for copies and not for grants
// is a different problem from one that answers for neither.
func readLedger(ctx context.Context, opts ValidateDatasetOpts, accountID string) (datasetLedger, Check) {
	name := checkOf(datasetLedgerCheck, opts.LedgerTable)
	prerequisite := dataset.LedgerTablePrerequisite(opts.LedgerTable, accountID)

	if opts.Copies == nil || opts.Grants == nil {
		return datasetLedger{}, fail(name, "no ledger was configured to read the intended access from")
	}

	copies, err := opts.Copies.ListAll(ctx)
	if err != nil {
		return datasetLedger{}, fail(name, "%v", prerequisite.Missing(fmt.Errorf(
			"reading the copies the deployed policies are compared against: %w", err)))
	}
	grants, err := opts.Grants.ListAll(ctx)
	if err != nil {
		return datasetLedger{}, fail(name, "%v", prerequisite.Missing(fmt.Errorf(
			"reading the grants the deployed policies are compared against: %w", err)))
	}

	recorded := datasetLedger{copies: copies, grants: grants}
	if account := strings.TrimSpace(opts.ResearcherAccountID); account != "" {
		recorded = recorded.forResearcher(account)
	}
	return recorded, pass(name)
}

// forResearcher narrows the ledger to the datasets one researcher account holds a grant
// against, keeping every copy and every grant belonging to those datasets.
//
// The narrowing is by dataset rather than by grant because the two documents are. A pair
// rendered from one account's grants alone is missing every other account's statements, so
// comparing against it would report each of those as drift, and reconciliation would then
// write that pair and withdraw access nobody asked about.
func (l datasetLedger) forResearcher(researcherAccountID string) datasetLedger {
	held := make(map[string]struct{})
	for _, grant := range l.grants {
		if strings.TrimSpace(grant.ResearcherAccountID) == researcherAccountID {
			held[grant.HotCacheBucket] = struct{}{}
		}
	}

	var narrowed datasetLedger
	for _, c := range l.copies {
		if _, covered := held[c.HotCacheBucket]; covered {
			narrowed.copies = append(narrowed.copies, c)
		}
	}
	for _, grant := range l.grants {
		if _, covered := held[grant.HotCacheBucket]; covered {
			narrowed.grants = append(narrowed.grants, grant)
		}
	}
	return narrowed
}

// copyGroup is the copies sharing one deployed resource -- the bucket their documents sit
// on, or the key encrypting them -- together with every grant made against any of them.
//
// It is the unit both per-dataset checks work in, because it is the unit the deployment
// works in: one bucket and one key per dataset, holding one pair of documents between them
// however many times the dataset was copied and however many researchers hold a grant
// against it.
type copyGroup struct {
	copies []*dataset.Copy
	grants []*dataset.Grant
}

// groupedBy gathers the ledger's copies under whatever the given function returns for
// each, in the order those values were first seen so a report reads the same way twice,
// and hands each group the grants made against its copies.
func (l datasetLedger) groupedBy(under func(*dataset.Copy) string) []copyGroup {
	byCopy := make(map[string][]*dataset.Grant, len(l.grants))
	for _, grant := range l.grants {
		byCopy[grant.CopyID] = append(byCopy[grant.CopyID], grant)
	}

	var groups []copyGroup
	index := make(map[string]int, len(l.copies))
	for _, c := range l.copies {
		shared := strings.TrimSpace(under(c))
		at, seen := index[shared]
		if !seen {
			at = len(groups)
			index[shared] = at
			groups = append(groups, copyGroup{})
		}
		groups[at].copies = append(groups[at].copies, c)
		groups[at].grants = append(groups[at].grants, byCopy[c.CopyID]...)
	}
	return groups
}

// subject is the copy a report names the group by. Every copy in a group shares the
// dataset, the bucket and the key, so any of them will do; copies arrive newest first.
func (g copyGroup) subject() *dataset.Copy {
	return g.copies[0]
}

// policyTarget is what the group's documents are expected to say: the pair they are
// deployed on, and every grant recorded against the copies they cover.
func (g copyGroup) policyTarget() dataset.PolicyTarget {
	subject := g.subject()
	return dataset.PolicyTarget{
		DatasetURI:     subject.DatasetURI,
		HotCacheBucket: subject.HotCacheBucket,
		KeyArn:         subject.KeyArn,
		Grants:         g.grants,
	}
}

// activeGrants returns the grants currently giving a researcher access through the group.
func (g copyGroup) activeGrants(now time.Time) []*dataset.Grant {
	var active []*dataset.Grant
	for _, grant := range g.grants {
		if grant.IsActive(now) {
			active = append(active, grant)
		}
	}
	return active
}

// destroyed reports whether the pending window on the group's key has already elapsed for
// every copy under it, which is the point past which those copies are unreadable and
// nothing about them can be put back.
func (g copyGroup) destroyed(now time.Time) bool {
	for _, c := range g.copies {
		if !c.IsDestroyed(now) {
			return false
		}
	}
	return len(g.copies) > 0
}

// probeExecutionName is the execution named by the reachability probe. Nothing is ever
// started under it: it exists so that the orchestration can be asked a question whose
// answer is known in advance, which is what distinguishes an orchestration that answers
// from one that cannot be reached.
const probeExecutionName = "data-releaser-reachability-probe"

// checkCopyOrchestration reports whether the copy orchestration answers for the
// configured state machine.
//
// The orchestration is installed separately, and all this tool ever asks of it is to start
// a copy and later to describe one. So a copy that has already run is the strongest
// evidence available: describing it confirms the state machine named in configuration is
// the one those copies were started on, and that this identity may ask about it.
//
// Where no copy has run yet — a fresh deployment, which is exactly when an operator wants
// to know — an execution that cannot exist is described instead. An answer of "no such
// execution" establishes that the account and region are reachable and that the call is
// permitted. It does not establish that the state machine itself is deployed, because the
// same answer comes back either way, and that only becomes visible when a copy is started
// against it.
func checkCopyOrchestration(
	ctx context.Context,
	executions internalaws.StepFunctionsClient,
	targets DatasetCopyTargets,
	copies []*dataset.Copy,
	accountID string,
) Check {
	stateMachine := strings.TrimSpace(targets.StateMachineARN)
	name := checkOf(copyOrchestrationCheck, stateMachine)
	prerequisite := dataset.CopyStateMachinePrerequisite(stateMachine, copyAccount(targets, accountID))

	if executions == nil {
		return fail(name, "no client was configured to reach the copy orchestration with")
	}
	if stateMachine == "" {
		return fail(name, "%v", prerequisite.Missingf(
			"no copy state machine is configured, so no dataset can be copied into this account at all"))
	}

	if started := recordedExecution(stateMachine, copies); started != "" {
		if _, err := executions.DescribeExecution(ctx, &sfn.DescribeExecutionInput{
			ExecutionArn: aws.String(started),
		}); err != nil {
			return fail(name, "%v", prerequisite.Missing(fmt.Errorf(
				"describing copy %s, which this deployment started on that state machine: %w", started, err)))
		}
		return pass(name)
	}

	probe, err := probeExecutionARN(stateMachine)
	if err != nil {
		return fail(name, "%v", prerequisite.Missing(err))
	}

	_, err = executions.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: aws.String(probe)})
	var absent *sfntypes.ExecutionDoesNotExist
	switch {
	case err == nil, errors.As(err, &absent):
		return note(name, "the orchestration answers for this state machine's account and region "+
			"and this identity may query it. No copy has been started from this deployment yet, so "+
			"whether the state machine itself is deployed is not established here — it answers the "+
			"same way either way, and the first copy started against it is what will say")
	default:
		return fail(name, "%v", prerequisite.Missing(fmt.Errorf(
			"asking the orchestration about an execution of that state machine: %w", err)))
	}
}

// recordedExecution returns the newest copy this deployment started on the given state
// machine, or the empty string when it has started none.
//
// The state machine an execution belongs to is part of its identifier, so an execution
// recorded before the configured state machine was changed is passed over: describing it
// would confirm a state machine nothing is submitted to any more.
func recordedExecution(stateMachineARN string, copies []*dataset.Copy) string {
	machine := stateMachineName(stateMachineARN)
	if machine == "" {
		return ""
	}
	// Copies arrive newest first, so the most recent transfer is the one described.
	for _, c := range copies {
		execution := strings.TrimSpace(c.CopyExecutionArn)
		if execution == "" {
			continue
		}
		parsed, err := awsarn.Parse(execution)
		if err != nil {
			continue
		}
		kind, rest, found := strings.Cut(parsed.Resource, ":")
		if !found || kind != "execution" {
			continue
		}
		if named, _, found := strings.Cut(rest, ":"); found && named == machine {
			return execution
		}
	}
	return ""
}

// probeExecutionARN builds the identifier of an execution that does not exist on the given
// state machine.
func probeExecutionARN(stateMachineARN string) (string, error) {
	parsed, err := awsarn.Parse(stateMachineARN)
	if err != nil {
		return "", fmt.Errorf("the configured state machine %q is not an ARN, so the orchestration "+
			"cannot be asked about it: %w", stateMachineARN, err)
	}
	machine := stateMachineName(stateMachineARN)
	if machine == "" {
		return "", fmt.Errorf("the configured state machine ARN %q names no state machine; it has to "+
			"be the ARN of a state machine rather than of anything else the copy orchestration deployed",
			stateMachineARN)
	}
	return fmt.Sprintf("arn:%s:states:%s:%s:execution:%s:%s",
		parsed.Partition, parsed.Region, parsed.AccountID, machine, probeExecutionName), nil
}

// stateMachineName reads the state machine's name out of its ARN, and returns the empty
// string for an ARN that names something else.
func stateMachineName(stateMachineARN string) string {
	parsed, err := awsarn.Parse(strings.TrimSpace(stateMachineARN))
	if err != nil {
		return ""
	}
	kind, name, found := strings.Cut(parsed.Resource, ":")
	if !found || kind != "stateMachine" {
		return ""
	}
	return name
}

// probeObjectBody is what the writability probe puts in the object, so that a copy of it
// left behind explains itself to whoever finds it.
const probeObjectBody = "Written by the data releaser to confirm that the copy orchestration's " +
	"working bucket can be written to. It carries no data and is deleted immediately; a copy of it " +
	"left behind is safe to remove.\n"

// checkCopyWorkingBucket reports whether the bucket the copy orchestration works in can be
// written to.
//
// It writes. That is the point: whether the bucket exists and whether it can be written to
// are different questions, and only the second one matters here, because what this tool
// does with the bucket is put a copy's instructions in it. Asking S3 about the bucket
// answers neither reliably — a policy granting writes on one prefix and nothing else
// refuses the question while permitting the write, so a deployment that works would be
// reported as broken, and a bucket that answers the question can still refuse the write.
//
// So the probe is the same operation a copy's instructions are, at the same prefix, and it
// is removed immediately. It records no copy, grants nobody anything, and touches no
// dataset bucket. Where the removal fails the object is named, since a bucket that accepts
// writes and refuses deletes leaves one behind.
func checkCopyWorkingBucket(
	ctx context.Context,
	objects internalaws.S3Client,
	targets DatasetCopyTargets,
	accountID string,
	now time.Time,
) Check {
	bucket := strings.TrimSpace(targets.WorkingBucket)
	name := checkOf(workingBucketCheck, bucket)
	prerequisite := dataset.CopyWorkingBucketPrerequisite(bucket, copyAccount(targets, accountID))

	if objects == nil {
		return fail(name, "no client was configured to reach the working bucket with")
	}
	if bucket == "" {
		return fail(name, "%v", prerequisite.Missingf(
			"no working bucket is configured, so a copy has nowhere to read its instructions from"))
	}

	key := probeObjectKey(targets.WorkingBucketPrefix, now)
	if _, err := objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(probeObjectBody),
		ContentType: aws.String("text/plain"),
	}); err != nil {
		return fail(name, "%v", prerequisite.Missing(fmt.Errorf(
			"writing %s, beneath the prefix a copy's own instructions are written to, so either the "+
				"bucket is absent or that prefix cannot be written to: %w", s3URI(bucket, key), err)))
	}

	if _, err := objects.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		return note(name, "the bucket accepts writes. The probe object %s could not be removed "+
			"afterwards and holds nothing, so it can be deleted by hand: %v", s3URI(bucket, key), err)
	}

	return pass(name)
}

// probeObjectKey is where the writability probe writes, beneath the configured working
// prefix so that a grant scoped to that prefix covers it, and named so that a copy of it
// left behind is recognisable and cannot collide with a copy's own artefacts.
func probeObjectKey(workingPrefix string, now time.Time) string {
	prefix := strings.TrimSpace(workingPrefix)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return fmt.Sprintf("%svalidate/writability-probe-%s.txt", prefix, now.UTC().Format("20060102T150405Z"))
}

// s3URI renders a bucket and key the way an operator would paste it back.
func s3URI(bucket, key string) string {
	return "s3://" + bucket + "/" + key
}

// checkSourceCredentials reports what can be established about the secret the copy
// orchestration reads the source store's credentials from.
//
// What can be established is the identifier, and no more. The credentials are not ours to
// hold or to use: only the secret's identifier travels with a copy, and the orchestration
// resolves it with its own permissions, which is what keeps a source credential out of this
// tool entirely. So whether the secret exists, and whether the orchestration may read it,
// can only be answered by the orchestration — and a wrong answer surfaces as a copy failing
// at its first read of the source store.
func checkSourceCredentials(source DatasetSourceStore, copyAccountID string) Check {
	secret := strings.TrimSpace(source.CredentialsSecret)
	name := checkOf(sourceCredentialsCheck, secret)
	prerequisite := dataset.SourceCredentialsPrerequisite(secret, copyAccountID)

	if secret == "" {
		return fail(name, "%v", prerequisite.Missingf(
			"no credentials secret is configured, so a copy has no way to authenticate to the source store"))
	}

	// An identifier written as an ARN is checked for naming a secret, because that is the
	// one mistake visible from here: an ARN of anything else is accepted by configuration,
	// carried into a copy, and refused by the orchestration hours later.
	if strings.HasPrefix(secret, "arn:") {
		parsed, err := awsarn.Parse(secret)
		if err != nil {
			return fail(name, "%v", prerequisite.Missingf(
				"the configured identifier begins as an ARN but is not one: %v", err))
		}
		if parsed.Service != "secretsmanager" {
			return fail(name, "%v", prerequisite.Missingf(
				"the configured identifier names a %s resource rather than a secret, so the copy "+
					"orchestration has nothing to resolve the source store's credentials from",
				parsed.Service))
		}
	}

	return skip(name, "the secret is named, and that is as far as this can be checked. "+
		"The source store's credentials never pass through this tool: only the secret's identifier "+
		"travels with a copy, and the copy orchestration resolves it with its own permissions. So "+
		"whether the secret exists and whether that orchestration may read it can only be "+
		"established by it, and a missing or unreadable secret surfaces as a copy failing at its "+
		"first read of the source store")
}

// checkSourceEndpoint reports what can be established about the route to the source store.
//
// Its shape, and nothing else. The endpoint is reached from the subnets the copy
// orchestration runs in, not from wherever this command was run, so resolving it here would
// prove nothing: it can resolve for an operator and be unreachable from those subnets, and
// it can fail to resolve on a deployment that is working perfectly.
func checkSourceEndpoint(source DatasetSourceStore) Check {
	endpoint := strings.TrimSpace(source.EndpointURL)
	name := checkOf(sourceEndpointCheck, endpoint)
	prerequisite := dataset.SourceEndpointPrerequisite(endpoint)

	if endpoint == "" {
		return fail(name, "%v", prerequisite.Missingf(
			"no endpoint is configured, so a copy has no address to read the source store at"))
	}
	parsed, err := url.Parse(endpoint)
	switch {
	case err != nil:
		return fail(name, "%v", prerequisite.Missingf("the configured endpoint is not a URL: %v", err))
	case parsed.Scheme != "https" && parsed.Scheme != "http":
		return fail(name, "%v", prerequisite.Missingf(
			"the configured endpoint has scheme %q rather than http or https", parsed.Scheme))
	case parsed.Host == "":
		return fail(name, "%v", prerequisite.Missingf("the configured endpoint names no host"))
	}

	return skip(name, "the endpoint is a well-formed URL naming host %s, and that is as far as this "+
		"can be checked. It is reached from the subnets the copy orchestration runs in rather than "+
		"from here, so a lookup made here would say nothing either way: it can succeed where a copy "+
		"cannot reach the store, and fail on a deployment that works", parsed.Host)
}

// checkDeployedAccess compares the access the ledger describes for one dataset against the
// access its deployed documents grant, and returns what was compared so a repair can be
// decided from it.
//
// Every grant recorded against the dataset's copies goes into the comparison, whatever
// state each is in, and the withdrawn ones are the interesting ones: a grant still deployed
// after its revocation is access nobody believes exists.
func checkDeployedAccess(
	ctx context.Context,
	differ *dataset.PolicyDiffer,
	group copyGroup,
	now time.Time,
) (Check, *dataset.PolicyDiff) {
	subject := group.subject()
	name := fmt.Sprintf("%s: %s", deployedAccessCheck, subject.DatasetURI)

	// A destroyed key leaves nothing to compare. The documents may still be deployed, but
	// the objects under them are unreadable for good, so reporting the pair as a failure
	// every run from here on would only teach an operator to stop reading the output.
	if group.destroyed(now) {
		return skip(name, "the key encrypting this dataset was destroyed on %s, so what was copied "+
			"is unreadable for good and there is no access left to compare",
			subject.KeyDeletionDate.UTC().Format(time.RFC3339)), nil
	}

	diff, err := differ.Diff(ctx, group.policyTarget())
	if err != nil {
		return fail(name, "%v", err), nil
	}
	if diff.Matches() {
		return pass(name), diff
	}
	return fail(name, "%s", diff.Report()), diff
}

// reapplyLedgerPolicies replaces both of a dataset's deployed documents with the pair the
// ledger describes.
//
// This is the only thing here that writes, and it writes whole documents: the pair is
// rendered from every grant recorded against the dataset's copies and replaces what is
// deployed, so a statement added by hand is gone afterwards and one removed by hand is
// back. What it replaced is reported, because a repair an operator cannot see the shape of
// is not a repair they can check.
func reapplyLedgerPolicies(
	ctx context.Context,
	applier *dataset.PolicyApplier,
	group copyGroup,
	diff *dataset.PolicyDiff,
) Check {
	name := fmt.Sprintf("Ledger policies re-applied: %s", group.subject().DatasetURI)

	if diff == nil {
		// The deployed access could not be read, so what a re-application would change
		// cannot be stated. Writing anyway would replace documents whose contents are
		// unknown and report nothing about what was lost.
		return skip(name, "the deployed policies could not be read, so re-applying them could not "+
			"be reported as a change; the reported failure has to be resolved first")
	}

	application, err := applier.Apply(ctx, group.policyTarget())
	if err != nil {
		return fail(name, "%v", err)
	}
	return note(name, "%s", describeReapplication(group, application))
}

// describeReapplication says what a re-application replaced and what the deployed access
// now is.
func describeReapplication(group copyGroup, application *dataset.PolicyApplication) string {
	var b strings.Builder

	replaced := make([]string, 0, 2)
	for _, target := range application.Written {
		switch target {
		case dataset.ApplyTargetBucket:
			replaced = append(replaced, fmt.Sprintf("the %s of %s", target, application.HotCacheBucket))
		default:
			replaced = append(replaced, fmt.Sprintf("the %s of %s", target, application.KeyArn))
		}
	}
	fmt.Fprintf(&b, "replaced %s", joinList(replaced))

	if application.Granting {
		fmt.Fprintf(&b, ". The deployed access is now what the ledger describes: %s",
			joinList(grantPhrases(group, application.GrantIDs)))
	} else {
		fmt.Fprintf(&b, ". Neither document grants any researcher access now, which is what the "+
			"ledger describes: no grant against %s is still deployed", copiesPhrase(group.copies))
	}
	if application.KeyPolicyLeftAlone != "" {
		fmt.Fprintf(&b, ". The key policy was left alone: %s", application.KeyPolicyLeftAlone)
	}
	return b.String()
}

// grantPhrases renders the access each written grant gives, in the order the documents
// state them, so a report says who a repair left reading rather than only how many.
func grantPhrases(group copyGroup, grantIDs []string) []string {
	byID := make(map[string]*dataset.Grant, len(group.grants))
	for _, grant := range group.grants {
		byID[grant.GrantID] = grant
	}

	phrases := make([]string, 0, len(grantIDs))
	for _, id := range grantIDs {
		grant, recorded := byID[id]
		if !recorded {
			continue
		}
		phrases = append(phrases, fmt.Sprintf("account %s reading from %s, %s",
			grant.ResearcherAccountID, grant.TargetVpcID, expiryPhrase(grant)))
	}
	return phrases
}

// expiryPhrase renders when a grant's access ceases, the way a report reads it.
func expiryPhrase(grant *dataset.Grant) string {
	if grant.IsUnlimited() {
		return "with no expiry"
	}
	return "until " + grant.ExpiresAt.UTC().Format(time.RFC3339)
}

// checkDatasetKey reports the state of the key encrypting one dataset, and fails when that
// state contradicts a grant still giving access through it.
//
// Bucket keys are off on these buckets, so every read is decrypted per request and the key
// is in the path of all of them. A key that cannot decrypt therefore stops reads as surely
// as a withdrawn grant does — but with none of the visibility: the researcher sees a denial
// they cannot distinguish from a wrong VPC identifier or a passed expiry, and so does
// whoever they report it to. Which is the whole reason to say so here.
func checkDatasetKey(
	ctx context.Context,
	keys internalaws.KMSClient,
	group copyGroup,
	accountID string,
	now time.Time,
) Check {
	subject := group.subject()
	name := fmt.Sprintf("Dataset key state: %s", subject.DatasetURI)
	prerequisite := dataset.DatasetKeyPrerequisite(subject.KeyArn, accountID)

	// Every copy under this key has had it destroyed, so there is no key left to describe
	// and its absence is the intended outcome rather than a finding.
	if group.destroyed(now) {
		return skip(name, "the key encrypting this dataset was destroyed on %s, as the purge of the "+
			"dataset intended, so there is no key state left to report",
			subject.KeyDeletionDate.UTC().Format(time.RFC3339))
	}

	if keys == nil {
		return fail(name, "no client was configured to read the dataset key's state with")
	}

	state, deletionDate, err := describeKeyState(ctx, keys, subject.KeyArn)
	if err != nil {
		return fail(name, "%v", prerequisite.Missing(fmt.Errorf(
			"reading the state of the key encrypting dataset %q: %w", subject.DatasetURI, err)))
	}
	if state == kmstypes.KeyStateEnabled {
		return pass(name)
	}

	pending := state == kmstypes.KeyStatePendingDeletion || state == kmstypes.KeyStatePendingReplicaDeletion
	if granting := group.activeGrants(now); len(granting) > 0 {
		if pending {
			return fail(name, "the key %s encrypting %s is %s, so what it holds is being destroyed "+
				"while access through it is still live: %s%s. Reads fail at decryption and the "+
				"researcher cannot tell that from a wrong VPC or a passed expiry; revoke the grants, "+
				"or cancel the deletion before it becomes irreversible%s",
				subject.KeyArn, subject.HotCacheBucket, state, grantsPhrase(granting),
				grantedToPhrase(granting), deletionPhrase(deletionDate))
		}
		return fail(name, "the key %s encrypting %s is %s rather than enabled, so every read fails "+
			"at decryption while access through it is still live: %s%s. The researcher sees a denial "+
			"indistinguishable from a wrong VPC or a passed expiry; enable the key, or revoke the "+
			"grants if the withdrawal was intended",
			subject.KeyArn, subject.HotCacheBucket, state, grantsPhrase(granting),
			grantedToPhrase(granting))
	}

	if pending {
		return note(name, "the key is %s. Nothing is granted through it, so no access is affected%s",
			state, deletionPhrase(deletionDate))
	}
	return note(name, "the key is %s rather than enabled. Nothing is granted through it, so no "+
		"access is affected, but no copy of this dataset can be granted until the key can "+
		"decrypt again", state)
}

// describeKeyState reads a key's state, and the date its deletion becomes irreversible
// where one is scheduled.
func describeKeyState(ctx context.Context, keys internalaws.KMSClient, keyArn string) (kmstypes.KeyState, *time.Time, error) {
	described, err := keys.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(keyArn)})
	if err != nil {
		return "", nil, err
	}
	if described.KeyMetadata == nil || strings.TrimSpace(string(described.KeyMetadata.KeyState)) == "" {
		return "", nil, errors.New("the key was found but reported no state, so whether it can " +
			"decrypt this dataset cannot be established")
	}
	return described.KeyMetadata.KeyState, described.KeyMetadata.DeletionDate, nil
}

// grantsPhrase names the grants a finding is about.
func grantsPhrase(grants []*dataset.Grant) string {
	ids := make([]string, 0, len(grants))
	for _, grant := range grants {
		ids = append(ids, grant.GrantID)
	}
	if len(ids) == 1 {
		return "grant " + ids[0]
	}
	return "grants " + joinList(ids)
}

// copiesPhrase names the copies a finding is about.
func copiesPhrase(copies []*dataset.Copy) string {
	ids := make([]string, 0, len(copies))
	for _, c := range copies {
		ids = append(ids, c.CopyID)
	}
	if len(ids) == 1 {
		return "copy " + ids[0]
	}
	return "copies " + joinList(ids)
}

// grantedToPhrase names the accounts a finding's grants give access to, so the report says
// who is affected rather than only which records are.
func grantedToPhrase(grants []*dataset.Grant) string {
	var accounts []string
	for _, grant := range grants {
		if account := strings.TrimSpace(grant.ResearcherAccountID); account != "" &&
			!slices.Contains(accounts, account) {
			accounts = append(accounts, account)
		}
	}
	if len(accounts) == 0 {
		return ""
	}
	return " to account " + joinList(accounts)
}

// deletionPhrase says when a scheduled deletion becomes irreversible, where the key
// reported a date for it.
func deletionPhrase(deletionDate *time.Time) string {
	if deletionDate == nil {
		return ""
	}
	return ", and what was copied becomes unreadable for good on " + deletionDate.UTC().Format(time.RFC3339)
}

// copyAccount is the account the copy orchestration is installed in, which defaults to the
// account holding the dataset buckets.
func copyAccount(targets DatasetCopyTargets, accountID string) string {
	if copyAccountID := strings.TrimSpace(targets.AccountID); copyAccountID != "" {
		return copyAccountID
	}
	return accountID
}

// checkOf names a check together with the resource it is about, or on its own where that
// resource is not configured.
func checkOf(label, subject string) string {
	if strings.TrimSpace(subject) == "" {
		return label
	}
	return fmt.Sprintf("%s (%s)", label, subject)
}

// joinList renders a list of things the way a sentence reads them.
func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}
