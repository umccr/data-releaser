package config

// These tests are about the split itself rather than any individual field check: a file
// carrying only one mechanism's configuration must satisfy that mechanism's sections and
// must fail the other's. What is being guarded against is a command refusing to start
// over fields it never reads — the htsget warehouse and endpoints blocking a direct-read
// command, or the copy orchestration blocking an htsget one.

import (
	"errors"
	"strings"
	"testing"
)

// The two mechanisms' sections, plus the ones they genuinely share. A command names a
// subset of one column; nothing names both.
var (
	sharedSections = []Section{SectionAccount, SectionLedger, SectionManifest}

	htsgetOnlySections = []Section{
		SectionWarehouse,
		SectionAuthorizationEndpoint,
		SectionHtsgetEndpoint,
		SectionResearchers,
	}

	directReadOnlySections = []Section{
		SectionCopyService,
		SectionSourceStore,
		SectionReleasePolicies,
		SectionHotCache,
		SectionDatasetManifest,
	}
)

// directReadOnlyConfig carries everything the direct-read mechanism needs and nothing the
// htsget mechanism does. The ledger table, the cache bucket name prefix and the dataset
// manifest key are all left to their defaults, so this also covers those defaults being
// enough to satisfy a check.
const directReadOnlyConfig = `
account: "999988887777"
region: "ap-southeast-2"
manifest:
  bucket: "data-control-manifests"
s3:
  copy:
    stateMachineArn: "arn:aws:states:ap-southeast-2:999988887777:stateMachine:steps-s3-copy"
    workingBucket: "steps-s3-copy-working"
    writerRoleName: "steps-s3-copy-writer"
  source:
    endpointUrl: "https://ceph.example.org"
    region: "us-east-1"
    credentialsSecret: "source-store/credentials"
  custodianRoleArn: "arn:aws:iam::999988887777:role/DatasetCustodian"
`

// htsgetOnlyConfig carries everything the htsget mechanism needs and nothing the
// direct-read mechanism does. It names no account or region deliberately: no htsget
// command writes a policy document, so neither value means anything to one.
const htsgetOnlyConfig = `
manifest:
  bucket: "data-control-manifests"
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
researchers:
  "777788889999":
    targetVpcArn: "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-0123456789abcdef0"
`

// TestRequire_DirectReadConfigNeedsNoHtsgetFields is the case that motivated splitting
// the checks: a warehouse and an htsget base URL are not prerequisites of granting a
// researcher direct read access, and requiring them globally made them so.
func TestRequire_DirectReadConfigNeedsNoHtsgetFields(t *testing.T) {
	cfg := loadConfigContent(t, directReadOnlyConfig)

	if err := cfg.Require(append(sharedSections, directReadOnlySections...)...); err != nil {
		t.Fatalf("a direct-read configuration should satisfy its own sections: %v", err)
	}

	// The same file fails the other mechanism's sections, and says which fields are
	// missing rather than that the file is bad.
	err := cfg.Require(htsgetOnlySections...)
	requireErrorNames(t, err,
		"warehouse.athenaWorkgroup",
		"warehouse.database",
		"endpoints.authorization",
		"endpoints.htsgetBaseUrl",
	)

	// The two manifests share a bucket by default, so a deployment names it once.
	if cfg.S3.Manifest.Bucket != cfg.Manifest.Bucket {
		t.Errorf("expected the dataset manifest to default to the shared manifest bucket %q, got %q",
			cfg.Manifest.Bucket, cfg.S3.Manifest.Bucket)
	}
	if cfg.S3.Manifest.KeyTemplate == cfg.Manifest.KeyTemplate {
		t.Errorf("the two manifests must not default to one key, or a researcher holding both access "+
			"kinds has one document overwrite the other; both resolved to %q", cfg.S3.Manifest.KeyTemplate)
	}
}

