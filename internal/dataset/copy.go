// This file builds the submission for one copy of one dataset out of the external
// source store and into that dataset's cache bucket: the instruction document, where
// that document has to be uploaded, and the input the copy state machine is started
// with. It performs no AWS calls and moves no bytes.
//
// Two facts about the deployment are unresolved and are therefore configuration rather
// than assumptions baked into the code: whether the source store's endpoint is routable
// from the subnets the copy service runs in, and whether the copy service is installed
// in the same account as the cache buckets. Both surface here only as values to be
// supplied and, when absent or unusable, as a named prerequisite with the party who owns
// it — never as a bare AWS error.

package dataset

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// File names the copy service uses for its own artefacts. Each is positioned relative to
// a different prefix, which is why the control prefix has to be spliced onto the
// destination ones individually rather than set once for the whole run.
const (
	instructionsFileName = "INSTRUCTIONS.jsonl"
	startMarkerFileName  = "STARTED_COPY.txt"
	summaryCsvFileName   = "ENDED_COPY.csv"
	htmlReportFileName   = "ENDED_COPY_REPORT.html"
)

// credentialProviderSecret asks the copy service to read a bucket's credentials from a
// Secrets Manager secret it resolves itself.
const credentialProviderSecret = "aws-secret" // pragma: allowlist secret

// runIDPattern is what a run identifier may contain, since it becomes a prefix in the
// working bucket. A ULID satisfies it.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// SourceStore describes the external, S3-compatible object store holding the
// authoritative copy of every dataset.
//
// The credentials for it are named here and never held: the copy service resolves the
// secret with its own permissions, so no source credential passes through this process.
type SourceStore struct {
	// EndpointURL is the store's custom S3 endpoint. Whether it resolves and is
	// routable from the copy service's subnets is a property of the deployment, not of
	// this value, so an unreachable endpoint is reported against the store's operator
	// rather than treated as a bug here.
	EndpointURL string

	// Region is the region the store presents itself as being in. S3-compatible stores
	// generally require some value even when the concept does not apply.
	Region string

	// CredentialsSecret is the Secrets Manager secret holding the store's access key.
	// The identifier travels to the copy service; the credentials do not travel at all.
	CredentialsSecret string
}

// CopyConfig is the deployment-dependent half of a copy: which copy service to submit
// to, where it keeps its working artefacts, and how to reach the source store.
type CopyConfig struct {
	// AccountID owns the cache buckets and the keys encrypting them.
	AccountID string

	// CopyAccountID is the account the copy service is installed in. It is separate
	// from AccountID because a copy service is normally installed to copy objects *out*
	// of its own account, and this mechanism copies *in*, so whether it can be
	// installed alongside the cache buckets at all is a deployment decision. Empty
	// means the same account as AccountID.
	CopyAccountID string

	// Region is the region the cache buckets are in. When set, the copy is required to
	// land there, so a bucket resolved to an unexpected region fails the run rather
	// than silently receiving the data.
	Region string

	// StateMachineARN identifies the copy state machine. Configuration, so that a copy
	// service in another account is a config change rather than a code change.
	StateMachineARN string

	// WorkingBucket is the bucket the copy service reads its instructions from and
	// writes its own outputs to. It belongs to the copy service's installation.
	WorkingBucket string

	// WorkingBucketPrefix is the prefix within WorkingBucket that the copy service was
	// deployed with. Every key this code computes for that bucket sits beneath it.
	WorkingBucketPrefix string

	// Source describes the store the dataset is read from.
	Source SourceStore

	// Performance tunes how hard this copy runs. Zero fields leave the copy service's
	// own defaults in place.
	Performance CopyPerformance
}

