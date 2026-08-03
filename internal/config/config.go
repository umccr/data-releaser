// Package config handles loading and validating the Data Releaser configuration
// from YAML files and CLI flag overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the full application configuration.
type Config struct {
	Profile       string              `yaml:"profile"`
	Warehouse     WarehouseConfig     `yaml:"warehouse"`
	Endpoints     EndpointsConfig     `yaml:"endpoints"`
	Cache         CacheConfig         `yaml:"cache"`
	Validation    ValidationConfig    `yaml:"validation"`
	Manifest      ManifestConfig      `yaml:"manifest"`
	Authorization AuthorizationConfig `yaml:"authorization"`
	// Researchers maps a researcher AWS account ID to its per-account settings.
	Researchers map[string]ResearcherConfig `yaml:"researchers"`
}

// AuthorizationConfig holds settings for the Authorization Service's store.
type AuthorizationConfig struct {
	// ShareRecordsTable is the DynamoDB table holding Share_Records. The
	// Infrastructure_Stack creates it as "<namePrefix>-ShareRecords", so this must be
	// sourced from that stack's output rather than hard-coded. Defaults to
	// DefaultShareRecordsTable, which matches the stack's default name prefix.
	ShareRecordsTable string `yaml:"shareRecordsTable"`
}

// DefaultShareRecordsTable is the Share_Records table name produced by the
// Infrastructure_Stack under its default "data-releaser" name prefix.
const DefaultShareRecordsTable = "data-releaser-ShareRecords"

// ResearcherConfig holds the per-account settings for one Researcher_Account.
type ResearcherConfig struct {
	// TargetVpcArn is the ARN of the researcher VPC that is associated with the
	// htsget VPC Lattice service network.
	//
	// This is the identity the authorization callout is keyed on, and it must be the
	// value htsget-rs actually forwards. VPC Lattice puts the *EC2 VPC* ARN of the
	// calling VPC on the invocation event
	// (`$.requestContext.identity.sourceVpcArn`), so the form is
	// "arn:aws:ec2:<region>:<account>:vpc/vpc-<id>" — not a vpc-lattice ARN. The
	// authorization lookup is an exact-match query on a DynamoDB GSI, so any other
	// value silently denies every request.
	TargetVpcArn string `yaml:"targetVpcArn"`
}

// vpcArnRe matches an EC2 VPC ARN, the form VPC Lattice reports as the source of an
// invocation and therefore the only form that can match a forwarded identity.
var vpcArnRe = regexp.MustCompile(`^arn:aws[a-z-]*:ec2:[a-z0-9-]+:\d{12}:vpc/vpc-[0-9a-f]+$`)

// ValidateTargetVpcArn checks that arn is a well-formed EC2 VPC ARN and, when
// accountID is non-empty, that it belongs to that account. Catching this at share
// time matters because a malformed ARN produces no error downstream — it is simply
// written to the store and then never matches an authorization callout.
func ValidateTargetVpcArn(arn, accountID string) error {
	if !vpcArnRe.MatchString(arn) {
		return fmt.Errorf(
			"target VPC ARN %q is not a valid EC2 VPC ARN; expected the form "+
				"arn:aws:ec2:<region>:<account-id>:vpc/vpc-<id>, which is what VPC Lattice "+
				"reports as the source of a request", arn)
	}
	if accountID != "" {
		parts := strings.Split(arn, ":")
		if parts[4] != accountID {
			return fmt.Errorf(
				"target VPC ARN %q belongs to account %s but the share targets account %s",
				arn, parts[4], accountID)
		}
	}
	return nil
}

// ResolveTargetVpcArn returns the configured target VPC ARN for a researcher account.
// It returns an error when the account has no entry, because there is no safe value to
// fall back to: a guessed ARN would never match a real request.
func (cfg *Config) ResolveTargetVpcArn(accountID string) (string, error) {
	researcher, ok := cfg.Researchers[accountID]
	if !ok || strings.TrimSpace(researcher.TargetVpcArn) == "" {
		return "", fmt.Errorf(
			"no target VPC ARN configured for researcher account %s; add it under "+
				"researchers.%s.targetVpcArn in the config file, or pass --target-vpc-arn. "+
				"It is the ARN of the researcher VPC associated with the htsget VPC Lattice "+
				"service network, e.g. arn:aws:ec2:ap-southeast-2:%s:vpc/vpc-0123456789abcdef0",
			accountID, accountID, accountID)
	}

	arn := strings.TrimSpace(researcher.TargetVpcArn)
	if err := ValidateTargetVpcArn(arn, accountID); err != nil {
		return "", fmt.Errorf("researchers.%s.targetVpcArn: %w", accountID, err)
	}
	return arn, nil
}

