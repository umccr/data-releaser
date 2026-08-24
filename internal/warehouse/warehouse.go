// Package warehouse implements the OrcaVault client for resolving library IDs
// to shareable S3 prefixes via Athena queries against the mart.bam table.
package warehouse

import (
	"fmt"
	"regexp"
	"strings"
)

// validLibraryIDPattern defines the allowed characters for a library ID.
// Only alphanumeric characters, dashes, underscores, and dots are permitted.
//
// This is an input-quality check, not a security boundary: the resolution query binds
// library IDs as Athena execution parameters, so escaping is Athena's job rather than
// ours. Rejecting odd IDs up front still gives a clearer error than an empty result set.
var validLibraryIDPattern = regexp.MustCompile(`^[a-zA-Z0-9\-_.]+$`)

// ValidateLibraryID checks whether a library ID contains only safe characters.
// Returns an error if the ID is empty or contains characters outside the allowed set.
func ValidateLibraryID(id string) error {
	if id == "" {
		return fmt.Errorf("library ID must not be empty")
	}
	if !validLibraryIDPattern.MatchString(id) {
		return fmt.Errorf("library ID %q contains invalid characters; only alphanumeric, dash, underscore, and dot are allowed", id)
	}
	return nil
}

// ValidateLibraryIDs validates a slice of library IDs. Returns an error listing
// all invalid IDs if any fail validation.
func ValidateLibraryIDs(ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("at least one library ID is required")
	}

	var invalid []string
	for _, id := range ids {
		if err := ValidateLibraryID(id); err != nil {
			invalid = append(invalid, id)
		}
	}

	if len(invalid) > 0 {
		return fmt.Errorf("invalid library IDs: %s", strings.Join(invalid, ", "))
	}
	return nil
}

// ResolutionQuery is a parameterized Athena query: SQL carrying positional `?`
// placeholders, plus the values to bind to them in order.
type ResolutionQuery struct {
	// SQL is the query text with one `?` placeholder per library ID.
	SQL string
	// Parameters are the values bound to those placeholders, in order.
	Parameters []string
}

// BuildResolutionQuery constructs the parameterized query for resolving library IDs to
// their associated BAM files in the OrcaVault Mart. The query targets mart.bam only and
// filters to format = 'bam'.
//
// The database parameter specifies the Lake Formation resource-link database name
// (typically "mart") from the config. It is interpolated rather than bound, because
// Athena parameters can only stand in for values, not identifiers — it comes from
// operator config rather than from a share request, and a bad name fails the query
// outright rather than changing what it matches.
//
// Library IDs are bound as execution parameters, so quoting and escaping are Athena's
// responsibility. They are still validated first, purely to reject obvious nonsense with
// a clear message.
func BuildResolutionQuery(database string, libraryIDs []string) (*ResolutionQuery, error) {
	if err := ValidateLibraryIDs(libraryIDs); err != nil {
		return nil, err
	}

	if database == "" {
		return nil, fmt.Errorf("database name must not be empty")
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(libraryIDs)), ", ")

	return &ResolutionQuery{
		SQL: fmt.Sprintf(
			`SELECT b.library_id, b.portal_run_id, b.bucket, b.key, b.format, b.storage_class FROM %s.bam AS b WHERE b.library_id IN (%s) AND lower(b.format) = 'bam'`,
			database,
			placeholders,
		),
		Parameters: libraryIDs,
	}, nil
}