// CopyPerformance tunes how hard one copy is allowed to run.
//
// The values belong to the source being copied from rather than to this installation,
// which is why they travel with the copy rather than being fixed when the copy service is
// deployed. A store that scales with the load put on it, as S3 does, wants the defaults; a
// store that does not -- an on-premise or S3-compatible endpoint on a fixed link -- is
// saturated by them, and the same installation may well copy from both.
//
// Saturating the source does not fail the transfers that caused it. It fails the small
// object copies running beside them: their requests queue behind the large ones until the
// Lambda carrying a batch exceeds its timeout, and because that stage tolerates no
// failures at all, one late batch fails the entire run -- including the large transfers
// that were succeeding. So the setting that needs lowering is rarely the one whose
// symptom is visible.
//
// A zero field is omitted from the request, which leaves the copy service's default for
// it. That is deliberately not the same as sending zero, which would ask for no
// concurrency at all.
type CopyPerformance struct {
	// HeadConcurrency bounds how many source objects are examined at once, before any
	// data moves. It is the first load the source sees.
	HeadConcurrency int

	// SmallCopyConcurrency bounds how many small-object copy Lambdas run at once, and
	// SmallCopyBatchSize how many objects each one is given. The batch is copied one
	// object at a time, so the size multiplies directly into how long an invocation
	// runs against a fixed timeout.
	SmallCopyConcurrency int
	SmallCopyBatchSize   int

	// LargeCopyConcurrency bounds how many large objects are transferred at once. Each
	// runs as its own task, so this is the heaviest sustained read the source sees.
	LargeCopyConcurrency int
}

// IsZero reports whether nothing is configured, and so whether the copy service is left
// entirely to its own defaults.
func (p CopyPerformance) IsZero() bool {
	return p == CopyPerformance{}
}

// copyAccount returns the account the copy service runs in, defaulting to the account
// owning the cache buckets.
func (c CopyConfig) copyAccount() string {
	if account := strings.TrimSpace(c.CopyAccountID); account != "" {
		return account
	}
	return strings.TrimSpace(c.AccountID)
}

// SourceObject is one object belonging to a dataset, named relative to the dataset's own
// prefix: "sample1/reads.bam" for an object at "cohort-a/sample1/reads.bam" in a dataset
// rooted at "cohort-a/".
type SourceObject struct {
	// Key is the object's key relative to the dataset prefix.
	Key string

	// Sums optionally asserts a checksum for this object, which the copy service
	// verifies as it transfers. Only available when objects are named explicitly: a
	// checksum describes one object, so it cannot accompany a wildcard.
	Sums string
}

// CopyRequest is the caller's half of a copy: which dataset, into which bucket, and
// under what identity this particular run is recorded.
type CopyRequest struct {
	// RunID distinguishes this copy from every other copy of the same dataset. It
	// becomes the prefix in the working bucket that the instructions and the copy's own
	// outputs are written under, and those outputs are overwritten when a prefix is
	// reused, so each run needs a fresh value. The identifier the copy is recorded
	// under serves.
	RunID string

	// DatasetURI identifies the collection of objects in the source store to copy.
	DatasetURI string

	// DestinationBucket is the cache bucket for this dataset. It holds one dataset, so
	// the objects land at its root and nothing needs a per-dataset prefix.
	DestinationBucket string

	// Objects optionally names the dataset's objects explicitly. Leaving it empty hands
	// enumeration to the copy service; see PlanCopy for why that is the default.
	Objects []SourceObject

	// DryRun asks the copy service to go through the motions — including checking that
	// every source object exists and that the destination is writable — without moving
	// any bytes.
	DryRun bool
}

// DatasetLocation is where a dataset sits in the source store.
type DatasetLocation struct {
	// Bucket holds the dataset.
	Bucket string

	// Prefix is the slash-terminated prefix the dataset's objects sit under. Empty means
	// the whole bucket is the dataset.
	Prefix string
}

// String renders the location back as a URI, in the one form this project writes: the
// s3 scheme, the bucket, and a slash-terminated prefix.
func (l DatasetLocation) String() string {
	return "s3://" + l.Bucket + "/" + l.Prefix
}

// CanonicalDatasetURI reduces a dataset URI to the single form everything else compares
// against.
//
// The URI is the identity of a dataset, and identity here is exact string equality in
// several places at once: it is the key a dataset's copies and grants are found under, the
// input a cache bucket name is derived from, and the key a bucket pinned in configuration is
// looked up by. Nothing checks that those agree, so a URI written one way in one place and
// another way somewhere else silently names two different datasets -- and the second of
// them has never been staged, holds nothing, and reports as much.
//
// The differences are the ones nobody would think twice about: a missing trailing slash,
// an omitted scheme. Reducing them to one form here is what keeps them from being
// differences at all.
func CanonicalDatasetURI(datasetURI string) (string, error) {
	location, err := ParseDatasetURI(datasetURI)
	if err != nil {
		return "", err
	}
	return location.String(), nil
}

