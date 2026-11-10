package config

// Configuration is validated per section, on demand, by the command that reads it.
//
// A single global check does not work here. The two release mechanisms share a file but
// almost no fields, so requiring everything meant a command could be blocked by a field
// belonging to the other mechanism — configuration it neither reads nor could act on.
// Naming sections instead keeps every failure attributable: the operator is told which
// field is missing, and only about fields the command they ran actually needs.
//
// Every check here guards a value whose wrongness is invisible at the point of use.
// A malformed ARN names no principal, a VPC identifier in the wrong form matches no
// request, and a bucket name derived from the wrong prefix points at a bucket that holds
// nothing. All three present as an access denial long after the mistake was made, so the
// config file is the last place they are still legible.

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"

	"github.com/umccr/data-releaser/internal/model"
)

// Section names one group of configuration fields that a command can declare a
// dependency on.
type Section string

// Sections shared by both mechanisms.
const (
	// SectionAccount is the account and region every resource lives in.
	SectionAccount Section = "account"
	// SectionLedger is the record store both mechanisms write to.
	SectionLedger Section = "ledger"
	// SectionManifest is the shared manifest bucket and the htsget document's key.
	SectionManifest Section = "manifest"
)

// Sections belonging to the htsget mechanism.
const (
	// SectionWarehouse is the Athena workgroup and database libraries are resolved
	// through.
	SectionWarehouse Section = "warehouse"
	// SectionHtsgetEndpoint is the htsget-rs base URL embedded in researcher manifests.
	SectionHtsgetEndpoint Section = "htsget-endpoint"
	// SectionAuthorizationEndpoint is the authorization callout's URL.
	SectionAuthorizationEndpoint Section = "authorization-endpoint"
	// SectionResearchers is the per-account VPC ARN the callout is keyed on.
	SectionResearchers Section = "researchers"
)

// Sections belonging to the direct-read mechanism.
const (
	// SectionCopyService locates the copy orchestration and its working bucket.
	SectionCopyService Section = "copy-service"
	// SectionSourceStore is the external store datasets are copied out of.
	SectionSourceStore Section = "source-store"
	// SectionReleasePolicies is what the bucket and key policies need beyond the release
	// itself: the role a copy writes through, and the custodian permitted to withdraw.
	SectionReleasePolicies Section = "release-policies"
	// SectionHotCache is how a dataset's cache bucket name is arrived at.
	SectionHotCache Section = "hot-cache"
	// SectionDatasetManifest is where the dataset access document is delivered.
	SectionDatasetManifest Section = "dataset-manifest"
)

// sectionSpec is one section's dependencies and its checks.
type sectionSpec struct {
	// requires are sections that are implied by this one, so a command names only what
	// it reads rather than having to know what those fields in turn depend on.
	requires []Section
	// check reports every problem it finds, so one run tells the operator everything
	// they have to fix.
	check func(cfg *Config) []string
}

// sectionOrder fixes the order failures are reported in, from the most general section
// to the most specific, so the output does not depend on the order a command happened to
// name its sections in.
var sectionOrder = []Section{
	SectionAccount,
	SectionLedger,
	SectionManifest,
	SectionWarehouse,
	SectionAuthorizationEndpoint,
	SectionHtsgetEndpoint,
	SectionResearchers,
	SectionCopyService,
	SectionSourceStore,
	SectionReleasePolicies,
	SectionHotCache,
	SectionDatasetManifest,
}

var sectionSpecs = map[Section]sectionSpec{
	SectionAccount:               {check: checkAccount},
	SectionLedger:                {check: checkLedger},
	SectionManifest:              {check: checkManifest},
	SectionWarehouse:             {check: checkWarehouse},
	SectionAuthorizationEndpoint: {check: checkAuthorizationEndpoint},
	SectionHtsgetEndpoint:        {check: checkHtsgetEndpoint},
	SectionResearchers:           {check: checkResearchers},
	SectionCopyService:           {requires: []Section{SectionAccount}, check: checkCopyService},
	SectionSourceStore:           {check: checkSourceStore},
	SectionReleasePolicies:       {requires: []Section{SectionAccount}, check: checkReleasePolicies},
	SectionHotCache:              {requires: []Section{SectionAccount}, check: checkHotCache},
	SectionDatasetManifest:       {check: checkDatasetManifest},
}