// TestRequire_HtsgetConfigNeedsNoDirectReadFields is the same rule in the other
// direction, including the account: an htsget command composes no policy document, so it
// starts without one being configured.
func TestRequire_HtsgetConfigNeedsNoDirectReadFields(t *testing.T) {
	cfg := loadConfigContent(t, htsgetOnlyConfig)

	if err := cfg.Require(append([]Section{SectionLedger, SectionManifest}, htsgetOnlySections...)...); err != nil {
		t.Fatalf("an htsget configuration should satisfy its own sections: %v", err)
	}

	err := cfg.Require(directReadOnlySections...)
	requireErrorNames(t, err,
		"s3.copy.stateMachineArn",
		"s3.copy.workingBucket",
		"s3.copy.writerRoleName",
		"s3.source.endpointUrl",
		"s3.source.credentialsSecret",
		// Reached through the implied account section rather than named directly.
		"account is required",
		"region is required",
	)
}

// TestRequire_ImpliedSectionsAreChecked covers a section naming what it in turn depends
// on, so a caller states what it reads without having to know that a policy document also
// needs the account it names as a principal.
func TestRequire_ImpliedSectionsAreChecked(t *testing.T) {
	cfg := loadConfigContent(t, `
s3:
  copy:
    stateMachineArn: "arn:aws:states:ap-southeast-2:999988887777:stateMachine:steps-s3-copy"
    workingBucket: "steps-s3-copy-working"
`)

	// Every field of the named section is present, so the only failures left are the
	// implied ones.
	err := cfg.Require(SectionCopyService)
	requireErrorNames(t, err, "account is required", "region is required")
	if strings.Contains(errText(err), "s3.copy") {
		t.Errorf("the named section is satisfied and should report nothing, got:\n%s", errText(err))
	}
}

// TestRequire_ChecksOnlyWhatIsNamed pins the behaviour the split exists for: a section
// nobody named is not checked, even when it is plainly unconfigured.
func TestRequire_ChecksOnlyWhatIsNamed(t *testing.T) {
	cfg := &Config{}

	if err := cfg.Require(); err != nil {
		t.Errorf("naming no section should check nothing, got: %v", err)
	}

	// Requiring one unconfigured section reports that section and stays silent about
	// every other unconfigured one.
	err := cfg.Require(SectionHtsgetEndpoint)
	requireErrorNames(t, err, "endpoints.htsgetBaseUrl")
	for _, unnamed := range []string{"warehouse.", "s3.", "ledger.", "manifest."} {
		if strings.Contains(errText(err), unnamed) {
			t.Errorf("expected nothing about %s when it was not named, got:\n%s", unnamed, errText(err))
		}
	}
}

// TestRequire_ReportsEveryFailureAtOnce checks that one run describes the whole of what
// is wrong, and in the file's own order rather than the order the caller happened to
// name its sections in.
func TestRequire_ReportsEveryFailureAtOnce(t *testing.T) {
	cfg := &Config{}

	err := cfg.Require(SectionCopyService, SectionLedger, SectionAccount)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a collected validation failure, got %v", err)
	}

	indexOf := func(field string) int {
		for i, msg := range verr.Errors {
			if strings.Contains(msg, field) {
				return i
			}
		}
		t.Errorf("expected the failure to name %q, got:\n%s", field, err.Error())
		return -1
	}
	account, ledger, copyService := indexOf("account is required"), indexOf("ledger.table"), indexOf("s3.copy.stateMachineArn")
	if account < 0 || ledger < 0 || copyService < 0 {
		return
	}
	if !(account < ledger && ledger < copyService) {
		t.Errorf("expected failures ordered from the most general section to the most specific, got:\n%s", err.Error())
	}
}

func TestRequire_UnknownSection(t *testing.T) {
	cfg := &Config{}
	err := cfg.Require(Section("htsget"))
	requireErrorNames(t, err, `unknown configuration section "htsget"`)
}