// CopyInstruction is one line of the JSONL document the copy service reads: what to
// fetch from the source store, and how much of the key is folder structure to reproduce
// in the destination.
type CopyInstruction struct {
	// SourceBucket holds the object.
	SourceBucket string `json:"sourceBucket"`

	// SourceKey is one object's key, or a prefix suffixed with "*" to hand expansion of
	// a whole folder to the copy service.
	SourceKey string `json:"sourceKey"`

	// SourceRootFolderKey is the leading portion of SourceKey that is the root of the
	// structure being copied, so everything below it is reproduced in the destination.
	// Without it the copy service places objects under their base file name alone,
	// which flattens the dataset's layout and collides two objects of the same name in
	// different folders. A reader also needs an index to keep its documented position
	// relative to the file it indexes.
	//
	// Absent where SourceKey is a wildcard: the copy service refuses the two together,
	// because the wildcard already names the folder this would otherwise state.
	SourceRootFolderKey string `json:"sourceRootFolderKey,omitempty"`

	// Sums asserts a checksum for this object, absent when no assertion is made.
	Sums string `json:"sums,omitempty"`
}

// BucketDefinition tells the copy service how to reach one bucket, keyed by bucket name
// in the invocation. For the external source store that means a custom endpoint in
// S3-compatible mode plus a reference to the secret holding its credentials — the
// reference only, since the copy service reads that secret itself.
type BucketDefinition struct {
	CredentialProvider string `json:"credentialProvider,omitempty"`
	Secret             string `json:"secret,omitempty"`
	Region             string `json:"region,omitempty"`
	EndpointURL        string `json:"endpointUrl,omitempty"`
	S3Compatible       bool   `json:"s3Compatible"`
}

// CopyInvocation is the input document the copy state machine is started with.
//
// The artefact keys are what keep the copy's own bookkeeping out of the dataset: the
// start marker and the CSV summary are written into the destination bucket, so they are
// placed under the reserved control prefix that every rendered grant denies the
// researcher. The HTML report is not written to the destination at all — it carries cost
// estimates for the transfer, which is our information and not the researcher's — but is
// retained in the working bucket, where only we can read it.
type CopyInvocation struct {
	// DestinationBucket receives the copied objects.
	DestinationBucket string `json:"destinationBucket"`

	// DestinationPrefix is where in that bucket they land. Empty — the bucket root —
	// because a cache bucket holds exactly one dataset, and because every other key
	// below is relative to this one: anchoring the objects at the root is what allows
	// the control artefacts to sit in a sibling prefix the researcher can be denied
	// wholesale.
	DestinationPrefix string `json:"destinationPrefix"`

	// DestinationRequiredRegion fails the run if the destination bucket is not in this
	// region, rather than copying the data somewhere unintended.
	DestinationRequiredRegion string `json:"destinationRequiredRegion,omitempty"`

	// SourceRequiredRegion is the region the source store must present itself as being
	// in. It is stated rather than left to default, because the copy service defaults it
	// to the region it was installed in while the source's own region is declared
	// separately in the bucket definition -- so leaving it out couples the two silently,
	// and a source configured with any other region is refused by a check nothing here
	// made. Both come from the same configured value, so they agree by construction.
	SourceRequiredRegion string `json:"sourceRequiredRegion,omitempty"`

	// InstructionsPrefix is where in the working bucket this run's instructions and
	// outputs live, relative to the prefix the copy service was deployed with.
	InstructionsPrefix string `json:"instructionsPrefix"`

	// InstructionsKey names the instruction document within InstructionsPrefix. Stated
	// rather than defaulted, so the document the copy service reads is the one this
	// code decided to write.
	InstructionsKey string `json:"instructionsKey"`

	// StartMarkerKey, SummaryCsvKey, and HTMLReportKey are relative to
	// DestinationPrefix, and all three are placed under the reserved control prefix.
	StartMarkerKey string `json:"startMarkerKey"`
	SummaryCsvKey  string `json:"summaryCsvKey"`
	HTMLReportKey  string `json:"htmlReportKey"`

	// HTMLReport is false: the report includes transfer cost estimates and must not be
	// written into a bucket a researcher reads.
	HTMLReport bool `json:"htmlReport"`

	// RetainHTMLReport keeps that report in the working bucket instead, which is the
	// only place it appears.
	RetainHTMLReport bool `json:"retainHtmlReport"`

	// RetainSummaryCsv keeps a second copy of the summary in the working bucket. The
	// copy in the destination bucket is encrypted with the dataset's key, so purging the
	// dataset takes the record of what was transferred with it unless it also exists
	// somewhere the purge does not reach.
	RetainSummaryCsv bool `json:"retainSummaryCsv"`

	// ContinueOnError is false: a dataset is released as a unit, so a run that could
	// not copy every object must fail rather than leave a partial copy that looks
	// finished.
	ContinueOnError bool `json:"continueOnError"`

	// DryRun goes through the motions without moving bytes.
	DryRun bool `json:"dryRun,omitempty"`

	// Performance tunes how hard the copy runs. Omitted entirely when nothing is
	// configured, and each field omitted when it is zero, because the copy service
	// reads an absent field as "use the default" and a present zero as the number
	// zero.
	Performance *copyPerformanceInput `json:"performance,omitempty"`

	// BucketDefinitions configures access per bucket, and is how the source store's
	// endpoint and credential secret reach the copy service.
	BucketDefinitions map[string]BucketDefinition `json:"bucketDefinitions,omitempty"`
}

