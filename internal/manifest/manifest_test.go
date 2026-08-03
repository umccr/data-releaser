package manifest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/umccr/data-releaser/internal/model"
)

func TestGenerate_NoShares_ReturnsNil(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()

	result := g.Generate(nil, now)
	if result != nil {
		t.Errorf("expected nil for empty shares, got %+v", result)
	}

	result = g.Generate([]model.ShareRecord{}, now)
	if result != nil {
		t.Errorf("expected nil for empty slice, got %+v", result)
	}
}

func TestGenerate_AllExpiredOrRevoked_ReturnsNil(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	revokedAt := now.Add(-30 * time.Minute)

	shares := []model.ShareRecord{
		{
			ShareID:             "expired-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			ExpiresAt:           &past,
		},
		{
			ShareID:             "revoked-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L002"},
			RevokedAt:           &revokedAt,
		},
	}

	result := g.Generate(shares, now)
	if result != nil {
		t.Errorf("expected nil when all shares are expired/revoked, got %+v", result)
	}
}

func TestGenerate_SingleActiveShare(t *testing.T) {
	g := NewGenerator("https://htsget-lattice.data-control.umccr.org")
	now := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)

	shares := []model.ShareRecord{
		{
			ShareID:             "share-1",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L2400001"},
			S3Prefixes:          []string{"s3://bucket/prefix/"},
			Files: []model.ShareFile{
				{LibraryID: "L2400001", HtsgetID: "demo/L2400001/NA12878", Format: "bam"},
			},
			CreatedAt: now.Add(-time.Hour),
			ExpiresAt: &expiry,
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	if result.HtsgetBaseUrl != "https://htsget-lattice.data-control.umccr.org" {
		t.Errorf("HtsgetBaseUrl: expected %q, got %q",
			"https://htsget-lattice.data-control.umccr.org", result.HtsgetBaseUrl)
	}
	if !result.GeneratedAt.Equal(now) {
		t.Errorf("GeneratedAt: expected %v, got %v", now, result.GeneratedAt)
	}
	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(result.Libraries))
	}

	lib := result.Libraries[0]
	if lib.LibraryID != "L2400001" {
		t.Errorf("LibraryID: expected %q, got %q", "L2400001", lib.LibraryID)
	}
	if lib.ExpiresAt == nil || !lib.ExpiresAt.Equal(expiry) {
		t.Errorf("ExpiresAt: expected %v, got %v", expiry, lib.ExpiresAt)
	}
	if len(lib.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(lib.Files))
	}
	if lib.Files[0].Request != "reads/demo/L2400001/NA12878" {
		t.Errorf("Request: expected %q, got %q", "reads/demo/L2400001/NA12878", lib.Files[0].Request)
	}
	if lib.Files[0].Rules != nil {
		t.Errorf("expected nil rules for unrestricted access, got %v", lib.Files[0].Rules)
	}
}

func TestGenerate_UnlimitedShare(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()

	shares := []model.ShareRecord{
		{
			ShareID:             "unlimited-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			S3Prefixes:          []string{"s3://bucket/prefix/"},
			CreatedAt:           now.Add(-time.Hour),
			ExpiresAt:           nil, // unlimited
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(result.Libraries))
	}
	if result.Libraries[0].ExpiresAt != nil {
		t.Errorf("expected nil ExpiresAt for unlimited share, got %v", result.Libraries[0].ExpiresAt)
	}
}