// TestRequire_ResearcherVpcMustBeTheFormTheCalloutSees rejects the direct-read
// mechanism's form of a VPC where the htsget mechanism's is expected. The two are
// near-homographs for different values — a full EC2 VPC ARN matched against the identity
// forwarded to the callout, against a bare vpc- identifier compared inside a bucket
// policy condition — and the wrong one denies every request without ever failing at the
// point it was written.
func TestRequire_ResearcherVpcMustBeTheFormTheCalloutSees(t *testing.T) {
	cases := []struct {
		name string
		arn  string
	}{
		{"bare vpc identifier", "vpc-0123456789abcdef0"},
		{"vpc-lattice association", "arn:aws:vpc-lattice:ap-southeast-2:777788889999:servicenetworkserviceassociation/*"},
		{"another account's vpc", "arn:aws:ec2:ap-southeast-2:111111111111:vpc/vpc-0123456789abcdef0"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadConfigContent(t, `
researchers:
  "777788889999":
    targetVpcArn: "`+c.arn+`"
`)
			err := cfg.Require(SectionResearchers)
			requireErrorNames(t, err, "researchers.777788889999.targetVpcArn")
		})
	}
}

// TestRequire_HotCacheBucketCollision covers two dataset URIs pinned to one bucket. One
// bucket holds one key, so purging either dataset would destroy both.
func TestRequire_HotCacheBucketCollision(t *testing.T) {
	cfg := loadConfigContent(t, `
account: "999988887777"
region: "ap-southeast-2"
s3:
  hotCache:
    buckets:
      "s3://source/dataset-a/": "shared-cache-bucket"
      "s3://source/dataset-b/": "shared-cache-bucket"
`)

	err := cfg.Require(SectionHotCache)
	requireErrorNames(t, err, "dataset-a", "dataset-b", "shared-cache-bucket")
}

// TestResolveBucketNameHashKey covers the refusal that keeps a derived bucket name
// honest. Treating the key as merely absent would derive the legible name instead — a
// different name, so a different bucket — and the tool would then look for a released
// dataset in a bucket that holds nothing.
func TestResolveBucketNameHashKey(t *testing.T) {
	t.Run("legible names need no key", func(t *testing.T) {
		t.Setenv(BucketNameHashKeyEnv, "")
		key, err := HotCacheConfig{}.ResolveBucketNameHashKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key != "" {
			t.Errorf("expected no key when names are derived legibly, got %q", key)
		}
	})

	for name, missing := range map[string]string{"unset": "", "whitespace only": "   "} {
		t.Run("obscured names refuse a key that is "+name, func(t *testing.T) {
			t.Setenv(BucketNameHashKeyEnv, missing)
			_, err := HotCacheConfig{ObscureBucketNames: true}.ResolveBucketNameHashKey()
			requireErrorNames(t, err, BucketNameHashKeyEnv)
		})
	}

	t.Run("obscured names take the key from the environment", func(t *testing.T) {
		t.Setenv(BucketNameHashKeyEnv, "  a-shared-secret  ")
		key, err := HotCacheConfig{ObscureBucketNames: true}.ResolveBucketNameHashKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key != "a-shared-secret" {
			t.Errorf("expected the trimmed key, got %q", key)
		}
	})

	// The section check surfaces the same refusal, so a command that derives a bucket
	// name fails before it looks for the bucket.
	t.Run("reported by the section check", func(t *testing.T) {
		t.Setenv(BucketNameHashKeyEnv, "")
		cfg := loadConfigContent(t, `
account: "999988887777"
region: "ap-southeast-2"
s3:
  hotCache:
    obscureBucketNames: true
`)
		err := cfg.Require(SectionHotCache)
		requireErrorNames(t, err, BucketNameHashKeyEnv)
	})
}

// loadConfigContent writes content to a temporary file and loads it, so what is checked
// is a configuration that arrived the way a real one does — defaults applied, nothing
// validated.
func loadConfigContent(t *testing.T, content string) *Config {
	t.Helper()
	cfg, err := Load(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	return cfg
}

// requireErrorNames asserts that err reports each field, so a failure tells the operator
// what to fix rather than only that something is wrong.
func requireErrorNames(t *testing.T, err error, fields ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a failure naming %v, got none", fields)
	}
	for _, field := range fields {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("expected the failure to name %q, got:\n%s", field, err.Error())
		}
	}
}

// errText renders an error for assertions that expect silence about something.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