// copyPerformanceInput is the performance block as the copy service reads it.
//
// It is a separate type from CopyPerformance because the two answer to different things:
// one is how this tool describes a copy's intensity, the other is the wire format of the
// service being asked to run it, and the service's field names are its to change.
type copyPerformanceInput struct {
	HeadConcurrency      int `json:"headConcurrency,omitempty"`
	SmallCopyConcurrency int `json:"smallCopyConcurrency,omitempty"`
	SmallCopyBatchSize   int `json:"smallCopyBatchSize,omitempty"`
	LargeCopyConcurrency int `json:"largeCopyConcurrency,omitempty"`
}

// performanceInput renders the configured tuning for the request, or nothing at all when
// none is configured, so that an unconfigured installation sends no performance block
// rather than one full of zeroes.
func performanceInput(p CopyPerformance) *copyPerformanceInput {
	if p.IsZero() {
		return nil
	}
	return &copyPerformanceInput{
		HeadConcurrency:      p.HeadConcurrency,
		SmallCopyConcurrency: p.SmallCopyConcurrency,
		SmallCopyBatchSize:   p.SmallCopyBatchSize,
		LargeCopyConcurrency: p.LargeCopyConcurrency,
	}
}

// CopyPlan is everything needed to submit one copy, and nothing that submits it.
type CopyPlan struct {
	// DatasetURI and Source are the dataset this copy is of, as given and as resolved.
	DatasetURI string
	Source     DatasetLocation

	// Instructions is the parsed form of the document, one entry per line.
	Instructions []CopyInstruction

	// InstructionsDocument is the JSONL document to upload, newline-terminated.
	InstructionsDocument []byte

	// InstructionsBucket and InstructionsKey are where that document must be uploaded
	// before the state machine is started. The key is absolute within the bucket,
	// unlike the relative form the invocation carries.
	InstructionsBucket string
	InstructionsKey    string

	// StateMachineARN is the copy state machine to start.
	StateMachineARN string

	// Invocation is the input to start it with.
	Invocation CopyInvocation

	// StartMarkerURI and SummaryCsvURI are where the copy will write its own artefacts
	// in the destination bucket, both under the reserved control prefix.
	StartMarkerURI string
	SummaryCsvURI  string

	// RetainedSummaryCsvURI and RetainedHTMLReportURI are the copies kept in the working
	// bucket. The retained summary outlives a purge of the dataset, so it is the durable
	// account of what was transferred.
	RetainedSummaryCsvURI string
	RetainedHTMLReportURI string
}

// InvocationJSON renders the state machine input.
func (p *CopyPlan) InvocationJSON() ([]byte, error) {
	document, err := json.Marshal(p.Invocation)
	if err != nil {
		return nil, fmt.Errorf("rendering copy invocation for dataset %q: %w", p.DatasetURI, err)
	}
	return document, nil
}

