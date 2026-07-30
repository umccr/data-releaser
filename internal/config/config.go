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
	Profile    string           `yaml:"profile"`
	Warehouse  WarehouseConfig  `yaml:"warehouse"`
	Endpoints  EndpointsConfig  `yaml:"endpoints"`
	Cache      CacheConfig      `yaml:"cache"`
	Validation ValidationConfig `yaml:"validation"`
	Manifest   ManifestConfig   `yaml:"manifest"`
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