// Require checks the named sections and every section they imply, reporting all failures
// together.
//
// Sections not named are not checked, which is the whole point: a command that grants
// direct S3 access has no business failing because the warehouse it never queries is
// unconfigured.
func (cfg *Config) Require(sections ...Section) error {
	wanted := make(map[Section]bool, len(sections))
	var want func(Section)
	want = func(s Section) {
		if wanted[s] {
			return
		}
		wanted[s] = true
		for _, dep := range sectionSpecs[s].requires {
			want(dep)
		}
	}

	var errs []string
	for _, s := range sections {
		if _, known := sectionSpecs[s]; !known {
			errs = append(errs, fmt.Sprintf("unknown configuration section %q", s))
			continue
		}
		want(s)
	}

	for _, s := range sectionOrder {
		if wanted[s] {
			errs = append(errs, sectionSpecs[s].check(cfg)...)
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

func checkAccount(cfg *Config) []string {
	var errs []string
	if account := strings.TrimSpace(cfg.Account); account == "" {
		errs = append(errs, "account is required: it is the account owning the ledger, the manifest bucket, and every dataset bucket and key")
	} else if !isAccountID(account) {
		errs = append(errs, fmt.Sprintf("account %q is not a 12-digit AWS account id; it is written into policy documents as a principal, so an alias names nothing", cfg.Account))
	}
	if strings.TrimSpace(cfg.Region) == "" {
		errs = append(errs, "region is required: a dataset key policy confines the researcher's decrypt permission to S3 in one region and cannot be rendered without it")
	}
	return errs
}

func checkLedger(cfg *Config) []string {
	if strings.TrimSpace(cfg.Ledger.Table) == "" {
		return []string{"ledger.table is required: it is the record table the deployment creates, and its name must be taken from that deployment's output"}
	}
	return nil
}

func checkManifest(cfg *Config) []string {
	return checkManifestTarget("manifest", cfg.Manifest)
}

func checkDatasetManifest(cfg *Config) []string {
	return checkManifestTarget("s3.manifest", cfg.S3.Manifest)
}

// checkManifestTarget checks one manifest delivery target. The template is rendered
// against a placeholder account so an unsupported substitution is caught here rather
// than when a researcher's manifest is first written.
func checkManifestTarget(path string, m ManifestConfig) []string {
	var errs []string
	if strings.TrimSpace(m.Bucket) == "" {
		errs = append(errs, fmt.Sprintf("%s.bucket is required: the bucket name must be taken from the deployment's output rather than assumed", path))
	}
	if _, err := m.RenderManifestKey("000000000000"); err != nil {
		errs = append(errs, fmt.Sprintf("%s.keyTemplate: %v", path, err))
	}
	return errs
}

func checkWarehouse(cfg *Config) []string {
	var errs []string
	if strings.TrimSpace(cfg.Warehouse.AthenaWorkgroup) == "" {
		errs = append(errs, "warehouse.athenaWorkgroup is required")
	}
	if strings.TrimSpace(cfg.Warehouse.Database) == "" {
		errs = append(errs, "warehouse.database is required")
	}
	return errs
}

func checkAuthorizationEndpoint(cfg *Config) []string {
	if strings.TrimSpace(cfg.Endpoints.Authorization) == "" {
		return []string{"endpoints.authorization is required"}
	}
	return nil
}

func checkHtsgetEndpoint(cfg *Config) []string {
	if strings.TrimSpace(cfg.Endpoints.HtsgetBaseURL) == "" {
		return []string{"endpoints.htsgetBaseUrl is required"}
	}
	return nil
}

// checkResearchers rejects malformed researcher VPC ARNs. They are only read when a
// share is created, and a bad value there fails silently at request time rather than
// loudly at write time.
func checkResearchers(cfg *Config) []string {
	var errs []string
	for _, accountID := range sortedKeys(cfg.Researchers) {
		arn := strings.TrimSpace(cfg.Researchers[accountID].TargetVpcArn)
		if arn == "" {
			errs = append(errs, fmt.Sprintf("researchers.%s.targetVpcArn is required", accountID))
			continue
		}
		if err := model.ValidateTargetVpcArn(arn, accountID); err != nil {
			errs = append(errs, fmt.Sprintf("researchers.%s.targetVpcArn: %v", accountID, err))
		}
	}
	return errs
}

func checkCopyService(cfg *Config) []string {
	copyCfg := cfg.S3.Copy
	var errs []string

	if arn := strings.TrimSpace(copyCfg.StateMachineArn); arn == "" {
		errs = append(errs, "s3.copy.stateMachineArn is required: the copy orchestration is deployed separately and this tool starts it by ARN")
	} else if !awsarn.IsARN(arn) {
		errs = append(errs, fmt.Sprintf("s3.copy.stateMachineArn %q is not an ARN", copyCfg.StateMachineArn))
	}
	if strings.TrimSpace(copyCfg.WorkingBucket) == "" {
		errs = append(errs, "s3.copy.workingBucket is required: it is where the copy orchestration reads its instructions from, and it belongs to that orchestration's installation")
	}
	if account := strings.TrimSpace(copyCfg.Account); account != "" && !isAccountID(account) {
		errs = append(errs, fmt.Sprintf("s3.copy.account %q is not a 12-digit AWS account id; leave it unset when the copy orchestration is installed alongside the dataset buckets", copyCfg.Account))
	}
	for _, setting := range []struct {
		name  string
		value int
	}{
		{"headConcurrency", copyCfg.Performance.HeadConcurrency},
		{"smallCopyConcurrency", copyCfg.Performance.SmallCopyConcurrency},
		{"smallCopyBatchSize", copyCfg.Performance.SmallCopyBatchSize},
		{"largeCopyConcurrency", copyCfg.Performance.LargeCopyConcurrency},
	} {
		if setting.value < 0 {
			errs = append(errs, fmt.Sprintf("s3.copy.performance.%s must not be negative, got %d; leave it at zero to keep the copy orchestration's own default", setting.name, setting.value))
		}
	}
	return errs
}

func checkSourceStore(cfg *Config) []string {
	source := cfg.S3.Source
	var errs []string

	if err := checkEndpointURL(strings.TrimSpace(source.EndpointURL)); err != nil {
		errs = append(errs, fmt.Sprintf("s3.source.endpointUrl: %v", err))
	}
	if strings.TrimSpace(source.CredentialsSecret) == "" {
		errs = append(errs, "s3.source.credentialsSecret is required: it names the secret the copy orchestration resolves the source store's credentials from, which never pass through this tool")
	}
	return errs
}

// checkEndpointURL checks the shape of the source store's endpoint. Whether it resolves,
// and whether it is routable from the subnets the copy orchestration runs in, is a
// property of the deployment that this tool is in no position to test.
func checkEndpointURL(endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("required: the source store is reached through a custom endpoint")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", endpoint, err)
	}
	// Plaintext is permitted because the source store's transport is its operator's
	// choice, and the copy orchestration is the only thing that connects to it.
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("%q must be an http or https URL", endpoint)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%q names no host", endpoint)
	}
	return nil
}

func checkReleasePolicies(cfg *Config) []string {
	var errs []string

	if strings.TrimSpace(cfg.S3.Copy.WriterRoleName) == "" {
		errs = append(errs, "s3.copy.writerRoleName is required: each dataset key policy has to name the role a copy writes through, or the copy is denied the data key it needs partway through a transfer")
	}
	if account := strings.TrimSpace(cfg.S3.Copy.WriterRoleAccount); account != "" && !isAccountID(account) {
		errs = append(errs, fmt.Sprintf("s3.copy.writerRoleAccount %q is not a 12-digit AWS account id; leave it unset when that role lives alongside the dataset buckets", cfg.S3.Copy.WriterRoleAccount))
	}
	if custodian := strings.TrimSpace(cfg.S3.CustodianRoleArn); custodian != "" && !awsarn.IsARN(custodian) {
		errs = append(errs, fmt.Sprintf("s3.custodianRoleArn %q is not an ARN; the key policy names the principal permitted to withdraw a dataset and cannot guess at it. Leave it unset to keep that power with the account's key administrators", cfg.S3.CustodianRoleArn))
	}
	return errs
}

func checkHotCache(cfg *Config) []string {
	hotCache := cfg.S3.HotCache
	var errs []string

	if strings.TrimSpace(hotCache.NamePrefix) == "" {
		errs = append(errs, "s3.hotCache.namePrefix is required: bucket names are derived from it, and a prefix differing from the deployment's derives a different bucket")
	}
	if _, err := hotCache.ResolveBucketNameHashKey(); err != nil {
		errs = append(errs, err.Error())
	}

	// Two dataset URIs pinned to one bucket would put two datasets in a bucket holding
	// one key, so purging either destroys both.
	claimed := make(map[string]string, len(hotCache.Buckets))
	for _, datasetURI := range sortedKeys(hotCache.Buckets) {
		bucket := strings.TrimSpace(hotCache.Buckets[datasetURI])
		switch {
		case strings.TrimSpace(datasetURI) == "":
			errs = append(errs, "s3.hotCache.buckets has an entry with no dataset URI")
		case bucket == "":
			errs = append(errs, fmt.Sprintf("s3.hotCache.buckets[%q] names no bucket; remove the entry to have the name derived", datasetURI))
		default:
			if other, taken := claimed[bucket]; taken {
				errs = append(errs, fmt.Sprintf("s3.hotCache.buckets maps both %q and %q to bucket %q; one dataset gets one bucket, because purging destroys the key that bucket is encrypted with", other, datasetURI, bucket))
				continue
			}
			claimed[bucket] = datasetURI
		}
	}
	return errs
}

// isAccountID reports whether s is an AWS account id: exactly twelve digits.
func isAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// sortedKeys orders a map's keys so that failures are reported in a stable order rather
// than the order Go happens to iterate in.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