// PlanCopy builds the submission for one copy of one dataset. It reaches nothing and
// changes nothing; uploading the instruction document and starting the execution are the
// caller's to do.
//
// Enumerating the dataset is the copy service's job unless the caller says otherwise.
// The dataset lives in a store this process holds no credentials for, by design — they
// belong to the copy service, which resolves them from the secret named in the bucket
// definitions — so listing the objects here is not merely awkward but impossible without
// taking on credentials that must not pass through. The default is therefore a single
// wildcard instruction over the dataset's prefix, which the copy service expands using
// its own access.
//
// A caller that already knows the dataset's objects may name them instead, which is the
// only way to assert per-object checksums, since a checksum describes an object rather
// than a folder.
//
// Deployment resources that are absent or unusable are reported together, each named
// with the party responsible for it, so one run tells the operator everything they have
// to fix.
func PlanCopy(cfg CopyConfig, req CopyRequest) (*CopyPlan, error) {
	if err := checkCopyPrerequisites(cfg, req); err != nil {
		return nil, err
	}

	source, err := checkCopyRequest(cfg, req)
	if err != nil {
		return nil, err
	}

	instructions, err := buildInstructions(source, req.Objects)
	if err != nil {
		return nil, err
	}
	document, err := marshalInstructions(instructions)
	if err != nil {
		return nil, err
	}

	// Relative to the prefix the copy service was deployed with; the upload key below
	// joins the two.
	instructionsPrefix := req.RunID + "/"
	workingPrefix := terminatePrefix(cfg.WorkingBucketPrefix) + instructionsPrefix

	startMarkerKey := controlKey(startMarkerFileName)
	summaryCsvKey := controlKey(summaryCsvFileName)
	htmlReportKey := controlKey(htmlReportFileName)

	return &CopyPlan{
		DatasetURI:           req.DatasetURI,
		Source:               source,
		Instructions:         instructions,
		InstructionsDocument: document,
		InstructionsBucket:   cfg.WorkingBucket,
		InstructionsKey:      workingPrefix + instructionsFileName,
		StateMachineARN:      cfg.StateMachineARN,
		Invocation: CopyInvocation{
			DestinationBucket:         req.DestinationBucket,
			DestinationPrefix:         "",
			DestinationRequiredRegion: strings.TrimSpace(cfg.Region),
			SourceRequiredRegion:      strings.TrimSpace(cfg.Source.Region),
			InstructionsPrefix:        instructionsPrefix,
			InstructionsKey:           instructionsFileName,
			StartMarkerKey:            startMarkerKey,
			SummaryCsvKey:             summaryCsvKey,
			HTMLReportKey:             htmlReportKey,
			HTMLReport:                false,
			RetainHTMLReport:          true,
			RetainSummaryCsv:          true,
			ContinueOnError:           false,
			DryRun:                    req.DryRun,
			Performance:               performanceInput(cfg.Performance),
			BucketDefinitions: map[string]BucketDefinition{
				source.Bucket: {
					CredentialProvider: credentialProviderSecret,
					Secret:             strings.TrimSpace(cfg.Source.CredentialsSecret),
					Region:             strings.TrimSpace(cfg.Source.Region),
					EndpointURL:        strings.TrimSpace(cfg.Source.EndpointURL),
					S3Compatible:       true,
				},
			},
		},
		StartMarkerURI: s3URI(req.DestinationBucket, startMarkerKey),
		SummaryCsvURI:  s3URI(req.DestinationBucket, summaryCsvKey),
		// The retained copies are keyed the same way as their destination counterparts,
		// so they too land under a control prefix — here inside this run's working
		// folder, which is ours alone and has no researcher to deny.
		RetainedSummaryCsvURI: s3URI(cfg.WorkingBucket, workingPrefix+summaryCsvKey),
		RetainedHTMLReportURI: s3URI(cfg.WorkingBucket, workingPrefix+htmlReportKey),
	}, nil
}

// checkCopyPrerequisites reports every deployment resource the copy depends on that is
// not configured, naming each one and its owner rather than letting the first AWS call
// fail with something less useful.
func checkCopyPrerequisites(cfg CopyConfig, req CopyRequest) error {
	report := &PrerequisiteReport{}
	copyAccount := cfg.copyAccount()

	if strings.TrimSpace(cfg.StateMachineARN) == "" {
		report.Add(CopyStateMachinePrerequisite("", copyAccount).
			Missingf("no copy state machine is configured"))
	}
	if strings.TrimSpace(cfg.WorkingBucket) == "" {
		report.Add(CopyWorkingBucketPrerequisite("", copyAccount).
			Missingf("no working bucket is configured"))
	}
	if err := checkEndpoint(cfg.Source.EndpointURL); err != nil {
		report.Add(SourceEndpointPrerequisite(strings.TrimSpace(cfg.Source.EndpointURL)).Missing(err))
	}
	if strings.TrimSpace(cfg.Source.CredentialsSecret) == "" {
		report.Add(SourceCredentialsPrerequisite("", copyAccount).
			Missingf("no secret is configured to hold the source store's credentials"))
	}
	if strings.TrimSpace(req.DestinationBucket) == "" {
		report.Add(CacheBucketPrerequisite("", strings.TrimSpace(cfg.AccountID)).
			Missingf("no cache bucket is known for dataset %q", req.DatasetURI))
	}

	return report.Err()
}

