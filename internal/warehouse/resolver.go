package warehouse

import (
	"fmt"
	"path"
	"strings"

	"github.com/umccr/data-releaser/internal/model"
)

// archivedStorageClasses lists S3 storage classes that are considered archived
// (not streamable by htsget-rs without a restore). Includes both canonical
// underscore forms and potential no-underscore variants for robustness.
var archivedStorageClasses = map[string]bool{
	"deep_archive": true,
	"deeparchive":  true,
	"glacier":      true,
	"glacier_ir":   true,
	"glacierir":    true,
}

// DerivePrefix returns the parent prefix of an S3 key (everything up to and including
// the last '/'). For example, "data/output/sample.bam" → "data/output/".
// This covers co-located index files (.bai) under the same prefix.
//
// If the key has no '/' (a root-level object), returns an empty string.
func DerivePrefix(key string) string {
	dir := path.Dir(key)
	if dir == "." {
		return ""
	}
	return dir + "/"
}

// IsArchivedStorageClass returns true if the given storage class is an archived tier
// (DeepArchive, Glacier, Glacier_IR). The check is case-insensitive.
func IsArchivedStorageClass(storageClass string) bool {
	return archivedStorageClasses[strings.ToLower(storageClass)]
}

// ValidateResolution checks the resolution results for problems:
//   - Library IDs that were requested but have no results (no shareable reads)
//   - Library IDs with archived storage classes (not streamable by htsget-rs)
//
// It collects ALL errors before returning, so the operator sees every problematic
// library ID at once rather than fixing them one by one.
//
// Returns nil if all requested library IDs have non-archived results.
//
// Requirements: 1.1, 1.4, 1.6, 1.7
func ValidateResolution(requestedLibraryIDs []string, resolved []model.ResolvedLibrary) error {
	// Build a set of library IDs that have at least one result
	resolvedSet := make(map[string]bool)
	for _, r := range resolved {
		resolvedSet[r.LibraryID] = true
	}

	// Detect library IDs with no results (no shareable reads)
	var noShareableReads []string
	for _, id := range requestedLibraryIDs {
		if !resolvedSet[id] {
			noShareableReads = append(noShareableReads, id)
		}
	}

	// Detect library IDs with archived storage classes
	type archivedEntry struct {
		libraryID    string
		storageClass string
	}
	var archivedLibraries []archivedEntry
	seenArchived := make(map[string]bool)
	for _, r := range resolved {
		if IsArchivedStorageClass(r.StorageClass) && !seenArchived[r.LibraryID] {
			archivedLibraries = append(archivedLibraries, archivedEntry{
				libraryID:    r.LibraryID,
				storageClass: r.StorageClass,
			})
			seenArchived[r.LibraryID] = true
		}
	}

	// Build combined error message if any problems were found
	if len(noShareableReads) == 0 && len(archivedLibraries) == 0 {
		return nil
	}

	var msgs []string
	for _, id := range noShareableReads {
		msgs = append(msgs, fmt.Sprintf(
			"library ID %s may not exist, may not be aligned yet, or may have reads that are archived",
			id,
		))
	}
	for _, entry := range archivedLibraries {
		msgs = append(msgs, fmt.Sprintf(
			"library ID %s has archived storage class %s and cannot be streamed by htsget-rs",
			entry.libraryID, entry.storageClass,
		))
	}

	return fmt.Errorf("cannot share libraries:\n  %s", strings.Join(msgs, "\n  "))
}

// ExtractPrefixes derives unique S3 prefixes from the resolved library results.
// Each prefix is the parent prefix of the resolved .bam key, deduplicated.
// The order of the returned prefixes is deterministic (first occurrence order).
func ExtractPrefixes(resolved []model.ResolvedLibrary) []string {
	seen := make(map[string]bool)
	var prefixes []string
	for _, r := range resolved {
		prefix := DerivePrefix(r.Key)
		if prefix != "" && !seen[prefix] {
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}
