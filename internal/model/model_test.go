package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEffectiveStatus_Active(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(24 * time.Hour)

	tests := []struct {
		name   string
		record ShareRecord
	}{
		{
			name: "no expiry no revocation (unlimited)",
			record: ShareRecord{
				CreatedAt: now.Add(-time.Hour),
			},
		},
		{
			name: "future expiry no revocation",
			record: ShareRecord{
				CreatedAt: now.Add(-time.Hour),
				ExpiresAt: &future,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.record.EffectiveStatus(now)
			if status != ShareStatusActive {
				t.Errorf("expected %q, got %q", ShareStatusActive, status)
			}
		})
	}
}

func TestEffectiveStatus_Expired(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	exactNow := now

	tests := []struct {
		name      string
		expiresAt time.Time
	}{
		{
			name:      "expired in the past",
			expiresAt: past,
		},
		{
			name:      "expired exactly at now",
			expiresAt: exactNow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expiresAt := tt.expiresAt
			record := ShareRecord{
				CreatedAt: now.Add(-24 * time.Hour),
				ExpiresAt: &expiresAt,
			}
			status := record.EffectiveStatus(now)
			if status != ShareStatusExpired {
				t.Errorf("expected %q, got %q", ShareStatusExpired, status)
			}
		})
	}
}

func TestEffectiveStatus_Revoked(t *testing.T) {
	now := time.Now().UTC()
	revokedAt := now.Add(-30 * time.Minute)
	future := now.Add(24 * time.Hour)

	tests := []struct {
		name   string
		record ShareRecord
	}{
		{
			name: "revoked with no expiry",
			record: ShareRecord{
				CreatedAt: now.Add(-time.Hour),
				RevokedAt: &revokedAt,
			},
		},
		{
			name: "revoked with future expiry (revoked takes precedence)",
			record: ShareRecord{
				CreatedAt: now.Add(-time.Hour),
				ExpiresAt: &future,
				RevokedAt: &revokedAt,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.record.EffectiveStatus(now)
			if status != ShareStatusRevoked {
				t.Errorf("expected %q, got %q", ShareStatusRevoked, status)
			}
		})
	}
}

func TestEffectiveStatus_RevokedTakesPrecedenceOverExpired(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	revokedAt := now.Add(-30 * time.Minute)

	// Both expired and revoked: revoked should take precedence
	record := ShareRecord{
		CreatedAt: now.Add(-24 * time.Hour),
		ExpiresAt: &past,
		RevokedAt: &revokedAt,
	}

	status := record.EffectiveStatus(now)
	if status != ShareStatusRevoked {
		t.Errorf("expected %q (revoked takes precedence over expired), got %q", ShareStatusRevoked, status)
	}
}

func TestAccessRule_JSONSerialization(t *testing.T) {
	refName := "chr1"
	format := "BAM"
	start := int64(1000)
	end := int64(2000)

	rule := AccessRule{
		ReferenceName: &refName,
		Format:        &format,
		Start:         &start,
		End:           &end,
	}

	data, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("failed to marshal AccessRule: %v", err)
	}

	var decoded AccessRule
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal AccessRule: %v", err)
	}

	if *decoded.ReferenceName != refName {
		t.Errorf("ReferenceName: expected %q, got %q", refName, *decoded.ReferenceName)
	}
	if *decoded.Format != format {
		t.Errorf("Format: expected %q, got %q", format, *decoded.Format)
	}
	if *decoded.Start != start {
		t.Errorf("Start: expected %d, got %d", start, *decoded.Start)
	}
	if *decoded.End != end {
		t.Errorf("End: expected %d, got %d", end, *decoded.End)
	}
}

func TestAccessRule_OmitsNilFields(t *testing.T) {
	rule := AccessRule{}

	data, err := json.Marshal(rule)
	if err != nil {
		t.Fatalf("failed to marshal empty AccessRule: %v", err)
	}

	expected := "{}"
	if string(data) != expected {
		t.Errorf("expected %q, got %q", expected, string(data))
	}
}

func TestManifest_JSONSerialization(t *testing.T) {
	now := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)

	manifest := Manifest{
		HtsgetBaseUrl: "https://htsget-lattice.data-control.umccr.org",
		GeneratedAt:   now,
		Libraries: []ManifestLibrary{
			{
				LibraryID: "L2400001",
				ExpiresAt: &expiry,
				Files: []ManifestFile{
					{Request: "reads/some-id"},
					{
						Request: "reads/another-id",
						Rules: []AccessRule{
							{ReferenceName: strPtr("chr1")},
						},
					},
				},
			},
		},
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("failed to marshal Manifest: %v", err)
	}

	var decoded Manifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal Manifest: %v", err)
	}

	if decoded.HtsgetBaseUrl != manifest.HtsgetBaseUrl {
		t.Errorf("HtsgetBaseUrl: expected %q, got %q", manifest.HtsgetBaseUrl, decoded.HtsgetBaseUrl)
	}
	if len(decoded.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(decoded.Libraries))
	}
	if decoded.Libraries[0].LibraryID != "L2400001" {
		t.Errorf("LibraryID: expected %q, got %q", "L2400001", decoded.Libraries[0].LibraryID)
	}
	if len(decoded.Libraries[0].Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(decoded.Libraries[0].Files))
	}
	if decoded.Libraries[0].Files[1].Rules == nil || len(decoded.Libraries[0].Files[1].Rules) != 1 {
		t.Fatalf("expected 1 rule on second file")
	}
}

func strPtr(s string) *string {
	return &s
}