func TestGenerate_MultipleLibrariesInOneShare(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()
	expiry := now.Add(24 * time.Hour)

	shares := []model.ShareRecord{
		{
			ShareID:             "multi-lib-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001", "L002", "L003"},
			S3Prefixes:          []string{"s3://bucket/p1/", "s3://bucket/p2/", "s3://bucket/p3/"},
			CreatedAt:           now.Add(-time.Hour),
			ExpiresAt:           &expiry,
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	if len(result.Libraries) != 3 {
		t.Fatalf("expected 3 libraries, got %d", len(result.Libraries))
	}

	// Libraries should be sorted by ID
	if result.Libraries[0].LibraryID != "L001" {
		t.Errorf("first library: expected L001, got %q", result.Libraries[0].LibraryID)
	}
	if result.Libraries[1].LibraryID != "L002" {
		t.Errorf("second library: expected L002, got %q", result.Libraries[1].LibraryID)
	}
	if result.Libraries[2].LibraryID != "L003" {
		t.Errorf("third library: expected L003, got %q", result.Libraries[2].LibraryID)
	}
}

func TestGenerate_EffectiveExpiry_LatestWins(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()
	earlyExpiry := now.Add(24 * time.Hour)
	lateExpiry := now.Add(72 * time.Hour)

	// Two shares grant the same library with different expiries
	shares := []model.ShareRecord{
		{
			ShareID:             "share-1",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			CreatedAt:           now.Add(-2 * time.Hour),
			ExpiresAt:           &earlyExpiry,
		},
		{
			ShareID:             "share-2",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			CreatedAt:           now.Add(-time.Hour),
			ExpiresAt:           &lateExpiry,
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library (merged), got %d", len(result.Libraries))
	}

	lib := result.Libraries[0]
	if lib.ExpiresAt == nil {
		t.Fatal("expected non-nil ExpiresAt")
	}
	if !lib.ExpiresAt.Equal(lateExpiry) {
		t.Errorf("expected latest expiry %v, got %v", lateExpiry, *lib.ExpiresAt)
	}
}

func TestGenerate_EffectiveExpiry_UnlimitedOverridesAll(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()
	expiry := now.Add(24 * time.Hour)

	// One share has expiry, another is unlimited — library should be nil (unlimited)
	shares := []model.ShareRecord{
		{
			ShareID:             "share-limited",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			CreatedAt:           now.Add(-2 * time.Hour),
			ExpiresAt:           &expiry,
		},
		{
			ShareID:             "share-unlimited",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			CreatedAt:           now.Add(-time.Hour),
			ExpiresAt:           nil, // unlimited
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(result.Libraries))
	}
	if result.Libraries[0].ExpiresAt != nil {
		t.Errorf("expected nil ExpiresAt when any share is unlimited, got %v", result.Libraries[0].ExpiresAt)
	}
}

func TestGenerate_MixedActiveAndInactive(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(24 * time.Hour)
	revokedAt := now.Add(-30 * time.Minute)

	shares := []model.ShareRecord{
		{
			ShareID:             "active-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			CreatedAt:           now.Add(-2 * time.Hour),
			ExpiresAt:           &future,
		},
		{
			ShareID:             "expired-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L002"},
			S3Prefixes:          []string{"s3://bucket/p2/"},
			CreatedAt:           now.Add(-48 * time.Hour),
			ExpiresAt:           &past,
		},
		{
			ShareID:             "revoked-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L003"},
			S3Prefixes:          []string{"s3://bucket/p3/"},
			CreatedAt:           now.Add(-3 * time.Hour),
			RevokedAt:           &revokedAt,
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	// Only L001 should be present (L002 expired, L003 revoked)
	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library (only active), got %d", len(result.Libraries))
	}
	if result.Libraries[0].LibraryID != "L001" {
		t.Errorf("expected L001, got %q", result.Libraries[0].LibraryID)
	}
}

func TestGenerate_MultipleSharesMultipleLibraries(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()
	expiry1 := now.Add(24 * time.Hour)
	expiry2 := now.Add(48 * time.Hour)

	// Share 1 grants L001 and L002; Share 2 grants L002 and L003
	shares := []model.ShareRecord{
		{
			ShareID:             "share-1",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L001", "L002"},
			S3Prefixes:          []string{"s3://bucket/p1/", "s3://bucket/p2/"},
			CreatedAt:           now.Add(-2 * time.Hour),
			ExpiresAt:           &expiry1,
		},
		{
			ShareID:             "share-2",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L002", "L003"},
			S3Prefixes:          []string{"s3://bucket/p2/", "s3://bucket/p3/"},
			CreatedAt:           now.Add(-time.Hour),
			ExpiresAt:           &expiry2,
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	if len(result.Libraries) != 3 {
		t.Fatalf("expected 3 libraries, got %d", len(result.Libraries))
	}

	// Check L001: only share-1 grants it → expiry1
	if result.Libraries[0].LibraryID != "L001" {
		t.Errorf("first library: expected L001, got %q", result.Libraries[0].LibraryID)
	}
	if result.Libraries[0].ExpiresAt == nil || !result.Libraries[0].ExpiresAt.Equal(expiry1) {
		t.Errorf("L001 expiry: expected %v, got %v", expiry1, result.Libraries[0].ExpiresAt)
	}

	// Check L002: both shares grant it → latest expiry (expiry2)
	if result.Libraries[1].LibraryID != "L002" {
		t.Errorf("second library: expected L002, got %q", result.Libraries[1].LibraryID)
	}
	if result.Libraries[1].ExpiresAt == nil || !result.Libraries[1].ExpiresAt.Equal(expiry2) {
		t.Errorf("L002 expiry: expected %v (latest), got %v", expiry2, result.Libraries[1].ExpiresAt)
	}

	// Check L003: only share-2 grants it → expiry2
	if result.Libraries[2].LibraryID != "L003" {
		t.Errorf("third library: expected L003, got %q", result.Libraries[2].LibraryID)
	}
	if result.Libraries[2].ExpiresAt == nil || !result.Libraries[2].ExpiresAt.Equal(expiry2) {
		t.Errorf("L003 expiry: expected %v, got %v", expiry2, result.Libraries[2].ExpiresAt)
	}
}

func TestGenerate_JSONOutputConformsToSchema(t *testing.T) {
	g := NewGenerator("https://htsget-lattice.data-control.umccr.org")
	now := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)

	shares := []model.ShareRecord{
		{
			ShareID:             "share-1",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L2400001"},
			S3Prefixes:          []string{"s3://bucket/prefix/"},
			Files: []model.ShareFile{
				{LibraryID: "L2400001", HtsgetID: "demo/L2400001/NA12878", Format: "bam"},
			},
			CreatedAt: now.Add(-time.Hour),
			ExpiresAt: &expiry,
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal manifest: %v", err)
	}

	// Verify we can unmarshal back
	var decoded model.Manifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal manifest: %v", err)
	}

	if decoded.HtsgetBaseUrl != "https://htsget-lattice.data-control.umccr.org" {
		t.Errorf("decoded HtsgetBaseUrl mismatch")
	}
	if !decoded.GeneratedAt.Equal(now) {
		t.Errorf("decoded GeneratedAt mismatch")
	}
	if len(decoded.Libraries) != 1 {
		t.Fatalf("decoded libraries count mismatch")
	}
	if decoded.Libraries[0].LibraryID != "L2400001" {
		t.Errorf("decoded LibraryID mismatch")
	}
	if len(decoded.Libraries[0].Files) != 1 {
		t.Fatalf("decoded files count mismatch: got %d", len(decoded.Libraries[0].Files))
	}
	if decoded.Libraries[0].Files[0].Request != "reads/demo/L2400001/NA12878" {
		t.Errorf("decoded request path mismatch: got %q", decoded.Libraries[0].Files[0].Request)
	}
}

func TestGenerate_HtsgetRequestPaths(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()

	shares := []model.ShareRecord{
		{
			ShareID:             "share-1",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L2400001", "L2400002"},
			S3Prefixes:          []string{"s3://bucket/p1/", "s3://bucket/p2/"},
			Files: []model.ShareFile{
				{LibraryID: "L2400001", HtsgetID: "p1/NA12878", Format: "bam"},
				{LibraryID: "L2400002", HtsgetID: "p2/NA12879", Format: "bam"},
			},
			CreatedAt: now.Add(-time.Hour),
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}

	// The request path is reads/<htsgetId> — the object key minus bucket and format
	// extension. Each library's files must come from that library's records only.
	want := map[string]string{
		"L2400001": "reads/p1/NA12878",
		"L2400002": "reads/p2/NA12879",
	}
	for _, lib := range result.Libraries {
		if len(lib.Files) != 1 {
			t.Fatalf("expected 1 file per library, got %d for %s", len(lib.Files), lib.LibraryID)
		}
		if lib.Files[0].Request != want[lib.LibraryID] {
			t.Errorf("library %s: expected request %q, got %q", lib.LibraryID, want[lib.LibraryID], lib.Files[0].Request)
		}
	}
}

// A share written before per-file recording existed carries no Files. The library must
// still appear (its prefixes are authorized) but with an empty file list rather than a
// fabricated reads/<library_id> path that htsget-rs would 404 on.
func TestGenerate_ShareWithoutRecordedFiles(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()

	shares := []model.ShareRecord{
		{
			ShareID:             "legacy-share",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L2400001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			CreatedAt:           now.Add(-time.Hour),
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(result.Libraries))
	}
	if len(result.Libraries[0].Files) != 0 {
		t.Errorf("expected no files for a share with no recorded files, got %v", result.Libraries[0].Files)
	}
}

// Several shares granting the same library must not produce duplicate request paths.
func TestGenerate_DeduplicatesFilesAcrossShares(t *testing.T) {
	g := NewGenerator("https://htsget.example.com")
	now := time.Now().UTC()

	file := model.ShareFile{LibraryID: "L2400001", HtsgetID: "p1/NA12878", Format: "bam"}
	shares := []model.ShareRecord{
		{
			ShareID:             "share-1",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L2400001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			Files:               []model.ShareFile{file},
			CreatedAt:           now.Add(-2 * time.Hour),
		},
		{
			ShareID:             "share-2",
			ResearcherAccountID: "123456789",
			LibraryIDs:          []string{"L2400001"},
			S3Prefixes:          []string{"s3://bucket/p1/"},
			Files: []model.ShareFile{
				file,
				{LibraryID: "L2400001", HtsgetID: "p1/NA12879", Format: "bam"},
			},
			CreatedAt: now.Add(-time.Hour),
		},
	}

	result := g.Generate(shares, now)
	if result == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(result.Libraries) != 1 {
		t.Fatalf("expected 1 library, got %d", len(result.Libraries))
	}

	files := result.Libraries[0].Files
	if len(files) != 2 {
		t.Fatalf("expected 2 deduplicated files, got %d (%v)", len(files), files)
	}
	// Sorted by request path for a stable manifest.
	if files[0].Request != "reads/p1/NA12878" || files[1].Request != "reads/p1/NA12879" {
		t.Errorf("unexpected file order/content: %v", files)
	}
}

func TestComputeEffectiveExpiry(t *testing.T) {
	now := time.Now().UTC()
	early := now.Add(24 * time.Hour)
	late := now.Add(72 * time.Hour)

	tests := []struct {
		name     string
		shares   []*model.ShareRecord
		wantNil  bool
		wantTime *time.Time
	}{
		{
			name: "single share with expiry",
			shares: []*model.ShareRecord{
				{ExpiresAt: &early},
			},
			wantTime: &early,
		},
		{
			name: "single unlimited share",
			shares: []*model.ShareRecord{
				{ExpiresAt: nil},
			},
			wantNil: true,
		},
		{
			name: "two shares latest wins",
			shares: []*model.ShareRecord{
				{ExpiresAt: &early},
				{ExpiresAt: &late},
			},
			wantTime: &late,
		},
		{
			name: "one limited one unlimited → unlimited",
			shares: []*model.ShareRecord{
				{ExpiresAt: &early},
				{ExpiresAt: nil},
			},
			wantNil: true,
		},
		{
			name: "multiple all unlimited",
			shares: []*model.ShareRecord{
				{ExpiresAt: nil},
				{ExpiresAt: nil},
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeEffectiveExpiry(tt.shares)
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %v", *result)
				}
			} else {
				if result == nil {
					t.Fatal("expected non-nil expiry")
				}
				if !result.Equal(*tt.wantTime) {
					t.Errorf("expected %v, got %v", *tt.wantTime, *result)
				}
			}
		})
	}
}
