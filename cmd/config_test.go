package cmd

// Configuration reaches a command in two steps: the root loads the file, and the command
// states which sections it reads. These tests cover that seam, because the split is what
// lets one mechanism's command run against a file that says nothing about the other's.
// Before it, a single check ran for every command, so a file with no warehouse and no
// htsget endpoint could not start a command that reads neither.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umccr/data-releaser/internal/config"
)

// The sections belonging to each mechanism, as a command would name them.
var (
	htsgetConfigSections = []config.Section{
		config.SectionWarehouse,
		config.SectionAuthorizationEndpoint,
		config.SectionHtsgetEndpoint,
		config.SectionResearchers,
	}

	directReadConfigSections = []config.Section{
		config.SectionAccount,
		config.SectionCopyService,
		config.SectionSourceStore,
		config.SectionReleasePolicies,
		config.SectionHotCache,
		config.SectionDatasetManifest,
	}
)

// directReadOnlyConfigFile carries what a direct-read release needs and nothing the
// htsget mechanism reads.
const directReadOnlyConfigFile = `
account: "916218008751"
region: "ap-southeast-2"
manifest:
  bucket: "data-control-manifests"
s3:
  copy:
    stateMachineArn: "arn:aws:states:ap-southeast-2:916218008751:stateMachine:steps-s3-copy"
    workingBucket: "steps-s3-copy-working"
    writerRoleName: "steps-s3-copy-writer"
  source:
    endpointUrl: "https://ceph.example.org"
    credentialsSecret: "source-store/credentials"
`

// htsgetOnlyConfigFile carries what an htsget share needs and nothing the direct-read
// mechanism reads.
const htsgetOnlyConfigFile = `
manifest:
  bucket: "data-control-manifests"
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
researchers:
  "534840902377":
    targetVpcArn: "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-0123456789abcdef0"
`

// TestLoadingConfigChecksNothing covers the root's half of the split. Which fields matter
// depends entirely on the command being run, so reading the file cannot be the place a
// missing field is reported.
func TestLoadingConfigChecksNothing(t *testing.T) {
	for name, content := range map[string]string{
		"direct-read only": directReadOnlyConfigFile,
		"htsget only":      htsgetOnlyConfigFile,
		"almost empty":     "profile: \"data-control\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := loadConfigFile(t, content)
			if cfg == nil {
				t.Fatal("expected the loaded configuration to be available to commands")
			}
		})
	}
}

// TestDirectReadConfigIsNotBlockedByHtsgetFields is the case the split was made for: the
// warehouse and the htsget base URL are not prerequisites of granting direct read access,
// and a command that reads neither now starts without them.
func TestDirectReadConfigIsNotBlockedByHtsgetFields(t *testing.T) {
	loadConfigFile(t, directReadOnlyConfigFile)

	if _, err := requireConfig(directReadConfigSections...); err != nil {
		t.Fatalf("a direct-read command should start on a configuration without htsget fields: %v", err)
	}

	// The same file is not enough for the other mechanism, and the refusal names the
	// fields rather than the file.
	_, err := requireConfig(htsgetConfigSections...)
	if err == nil {
		t.Fatal("expected an htsget command to be refused on a configuration with no htsget fields")
	}
	for _, field := range []string{"warehouse.athenaWorkgroup", "endpoints.htsgetBaseUrl"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("expected the refusal to name %q, got:\n%s", field, err.Error())
		}
	}
}

// TestHtsgetConfigIsNotBlockedByDirectReadFields is the same rule in the other direction,
// so adding the second mechanism did not make its configuration a prerequisite of the
// first.
func TestHtsgetConfigIsNotBlockedByDirectReadFields(t *testing.T) {
	loadConfigFile(t, htsgetOnlyConfigFile)

	sections := append([]config.Section{config.SectionLedger, config.SectionManifest}, htsgetConfigSections...)
	if _, err := requireConfig(sections...); err != nil {
		t.Fatalf("an htsget command should start on a configuration without direct-read fields: %v", err)
	}

	_, err := requireConfig(directReadConfigSections...)
	if err == nil {
		t.Fatal("expected a direct-read command to be refused on a configuration with no direct-read fields")
	}
	for _, field := range []string{"s3.copy.stateMachineArn", "s3.source.endpointUrl", "account is required"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("expected the refusal to name %q, got:\n%s", field, err.Error())
		}
	}
}

// TestRequireConfigWithoutLoadedConfig covers the only way a command can reach the
// configuration: there is no accessor that hands it back unchecked, and asking for it
// before the file is read is a failure rather than an empty configuration that would then
// be reported as a missing field.
func TestRequireConfigWithoutLoadedConfig(t *testing.T) {
	previous := appCfg
	t.Cleanup(func() { appCfg = previous })
	appCfg = nil

	if _, err := requireConfig(config.SectionLedger); err == nil {
		t.Error("expected requiring configuration before it is loaded to fail")
	}
}

// loadConfigFile writes content to a temporary file and takes it through the root's own
// loading, so what the assertions run against is a configuration that arrived the way a
// real invocation delivers it.
func loadConfigFile(t *testing.T, content string) *config.Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing a temporary configuration: %v", err)
	}

	previousFile, previousCfg := cfgFile, appCfg
	t.Cleanup(func() { cfgFile, appCfg = previousFile, previousCfg })
	cfgFile = path

	if err := rootCmd.PersistentPreRunE(rootCmd, nil); err != nil {
		t.Fatalf("loading configuration: %v", err)
	}
	return appCfg
}
