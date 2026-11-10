// Package config loads the Data Releaser configuration from a YAML file and CLI flag
// overrides.
//
// The file is divided into sections, and the sections are divided by which release
// mechanism reads them. Loading parses and applies defaults but validates nothing:
// there is no set of fields every command needs, and a single global check meant that
// an S3 command could not start until the htsget warehouse and endpoints were
// configured, even though neither means anything to it.
//
// Each command instead names the sections it actually uses and validates only those,
// through Require. That keeps a missing field reported against the command that needs
// it, and keeps one mechanism's configuration from blocking the other's.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the full application configuration.
//
// Grouping follows which mechanism reads a section, because that is what determines
// whether a given command has to care about it. The shared sections are the ones both
// mechanisms genuinely have in common: the account the resources live in, the record
// store, and the bucket researcher manifests are delivered through.
type Config struct {
	// Profile names an AWS profile or credential source the SDK understands. Empty
	// leaves the default credential chain in place.
	Profile string `yaml:"profile"`

	// Account owns every resource this tool writes to — the ledger table, the manifest
	// bucket, the cache buckets and their keys. It is written into the policy documents
	// as a principal, so it has to be the account id rather than an alias.
	Account string `yaml:"account"`

	// Region is where those resources live. It appears in the dataset key policy as the
	// condition confining a researcher's decrypt permission to S3 in one region, so a
	// wrong value here denies every read rather than failing visibly.
	Region string `yaml:"region"`

	// Ledger locates the record store. Both mechanisms write to it.
	Ledger LedgerConfig `yaml:"ledger"`

	// Manifest is the bucket researcher manifests are delivered through, shared by both
	// mechanisms, together with the key template for the htsget document. The dataset
	// document has its own template under s3.manifest, since the two describe different
	// kinds of access and a researcher may hold both.
	Manifest ManifestConfig `yaml:"manifest"`

	// Warehouse, Endpoints, Cache, Researchers and Validation belong to the htsget
	// mechanism.
	Warehouse WarehouseConfig `yaml:"warehouse"`
	Endpoints EndpointsConfig `yaml:"endpoints"`
	Cache     CacheConfig     `yaml:"cache"`
	// Researchers maps a researcher AWS account ID to its per-account settings.
	Researchers map[string]ResearcherConfig `yaml:"researchers"`
	Validation  ValidationConfig            `yaml:"validation"`

	// S3 belongs to the mechanism that copies a dataset into this account and grants a
	// researcher direct read access to that copy.
	S3 S3Config `yaml:"s3"`
}

// LedgerConfig locates the record store both mechanisms write to.
//
// One table holds the records of both, discriminated by mechanism, so that "what does
// this account have access to?" has a single answer. It is created by the deployment as
// "<namePrefix>-ShareRecords", so the name belongs in configuration sourced from that
// deployment's output rather than compiled in.
type LedgerConfig struct {
	// Table is the DynamoDB table holding the records. Defaults to DefaultLedgerTable,
	// which matches the deployment's default name prefix.
	Table string `yaml:"table"`
}

// DefaultLedgerTable is the record table name produced by the deployment under its
// default "data-releaser" name prefix.
const DefaultLedgerTable = "data-releaser-ShareRecords"

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

// MarshalYAML serializes a Duration back to its string form, so a config round-trips
// through YAML as "1h" rather than a nanosecond count.
func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

// Load reads configuration from the specified file path and applies defaults. If
// configPath is empty, it searches default locations.
//
// Nothing is validated here beyond the file parsing, because which fields are required
// depends entirely on the command being run. Callers state their own requirements with
// Require.
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
	if cfg.Ledger.Table == "" {
		cfg.Ledger.Table = DefaultLedgerTable
	}

	if cfg.S3.HotCache.NamePrefix == "" {
		cfg.S3.HotCache.NamePrefix = DefaultHotCacheNamePrefix
	}
	if cfg.S3.Manifest.KeyTemplate == "" {
		cfg.S3.Manifest.KeyTemplate = DefaultDatasetManifestKeyTemplate
	}
	// The two mechanisms' manifests share a bucket by default and differ only in their
	// key prefix, so a deployment that names one bucket does not have to name it twice.
	if cfg.S3.Manifest.Bucket == "" {
		cfg.S3.Manifest.Bucket = cfg.Manifest.Bucket
	}
}

// ValidationError collects multiple config validation failures.
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("configuration validation failed:")
	for _, err := range e.Errors {
		b.WriteString("\n  - ")
		b.WriteString(err)
	}
	return b.String()
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