// ManifestConfig holds the Manifest_Bucket settings. The bucket is a single
// same-account Data Control bucket (sourced from an Infrastructure_Stack output);
// each Researcher_Account's manifest lives under a per-account key prefix.
type ManifestConfig struct {
	// Bucket is the Data Control Manifest_Bucket name. It is never hard-coded and
	// is sourced from the Infrastructure_Stack output (Req 7.2, 7.9).
	Bucket string `yaml:"bucket"`
	// KeyTemplate is the per-account object key template. It supports the
	// {accountId} placeholder and defaults to DefaultManifestKeyTemplate.
	KeyTemplate string `yaml:"keyTemplate"`
}

// DefaultManifestKeyTemplate is the default per-account manifest key template.
const DefaultManifestKeyTemplate = "manifests/{accountId}/manifest.json"

// manifestPlaceholderRe matches {...} placeholder tokens in a key template.
var manifestPlaceholderRe = regexp.MustCompile(`\{[^}]*\}`)

// RenderManifestKey renders the manifest key template for the given account ID by
// substituting the {accountId} placeholder. Any other {...} token is rejected with
// a descriptive error so misconfigured templates fail fast (Req 7.2).
func (m ManifestConfig) RenderManifestKey(accountID string) (string, error) {
	var rerr error
	rendered := manifestPlaceholderRe.ReplaceAllStringFunc(m.KeyTemplate, func(token string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(token, "{"), "}")
		switch name {
		case "accountId":
			return accountID
		default:
			if rerr == nil {
				rerr = fmt.Errorf("unknown placeholder %q in manifest.keyTemplate %q (only {accountId} is supported)", token, m.KeyTemplate)
			}
			return token
		}
	})
	if rerr != nil {
		return "", rerr
	}
	return rendered, nil
}

// RenderManifestPrefix renders the per-account manifest key prefix (the directory
// portion of the rendered key with a trailing "*"), for example
// "manifests/<accountId>/*". Used to describe the researcher's cross-account read scope.
func (m ManifestConfig) RenderManifestPrefix(accountID string) (string, error) {
	key, err := m.RenderManifestKey(accountID)
	if err != nil {
		return "", err
	}
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		return key[:idx+1] + "*", nil
	}
	return "*", nil
}

// WarehouseConfig holds OrcaVault Mart (Athena/Lake Formation) settings.
type WarehouseConfig struct {
	AthenaWorkgroup string `yaml:"athenaWorkgroup"`
	Database        string `yaml:"database"`
	Catalog         string `yaml:"catalog"`
}

// EndpointsConfig holds service endpoint URLs.
type EndpointsConfig struct {
	Authorization string `yaml:"authorization"`
	HtsgetBaseURL string `yaml:"htsgetBaseUrl"`
	// APIGatewayRestAPIID is the REST API ID for the Authorization Service API Gateway.
	// Used by the `validate` command's API Gateway stage check (Req 15.10).
	APIGatewayRestAPIID string `yaml:"apiGatewayRestApiId"`
	// APIGatewayStageName is the stage name for the Authorization Service API Gateway.
	// Used by the `validate` command's API Gateway stage check (Req 15.10). Defaults to "prod".
	APIGatewayStageName string `yaml:"apiGatewayStageName"`
}

// CacheConfig holds cache TTL settings for the authorization response.
type CacheConfig struct {
	TTLCeiling Duration `yaml:"ttlCeiling"`
	DenyTTL    Duration `yaml:"denyTtl"`
}

// ValidationConfig holds optional validation/canary settings.
type ValidationConfig struct {
	Canary *CanaryConfig `yaml:"canary"`

	// SkipHtsgetCheck disables the direct htsget-rs /service-info health check.
	//
	// Set this when htsget-rs is fronted by VPC Lattice: its domain only resolves from
	// inside an associated VPC, so the check fails from an operator's machine even
	// though the deployment is healthy. Verify htsget-rs from inside the consumer VPC
	// instead.
	SkipHtsgetCheck bool `yaml:"skipHtsgetCheck"`

	// SkipAuthorizationCheck disables the Authorization Service reachability probe.
	//
	// Set this when the authorization API is deployed as a private API Gateway
	// endpoint (the default): it is served by an interface endpoint inside the data
	// account VPC, so its hostname does not resolve from an operator's machine and
	// the resource policy denies anything arriving another way. The check would fail
	// on a healthy deployment. Verify the callout from inside that VPC instead.
	SkipAuthorizationCheck bool `yaml:"skipAuthorizationCheck"`
}

// CanaryConfig holds the canary dataset reference for test-setup mode.
type CanaryConfig struct {
	LibraryID string `yaml:"libraryId"`
	Account   string `yaml:"account"`
	Bucket    string `yaml:"bucket"`
	Key       string `yaml:"key"`
}

