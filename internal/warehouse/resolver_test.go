package warehouse

import (
	"strings"
	"testing"

	"github.com/umccr/data-releaser/internal/model"
)

func TestDerivePrefix(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "typical bam path",
			key:  "data/output/sample.bam",
			want: "data/output/",
		},
		{
			name: "deeply nested path",
			key:  "umccr-research/wgs/L2400001/run-001/out/sample.bam",
			want: "umccr-research/wgs/L2400001/run-001/out/",
		},
		{
			name: "single directory",
			key:  "output/sample.bam",
			want: "output/",
		},
		{
			name: "root level object (no slash)",
			key:  "sample.bam",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DerivePrefix(tt.key)
			if got != tt.want {
				t.Errorf("DerivePrefix(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestIsArchivedStorageClass(t *testing.T) {
	tests := []struct {
		name         string
		storageClass string
		want         bool
	}{
		{name: "DEEP_ARCHIVE", storageClass: "DEEP_ARCHIVE", want: true},
		{name: "deep_archive lowercase", storageClass: "deep_archive", want: true},
		{name: "DeepArchive mixed case", storageClass: "DeepArchive", want: true},
		{name: "GLACIER", storageClass: "GLACIER", want: true},
		{name: "glacier lowercase", storageClass: "glacier", want: true},
		{name: "GLACIER_IR", storageClass: "GLACIER_IR", want: true},
		{name: "glacier_ir lowercase", storageClass: "glacier_ir", want: true},
		{name: "Glacier_IR mixed case", storageClass: "Glacier_IR", want: true},
		{name: "STANDARD", storageClass: "STANDARD", want: false},
		{name: "STANDARD_IA", storageClass: "STANDARD_IA", want: false},
		{name: "INTELLIGENT_TIERING", storageClass: "INTELLIGENT_TIERING", want: false},
		{name: "ONEZONE_IA", storageClass: "ONEZONE_IA", want: false},
		{name: "empty string", storageClass: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsArchivedStorageClass(tt.storageClass)
			if got != tt.want {
				t.Errorf("IsArchivedStorageClass(%q) = %v, want %v", tt.storageClass, got, tt.want)
			}
		})
	}
}

func TestValidateResolution_AllResolved(t *testing.T) {
	requested := []string{"L2400001", "L2400002"}
	resolved := []model.ResolvedLibrary{
		{LibraryID: "L2400001", Key: "data/out/sample1.bam", StorageClass: "STANDARD"},
		{LibraryID: "L2400002", Key: "data/out/sample2.bam", StorageClass: "STANDARD"},
	}

	err := ValidateResolution(requested, resolved)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidateResolution_NoShareableReads(t *testing.T) {
	requested := []string{"L2400001", "L2400002", "L2400003"}
	resolved := []model.ResolvedLibrary{
		{LibraryID: "L2400001", Key: "data/out/sample1.bam", StorageClass: "STANDARD"},
	}

	err := ValidateResolution(requested, resolved)
	if err == nil {
		t.Fatal("expected error for missing libraries")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "L2400002") {
		t.Errorf("error should mention L2400002, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "L2400003") {
		t.Errorf("error should mention L2400003, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "may not exist") {
		t.Errorf("error should contain the expected message, got: %s", errMsg)
	}
}

func TestValidateResolution_ArchivedStorage(t *testing.T) {
	requested := []string{"L2400001", "L2400002"}
	resolved := []model.ResolvedLibrary{
		{LibraryID: "L2400001", Key: "data/out/sample1.bam", StorageClass: "STANDARD"},
		{LibraryID: "L2400002", Key: "data/out/sample2.bam", StorageClass: "DEEP_ARCHIVE"},
	}

	err := ValidateResolution(requested, resolved)
	if err == nil {
		t.Fatal("expected error for archived library")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "L2400002") {
		t.Errorf("error should mention L2400002, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "archived storage class") {
		t.Errorf("error should mention archived storage class, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "DEEP_ARCHIVE") {
		t.Errorf("error should mention the storage class name, got: %s", errMsg)
	}
}

func TestValidateResolution_BothErrors(t *testing.T) {
	requested := []string{"L2400001", "L2400002", "L2400003"}
	resolved := []model.ResolvedLibrary{
		{LibraryID: "L2400001", Key: "data/out/sample1.bam", StorageClass: "GLACIER"},
	}

	err := ValidateResolution(requested, resolved)
	if err == nil {
		t.Fatal("expected error")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "L2400002") {
		t.Errorf("error should mention L2400002 (no shareable reads), got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "L2400003") {
		t.Errorf("error should mention L2400003 (no shareable reads), got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "L2400001") {
		t.Errorf("error should mention L2400001 (archived), got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "GLACIER") {
		t.Errorf("error should mention GLACIER storage class, got: %s", errMsg)
	}
}

func TestValidateResolution_MultipleFilesPerLibrary(t *testing.T) {
	requested := []string{"L2400001"}
	resolved := []model.ResolvedLibrary{
		{LibraryID: "L2400001", Key: "data/out/sample1.bam", StorageClass: "STANDARD"},
		{LibraryID: "L2400001", Key: "data/out/sample2.bam", StorageClass: "DEEP_ARCHIVE"},
	}

	err := ValidateResolution(requested, resolved)
	if err == nil {
		t.Fatal("expected error for partially archived library")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "L2400001") {
		t.Errorf("error should mention L2400001, got: %s", errMsg)
	}
}

func TestValidateResolution_EmptyResolved(t *testing.T) {
	requested := []string{"L2400001"}
	var resolved []model.ResolvedLibrary

	err := ValidateResolution(requested, resolved)
	if err == nil {
		t.Fatal("expected error for empty results")
	}
	if !strings.Contains(err.Error(), "L2400001") {
		t.Errorf("error should mention L2400001, got: %s", err.Error())
	}
}

func TestExtractPrefixes(t *testing.T) {
	tests := []struct {
		name     string
		resolved []model.ResolvedLibrary
		want     []string
	}{
		{
			name: "single file",
			resolved: []model.ResolvedLibrary{
				{Key: "data/output/sample.bam"},
			},
			want: []string{"data/output/"},
		},
		{
			name: "multiple files same prefix",
			resolved: []model.ResolvedLibrary{
				{Key: "data/output/sample1.bam"},
				{Key: "data/output/sample2.bam"},
			},
			want: []string{"data/output/"},
		},
		{
			name: "multiple files different prefixes",
			resolved: []model.ResolvedLibrary{
				{Key: "data/output1/sample1.bam"},
				{Key: "data/output2/sample2.bam"},
			},
			want: []string{"data/output1/", "data/output2/"},
		},
		{
			name: "deduplication",
			resolved: []model.ResolvedLibrary{
				{Key: "data/output/sample1.bam"},
				{Key: "data/output/sample2.bam"},
				{Key: "data/other/sample3.bam"},
				{Key: "data/output/sample4.bam"},
			},
			want: []string{"data/output/", "data/other/"},
		},
		{
			name:     "empty input",
			resolved: []model.ResolvedLibrary{},
			want:     nil,
		},
		{
			name: "root level object excluded",
			resolved: []model.ResolvedLibrary{
				{Key: "sample.bam"},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrefixes(tt.resolved)
			if len(got) != len(tt.want) {
				t.Fatalf("ExtractPrefixes() returned %d prefixes, want %d; got: %v, want: %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ExtractPrefixes()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
