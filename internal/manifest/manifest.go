// Package manifest implements manifest generation and S3 delivery for researcher accounts.
// The manifest is a JSON document listing the libraries and files a researcher can access.
package manifest

import (
	"cmp"
	"slices"
	"time"

	"github.com/umccr/data-releaser/internal/model"
)

// Generator builds Manifest documents from active ShareRecords for a researcher account.
type Generator struct {
	// HtsgetBaseUrl is the htsget-rs base URL included in generated manifests.
	HtsgetBaseUrl string
}

// NewGenerator creates a Generator with the given htsget base URL.
func NewGenerator(htsgetBaseUrl string) *Generator {
	return &Generator{
		HtsgetBaseUrl: htsgetBaseUrl,
	}
}

// libraryGrant tracks the shares that contribute to a library's access.
type libraryGrant struct {
	// libraryID is the library identifier.
	libraryID string

	// shares are the active ShareRecords granting this library.
	shares []*model.ShareRecord
}

// Generate builds a Manifest from the given active ShareRecords for a researcher account.
// It groups files by library, computes effective expiry per library (latest across shares,
// null if any share is unlimited), and constructs htsget request paths.
//
// Returns nil if there are no active shares (the manifest should be deleted).
func (g *Generator) Generate(shares []model.ShareRecord, now time.Time) *model.Manifest {
	if len(shares) == 0 {
		return nil
	}

	// Filter to only active shares
	active := filterActive(shares, now)
	if len(active) == 0 {
		return nil
	}

	// Group libraries across all active shares
	libraryGrants := groupByLibrary(active)

	// Build manifest libraries
	libraries := make([]model.ManifestLibrary, 0, len(libraryGrants))
	for _, grant := range libraryGrants {
		lib := buildManifestLibrary(grant)
		libraries = append(libraries, lib)
	}

	// Sort libraries by ID for deterministic output
	slices.SortFunc(libraries, func(a, b model.ManifestLibrary) int {
		return cmp.Compare(a.LibraryID, b.LibraryID)
	})

	return &model.Manifest{
		HtsgetBaseUrl: g.HtsgetBaseUrl,
		GeneratedAt:   now,
		Libraries:     libraries,
	}
}

// filterActive returns only the shares that are currently active (not expired, not revoked).
func filterActive(shares []model.ShareRecord, now time.Time) []*model.ShareRecord {
	var active []*model.ShareRecord
	for i := range shares {
		if shares[i].EffectiveStatus(now) == model.ShareStatusActive {
			active = append(active, &shares[i])
		}
	}
	return active
}

// groupByLibrary groups shares by library ID, maintaining which shares grant each library.
// Returns a sorted slice of libraryGrant (sorted by library ID for deterministic output).
func groupByLibrary(shares []*model.ShareRecord) []libraryGrant {
	grantMap := make(map[string]*libraryGrant)

	for _, share := range shares {
		for _, libID := range share.LibraryIDs {
			if grant, ok := grantMap[libID]; ok {
				grant.shares = append(grant.shares, share)
			} else {
				grantMap[libID] = &libraryGrant{
					libraryID: libID,
					shares:    []*model.ShareRecord{share},
				}
			}
		}
	}

	// Convert map to sorted slice
	grants := make([]libraryGrant, 0, len(grantMap))
	for _, grant := range grantMap {
		grants = append(grants, *grant)
	}
	slices.SortFunc(grants, func(a, b libraryGrant) int {
		return cmp.Compare(a.libraryID, b.libraryID)
	})

	return grants
}

// buildManifestLibrary constructs a ManifestLibrary from a libraryGrant,
// computing the effective expiry and building file entries.
func buildManifestLibrary(grant libraryGrant) model.ManifestLibrary {
	expiresAt := computeEffectiveExpiry(grant.shares)
	files := buildFiles(grant.libraryID, grant.shares)

	return model.ManifestLibrary{
		LibraryID: grant.libraryID,
		ExpiresAt: expiresAt,
		Files:     files,
	}
}

// computeEffectiveExpiry computes the effective expiry for a library across multiple shares.
// The effective expiry is the latest expiry among the shares granting that library,
// or nil if ANY granting share is unlimited (no expiry).
func computeEffectiveExpiry(shares []*model.ShareRecord) *time.Time {
	var latest *time.Time

	for _, share := range shares {
		if share.ExpiresAt == nil {
			// Any unlimited share means the library is effectively unlimited
			return nil
		}
		if latest == nil || share.ExpiresAt.After(*latest) {
			expiry := *share.ExpiresAt
			latest = &expiry
		}
	}

	return latest
}

// buildFiles constructs the ManifestFile entries for a library from the files the
// granting shares recorded at share time.
//
// The request path is `reads/<htsgetId>` (reads endpoint; variants out of
// scope), where the id is the object key without its bucket or format extension. The
// library ID cannot stand in for it: htsget-rs resolves an id against an S3 key, and
// a library maps to one or more objects whose keys are unrelated to its ID, so
// `reads/<library_id>` names nothing that can be fetched.
//
// Entries are deduplicated and sorted by request path so the manifest is stable when
// several shares grant the same library. A share written before file recording existed
// contributes nothing; the library then appears with an empty file list rather than a
// fabricated path that would 404.
//
// Access rules are not attached at the share level in the current design
// (whole-library access).
func buildFiles(libraryID string, shares []*model.ShareRecord) []model.ManifestFile {
	seen := make(map[string]bool)
	files := make([]model.ManifestFile, 0)

	for _, share := range shares {
		for _, file := range share.Files {
			if file.LibraryID != libraryID || file.HtsgetID == "" {
				continue
			}
			request := "reads/" + file.HtsgetID
			if seen[request] {
				continue
			}
			seen[request] = true
			files = append(files, model.ManifestFile{Request: request})
		}
	}

	slices.SortFunc(files, func(a, b model.ManifestFile) int {
		return cmp.Compare(a.Request, b.Request)
	})

	return files
}