// Duration is a time.Duration that supports YAML unmarshaling from strings like "1h", "60s".
type Duration struct {
	time.Duration
}

// UnmarshalYAML parses a duration string (e.g. "1h", "60s", "30m") into a Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML serializes a Duration back to its string form.
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// DefaultCacheTTLCeiling is the default max-age ceiling for authorization responses (Req 13.1).
const DefaultCacheTTLCeiling = 1 * time.Hour

// DefaultCacheDenyTTL is the default negative-cache TTL for deny responses (Req 13.2).
const DefaultCacheDenyTTL = 60 * time.Second

// Load reads configuration from the specified file path, applies defaults, and validates
// required fields. If configPath is empty, it searches default locations.
func Load(configPath string) (*Config, error) {
	path, err := resolveConfigPath(configPath)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	applyDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// resolveConfigPath determines which config file to load.
// Priority: explicit path > ./config.yaml > ~/.config/data-releaser/config.yaml
func resolveConfigPath(configPath string) (string, error) {
	if configPath != "" {
		if _, err := os.Stat(configPath); err != nil {
			return "", fmt.Errorf("config file not found: %s", configPath)
		}
		return configPath, nil
	}

	// Check current directory
	localPath := "config.yaml"
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	// Check ~/.config/data-releaser/config.yaml
	home, err := os.UserHomeDir()
	if err == nil {
		userPath := filepath.Join(home, ".config", "data-releaser", "config.yaml")
		if _, err := os.Stat(userPath); err == nil {
			return userPath, nil
		}
	}

	return "", fmt.Errorf("no config file found; provide one via --config flag, ./config.yaml, or ~/.config/data-releaser/config.yaml")
}

// applyDefaults sets default values for optional fields.
func applyDefaults(cfg *Config) {
	if cfg.Cache.TTLCeiling.Duration == 0 {
		cfg.Cache.TTLCeiling.Duration = DefaultCacheTTLCeiling
	}
	if cfg.Cache.DenyTTL.Duration == 0 {
		cfg.Cache.DenyTTL.Duration = DefaultCacheDenyTTL
	}
	if cfg.Warehouse.Catalog == "" {
		cfg.Warehouse.Catalog = "AwsDataCatalog"
	}
	if cfg.Endpoints.APIGatewayStageName == "" {
		cfg.Endpoints.APIGatewayStageName = "prod"
	}
	if cfg.Manifest.KeyTemplate == "" {
		cfg.Manifest.KeyTemplate = DefaultManifestKeyTemplate
	}
	if cfg.Authorization.ShareRecordsTable == "" {
		cfg.Authorization.ShareRecordsTable = DefaultShareRecordsTable
	}
}

// validate checks that all required fields are present and valid.
func validate(cfg *Config) error {
	var errs []string

	if cfg.Warehouse.AthenaWorkgroup == "" {
		errs = append(errs, "warehouse.athenaWorkgroup is required")
	}
	if cfg.Warehouse.Database == "" {
		errs = append(errs, "warehouse.database is required")
	}
	if cfg.Endpoints.Authorization == "" {
		errs = append(errs, "endpoints.authorization is required")
	}
	if cfg.Endpoints.HtsgetBaseURL == "" {
		errs = append(errs, "endpoints.htsgetBaseUrl is required")
	}

	// Validate the manifest key template renders and rejects unknown placeholders (Req 7.2).
	if _, err := cfg.Manifest.RenderManifestKey("000000000000"); err != nil {
		errs = append(errs, err.Error())
	}

	// Reject malformed researcher VPC ARNs up front. These are only read at share time,
	// and a bad value there fails silently at request time rather than loudly at write
	// time, so it is worth catching whenever the config is loaded.
	for accountID, researcher := range cfg.Researchers {
		arn := strings.TrimSpace(researcher.TargetVpcArn)
		if arn == "" {
			errs = append(errs, fmt.Sprintf("researchers.%s.targetVpcArn is required", accountID))
			continue
		}
		if err := ValidateTargetVpcArn(arn, accountID); err != nil {
			errs = append(errs, fmt.Sprintf("researchers.%s.targetVpcArn: %v", accountID, err))
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// ValidationError collects multiple config validation failures.
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string {
	msg := "configuration validation failed:"
	for _, err := range e.Errors {
		msg += "\n  - " + err
	}
	return msg
}

// ApplyFlagOverrides updates the config with any CLI flag values that were explicitly set.
// Only non-empty overrides are applied.
func (cfg *Config) ApplyFlagOverrides(overrides FlagOverrides) {
	if overrides.Profile != "" {
		cfg.Profile = overrides.Profile
	}
}

// FlagOverrides holds CLI flag values that can override config file settings.
type FlagOverrides struct {
	Profile string
}