// checkEndpoint reports why the configured source endpoint cannot be used, if it cannot.
// Only the value is examined: whether it resolves, and whether it is routable from the
// subnets the copy service runs in, is a property of the deployment that this process is
// in no position to test.
func checkEndpoint(endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return errors.New("no endpoint is configured for the source store")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a URL: %w", endpoint, err)
	}
	// Plaintext is permitted because the source store's transport is its operator's
	// choice, and the copy service is the only thing that connects to it.
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("endpoint %q must be an http or https URL", endpoint)
	}
	if parsed.Host == "" {
		return fmt.Errorf("endpoint %q names no host", endpoint)
	}
	return nil
}

// checkCopyRequest validates the caller's side of the copy and resolves the dataset's
// location, collecting every problem so a malformed request is reported once.
func checkCopyRequest(cfg CopyConfig, req CopyRequest) (DatasetLocation, error) {
	var errs []error

	runID := strings.TrimSpace(req.RunID)
	switch {
	case runID == "":
		errs = append(errs, errors.New(
			"a run identifier is required: it becomes this copy's own folder in the working "+
				"bucket, and reusing one overwrites the previous copy's outputs"))
	case !runIDPattern.MatchString(runID):
		errs = append(errs, fmt.Errorf(
			"run identifier %q must be alphanumeric, optionally with dots, dashes, or "+
				"underscores, since it becomes a key prefix", req.RunID))
	}

	source, err := ParseDatasetURI(req.DatasetURI)
	if err != nil {
		errs = append(errs, err)
	} else if source.Bucket == strings.TrimSpace(req.DestinationBucket) {
		// Access is configured per bucket name, so a source and destination sharing a
		// name would route the destination through the source store's endpoint and
		// credentials.
		errs = append(errs, fmt.Errorf(
			"dataset %q is read from bucket %q, which is also the destination bucket; the "+
				"source store and the cache bucket must not share a bucket name",
			req.DatasetURI, source.Bucket))
	}

	if len(errs) > 0 {
		return DatasetLocation{}, errors.Join(errs...)
	}
	return source, nil
}

// ParseDatasetURI resolves a dataset URI into the bucket and prefix its objects sit
// under. The "s3://" scheme is optional and the only one accepted; a prefix is
// normalised to end in a slash so it can be concatenated with an object's relative key.
func ParseDatasetURI(datasetURI string) (DatasetLocation, error) {
	trimmed := strings.TrimSpace(datasetURI)
	if trimmed == "" {
		return DatasetLocation{}, errors.New("a dataset URI is required")
	}
	if strings.ContainsAny(trimmed, "*?") {
		return DatasetLocation{}, fmt.Errorf(
			"dataset URI %q must not contain wildcards; it names a folder, and matching "+
				"within it is the copy's business", datasetURI)
	}

	remainder := trimmed
	if scheme, rest, found := strings.Cut(trimmed, "://"); found {
		if !strings.EqualFold(scheme, "s3") {
			return DatasetLocation{}, fmt.Errorf(
				"dataset URI %q has scheme %q; only s3 is understood", datasetURI, scheme)
		}
		remainder = rest
	}

	bucket, prefix, _ := strings.Cut(remainder, "/")
	if bucket == "" {
		return DatasetLocation{}, fmt.Errorf("dataset URI %q names no bucket", datasetURI)
	}
	prefix = strings.TrimPrefix(prefix, "/")
	if strings.Contains(prefix, "//") {
		return DatasetLocation{}, fmt.Errorf(
			"dataset URI %q contains an empty path segment", datasetURI)
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "." || segment == ".." {
			return DatasetLocation{}, fmt.Errorf(
				"dataset URI %q contains a relative path segment", datasetURI)
		}
	}

	return DatasetLocation{Bucket: bucket, Prefix: terminatePrefix(prefix)}, nil
}

