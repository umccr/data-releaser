// Package manifest implements manifest generation and S3 delivery for researcher accounts.
// The manifest is a JSON document listing the libraries and files a researcher can access.
package manifest

import (
	"fmt"
	"sort"
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
//
// Requirements: 7.1, 7.5, 7.6, 7.7
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
	sort.Slice(libraries, func(i, j int) bool {
		return libraries[i].LibraryID < libraries[j].LibraryID
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
	sort.Slice(grants, func(i, j int) bool {
		return grants[i].libraryID < grants[j].libraryID
	})

	return grants
}

// buildManifestLibrary constructs a ManifestLibrary from a libraryGrant,
// computing the effective expiry and building file entries.
func buildManifestLibrary(grant libraryGrant) model.ManifestLibrary {
	expiresAt := computeEffectiveExpiry(grant.shares)
	files := buildFiles(grant.libraryID)

	return model.ManifestLibrary{
		LibraryID: grant.libraryID,
		ExpiresAt: expiresAt,
		Files:     files,
	}
}

// computeEffectiveExpiry computes the effective expiry for a library across multiple shares.
// Per Req 7.7: the effective expiry is the latest expiry among the shares granting that library,
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

// buildFiles constructs the ManifestFile entries for a library.
// The htsget request path is `reads/<library_id>` (Req 7.6: reads endpoint; variants out of scope).
// Access rules are not attached at the share level in the current design (whole-library access).
func buildFiles(libraryID string) []model.ManifestFile {
	return []model.ManifestFile{
		{
			Request: fmt.Sprintf("reads/%s", libraryID),
		},
	}
}
