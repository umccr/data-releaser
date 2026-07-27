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
// This prevents SQL injection since Athena does not support parameterized queries.
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

// BuildResolutionQuery constructs the SQL query for resolving library IDs to their
// associated BAM files in the OrcaVault Mart. The query targets mart.bam only and
// filters to format = 'bam'.
//
// The database parameter specifies the Lake Formation resource-link database name
// (typically "mart") from the config.
//
// Library IDs are validated before inclusion in the query to prevent SQL injection.
// Athena does not support traditional parameterized queries, so validation ensures
// only safe characters (alphanumeric, dash, underscore, dot) appear in the IN clause.
//
// Returns the SQL query string or an error if any library ID fails validation.
func BuildResolutionQuery(database string, libraryIDs []string) (string, error) {
	if err := ValidateLibraryIDs(libraryIDs); err != nil {
		return "", err
	}

	if database == "" {
		return "", fmt.Errorf("database name must not be empty")
	}

	// Build the quoted IN clause values
	quotedIDs := make([]string, len(libraryIDs))
	for i, id := range libraryIDs {
		quotedIDs[i] = "'" + id + "'"
	}

	query := fmt.Sprintf(
		`SELECT b.library_id, b.portal_run_id, b.bucket, b.key, b.format, b.storage_class FROM %s.bam AS b WHERE b.library_id IN (%s) AND lower(b.format) = 'bam'`,
		database,
		strings.Join(quotedIDs, ", "),
	)

	return query, nil
}