// buildInstructions turns the dataset's location, and the caller's object list if there
// is one, into the lines of the instruction document.
//
// With no object list the result is a single wildcard over the dataset's prefix, leaving
// expansion to the copy service, which has the credentials for the source store. The
// consequence is that the dataset's contents are not inspected here at all: a folder in
// the source literally named after the control prefix would be copied into the region a
// researcher is denied, and only an explicit object list can be checked for that.
//
// The wildcard form carries no root folder key. The copy service refuses the two
// together, and rightly: the wildcard already names the folder being copied, so a root
// key alongside it either repeats that or contradicts it. Naming the root matters only
// where the keys are listed one by one, since there the folder being copied cannot be
// read off any single key.
func buildInstructions(source DatasetLocation, objects []SourceObject) ([]CopyInstruction, error) {
	if len(objects) == 0 {
		return []CopyInstruction{{
			SourceBucket: source.Bucket,
			SourceKey:    source.Prefix + "*",
		}}, nil
	}

	instructions := make([]CopyInstruction, 0, len(objects))
	var errs []error
	seen := make(map[string]struct{}, len(objects))

	for i, object := range objects {
		key, err := checkObjectKey(object.Key, i)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			errs = append(errs, fmt.Errorf("object %q is named more than once", key))
			continue
		}
		seen[key] = struct{}{}

		instructions = append(instructions, CopyInstruction{
			SourceBucket:        source.Bucket,
			SourceKey:           source.Prefix + key,
			SourceRootFolderKey: source.Prefix,
			Sums:                strings.TrimSpace(object.Sums),
		})
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Ordered by key so the document a given dataset produces does not depend on the
	// order the caller happened to discover its objects in.
	slices.SortFunc(instructions, func(a, b CopyInstruction) int {
		return cmp.Compare(a.SourceKey, b.SourceKey)
	})
	return instructions, nil
}

// checkObjectKey validates one object's key relative to the dataset prefix.
func checkObjectKey(key string, index int) (string, error) {
	trimmed := strings.TrimSpace(key)
	switch {
	case trimmed == "":
		return "", fmt.Errorf("object %d has no key", index)
	case strings.HasPrefix(trimmed, "/"):
		return "", fmt.Errorf(
			"object key %q must be relative to the dataset's prefix, so it cannot begin "+
				"with a slash", key)
	case strings.ContainsAny(trimmed, "*?"):
		return "", fmt.Errorf(
			"object key %q must not contain wildcards; omit the object list entirely to "+
				"have the whole dataset copied", key)
	case trimmed == ControlPrefix || strings.HasPrefix(trimmed, ControlPrefix+"/"):
		// The destination mirrors these keys, and the control prefix there is reserved
		// for the copy's own artefacts and denied to the researcher. An object copied
		// into it would be unreadable by the party it was released to, and could
		// overwrite the copy's summary of the run.
		return "", fmt.Errorf(
			"object key %q sits under %q, which is reserved for the copy's own artefacts "+
				"and is not readable by a researcher", key, ControlPrefix+"/")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("object key %q contains a relative path segment", key)
		}
	}
	return trimmed, nil
}

// marshalInstructions renders the instructions as JSONL: one compact object per line,
// each line newline-terminated.
func marshalInstructions(instructions []CopyInstruction) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// Keys are object keys, and escaping their ampersands and angle brackets would make
	// the document harder to read against a bucket listing without making it safer.
	encoder.SetEscapeHTML(false)
	for _, instruction := range instructions {
		if err := encoder.Encode(instruction); err != nil {
			return nil, fmt.Errorf("rendering copy instruction for %q: %w", instruction.SourceKey, err)
		}
	}
	return buf.Bytes(), nil
}

// controlKey positions one of the copy's own artefacts under the reserved prefix.
func controlKey(name string) string {
	return ControlPrefix + "/" + name
}

// terminatePrefix returns prefix with a trailing slash, leaving an empty prefix empty.
// Both the copy service's prefixes and a dataset's prefix have to be slash-terminated to
// concatenate correctly.
func terminatePrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" || strings.HasSuffix(trimmed, "/") {
		return trimmed
	}
	return trimmed + "/"
}

// s3URI renders a bucket and key as a URI, for reporting and for recording in the
// ledger.
func s3URI(bucket, key string) string {
	return "s3://" + bucket + "/" + key
}
