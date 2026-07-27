package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_ValidConfig(t *testing.T) {
	content := `
profile: "data-control"
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
  catalog: "AwsDataCatalog"
endpoints:
  authorization: "https://auth.data-control.umccr.org"
  htsgetBaseUrl: "https://htsget-lattice.data-control.umccr.org"
cache:
  ttlCeiling: "2h"
  denyTtl: "30s"
validation:
  canary:
    libraryId: "L2400001"
    account: "534840902377"
`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Profile != "data-control" {
		t.Errorf("expected profile 'data-control', got %q", cfg.Profile)
	}
	if cfg.Warehouse.AthenaWorkgroup != "primary" {
		t.Errorf("expected athenaWorkgroup 'primary', got %q", cfg.Warehouse.AthenaWorkgroup)
	}
	if cfg.Warehouse.Database != "mart" {
		t.Errorf("expected database 'mart', got %q", cfg.Warehouse.Database)
	}
	if cfg.Warehouse.Catalog != "AwsDataCatalog" {
		t.Errorf("expected catalog 'AwsDataCatalog', got %q", cfg.Warehouse.Catalog)
	}
	if cfg.Endpoints.Authorization != "https://auth.data-control.umccr.org" {
		t.Errorf("expected authorization endpoint, got %q", cfg.Endpoints.Authorization)
	}
	if cfg.Endpoints.HtsgetBaseURL != "https://htsget-lattice.data-control.umccr.org" {
		t.Errorf("expected htsgetBaseUrl endpoint, got %q", cfg.Endpoints.HtsgetBaseURL)
	}
	if cfg.Cache.TTLCeiling.Duration != 2*time.Hour {
		t.Errorf("expected ttlCeiling 2h, got %v", cfg.Cache.TTLCeiling.Duration)
	}
	if cfg.Cache.DenyTTL.Duration != 30*time.Second {
		t.Errorf("expected denyTtl 30s, got %v", cfg.Cache.DenyTTL.Duration)
	}
	if cfg.Validation.Canary == nil {
		t.Fatal("expected canary config to be non-nil")
	}
	if cfg.Validation.Canary.LibraryID != "L2400001" {
		t.Errorf("expected canary libraryId 'L2400001', got %q", cfg.Validation.Canary.LibraryID)
	}
	if cfg.Validation.Canary.Account != "534840902377" {
		t.Errorf("expected canary account '534840902377', got %q", cfg.Validation.Canary.Account)
	}
}

func TestLoad_DefaultValues(t *testing.T) {
	content := `
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Cache.TTLCeiling.Duration != DefaultCacheTTLCeiling {
		t.Errorf("expected default ttlCeiling %v, got %v", DefaultCacheTTLCeiling, cfg.Cache.TTLCeiling.Duration)
	}
	if cfg.Cache.DenyTTL.Duration != DefaultCacheDenyTTL {
		t.Errorf("expected default denyTtl %v, got %v", DefaultCacheDenyTTL, cfg.Cache.DenyTTL.Duration)
	}
	if cfg.Warehouse.Catalog != "AwsDataCatalog" {
		t.Errorf("expected default catalog 'AwsDataCatalog', got %q", cfg.Warehouse.Catalog)
	}
}

func TestLoad_MissingRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "missing athenaWorkgroup",
			content: `
warehouse:
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
`,
			wantErr: "warehouse.athenaWorkgroup is required",
		},
		{
			name: "missing database",
			content: `
warehouse:
  athenaWorkgroup: "primary"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
`,
			wantErr: "warehouse.database is required",
		},
		{
			name: "missing authorization endpoint",
			content: `
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  htsgetBaseUrl: "https://htsget.example.com"
`,
			wantErr: "endpoints.authorization is required",
		},
		{
			name: "missing htsgetBaseUrl",
			content: `
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
`,
			wantErr: "endpoints.htsgetBaseUrl is required",
		},
		{
			name: "all required fields missing",
			content: `
profile: "test"
`,
			wantErr: "configuration validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempConfig(t, tt.content)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !containsString(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	content := `
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
cache:
  ttlCeiling: "not-a-duration"
`
	path := writeTempConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
	if !containsString(err.Error(), "invalid duration") {
		t.Errorf("expected 'invalid duration' in error, got %q", err.Error())
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !containsString(err.Error(), "config file not found") {
		t.Errorf("expected 'config file not found' in error, got %q", err.Error())
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	content := `
warehouse: [[[invalid yaml
`
	path := writeTempConfig(t, content)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !containsString(err.Error(), "parsing config file") {
		t.Errorf("expected 'parsing config file' in error, got %q", err.Error())
	}
}

func TestApplyFlagOverrides(t *testing.T) {
	cfg := &Config{
		Profile: "original",
	}

	cfg.ApplyFlagOverrides(FlagOverrides{Profile: "override"})
	if cfg.Profile != "override" {
		t.Errorf("expected profile 'override', got %q", cfg.Profile)
	}

	// Empty override should not change existing value
	cfg.ApplyFlagOverrides(FlagOverrides{Profile: ""})
	if cfg.Profile != "override" {
		t.Errorf("expected profile to remain 'override', got %q", cfg.Profile)
	}
}

func TestLoad_OptionalProfileOmitted(t *testing.T) {
	content := `
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Profile != "" {
		t.Errorf("expected empty profile when omitted, got %q", cfg.Profile)
	}
}

func TestLoad_OptionalCanaryOmitted(t *testing.T) {
	content := `
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
`
	path := writeTempConfig(t, content)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Validation.Canary != nil {
		t.Errorf("expected nil canary when omitted, got %+v", cfg.Validation.Canary)
	}
}

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"1h", 1 * time.Hour},
		{"60s", 60 * time.Second},
		{"30m", 30 * time.Minute},
		{"24h", 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			content := fmt.Sprintf(`
warehouse:
  athenaWorkgroup: "primary"
  database: "mart"
endpoints:
  authorization: "https://auth.example.com"
  htsgetBaseUrl: "https://htsget.example.com"
cache:
  ttlCeiling: "%s"
`, tt.input)
			path := writeTempConfig(t, content)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Cache.TTLCeiling.Duration != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, cfg.Cache.TTLCeiling.Duration)
			}
		})
	}
}

// Helpers

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s, substr))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
