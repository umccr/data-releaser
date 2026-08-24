package warehouse

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateLibraryID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "valid simple", id: "L2400001", wantErr: false},
		{name: "valid with dash", id: "L2400-001", wantErr: false},
		{name: "valid with underscore", id: "L2400_001", wantErr: false},
		{name: "valid with dot", id: "L2400.001", wantErr: false},
		{name: "valid alphanumeric", id: "abc123XYZ", wantErr: false},
		{name: "empty string", id: "", wantErr: true},
		{name: "contains space", id: "L2400 001", wantErr: true},
		{name: "contains single quote", id: "L2400'001", wantErr: true},
		{name: "contains semicolon", id: "L2400;DROP", wantErr: true},
		{name: "contains double quote", id: `L2400"001`, wantErr: true},
		{name: "contains parenthesis", id: "L2400(001)", wantErr: true},
		{name: "contains SQL injection attempt", id: "'; DROP TABLE bam; --", wantErr: true},
		{name: "contains backslash", id: "L2400\\001", wantErr: true},
		{name: "contains slash", id: "L2400/001", wantErr: true},
		{name: "contains equals", id: "L2400=001", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLibraryID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateLibraryID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestValidateLibraryIDs(t *testing.T) {
	tests := []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{name: "single valid", ids: []string{"L2400001"}, wantErr: false},
		{name: "multiple valid", ids: []string{"L2400001", "L2400002", "L2400003"}, wantErr: false},
		{name: "empty slice", ids: []string{}, wantErr: true},
		{name: "nil slice", ids: nil, wantErr: true},
		{name: "one invalid", ids: []string{"L2400001", "bad;id"}, wantErr: true},
		{name: "all invalid", ids: []string{"bad;id", "other'id"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLibraryIDs(tt.ids)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateLibraryIDs(%v) error = %v, wantErr %v", tt.ids, err, tt.wantErr)
			}
		})
	}
}

func TestBuildResolutionQuery(t *testing.T) {
	tests := []struct {
		name       string
		database   string
		libraryIDs []string
		wantErr    bool
		// Substrings the SQL must contain when successful
		wantContains []string
		// Substrings the SQL must NOT contain when successful
		wantNotContains []string
		// Values expected to be bound to the SQL's placeholders, in order
		wantParameters []string
	}{
		{
			name:       "single library ID",
			database:   "mart",
			libraryIDs: []string{"L2400001"},
			wantErr:    false,
			wantContains: []string{
				"mart.bam AS b",
				"b.library_id IN (?)",
				"lower(b.format) = 'bam'",
				"b.library_id",
				"b.portal_run_id",
				"b.bucket",
				"b.key",
				"b.format",
				"b.storage_class",
			},
			wantParameters: []string{"L2400001"},
		},
		{
			name:       "multiple library IDs",
			database:   "mart",
			libraryIDs: []string{"L2400001", "L2400002", "L2400003"},
			wantErr:    false,
			wantContains: []string{
				"b.library_id IN (?, ?, ?)",
				"mart.bam AS b",
				"lower(b.format) = 'bam'",
			},
			wantParameters: []string{"L2400001", "L2400002", "L2400003"},
		},
		{
			name:       "custom database name",
			database:   "custom_db",
			libraryIDs: []string{"L2400001"},
			wantErr:    false,
			wantContains: []string{
				"custom_db.bam AS b",
			},
			wantNotContains: []string{
				"FROM mart.bam",
			},
			wantParameters: []string{"L2400001"},
		},
		{
			name:       "empty database name",
			database:   "",
			libraryIDs: []string{"L2400001"},
			wantErr:    true,
		},
		{
			name:       "empty library IDs",
			database:   "mart",
			libraryIDs: []string{},
			wantErr:    true,
		},
		{
			name:       "invalid library ID",
			database:   "mart",
			libraryIDs: []string{"L2400001", "bad;injection"},
			wantErr:    true,
		},
		{
			name:       "library ID with allowed special chars",
			database:   "mart",
			libraryIDs: []string{"L2400-001_v2.1"},
			wantErr:    false,
			wantContains: []string{
				"b.library_id IN (?)",
			},
			wantParameters: []string{"L2400-001_v2.1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, err := BuildResolutionQuery(tt.database, tt.libraryIDs)
			if (err != nil) != tt.wantErr {
				t.Errorf("BuildResolutionQuery() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			for _, substr := range tt.wantContains {
				if !strings.Contains(query.SQL, substr) {
					t.Errorf("BuildResolutionQuery() SQL missing expected substring %q\nGot: %s", substr, query.SQL)
				}
			}
			for _, substr := range tt.wantNotContains {
				if strings.Contains(query.SQL, substr) {
					t.Errorf("BuildResolutionQuery() SQL should not contain %q\nGot: %s", substr, query.SQL)
				}
			}
			if !slices.Equal(query.Parameters, tt.wantParameters) {
				t.Errorf("BuildResolutionQuery() Parameters = %v, want %v", query.Parameters, tt.wantParameters)
			}
		})
	}
}

// TestBuildResolutionQuery_BindsIDsRatherThanInliningThem pins the property the
// parameterized form exists for: an ID never becomes part of the SQL text, so escaping is
// Athena's job. The IDs here are the ones ValidateLibraryID accepts; the ones it rejects
// never reach this function.
func TestBuildResolutionQuery_BindsIDsRatherThanInliningThem(t *testing.T) {
	ids := []string{"L2400001", "L2400-001_v2.1"}
	query, err := BuildResolutionQuery("mart", ids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, id := range ids {
		if strings.Contains(query.SQL, id) {
			t.Errorf("SQL inlines library ID %q instead of binding it\nGot: %s", id, query.SQL)
		}
	}
	if got, want := strings.Count(query.SQL, "?"), len(ids); got != want {
		t.Errorf("SQL has %d placeholders, want %d\nGot: %s", got, want, query.SQL)
	}
	if !slices.Equal(query.Parameters, ids) {
		t.Errorf("Parameters = %v, want %v", query.Parameters, ids)
	}
}

func TestBuildResolutionQuery_SelectsCorrectColumns(t *testing.T) {
	query, err := BuildResolutionQuery("mart", []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify SELECT clause contains all required columns
	expectedColumns := []string{
		"b.library_id",
		"b.portal_run_id",
		"b.bucket",
		"b.key",
		"b.format",
		"b.storage_class",
	}
	for _, col := range expectedColumns {
		if !strings.Contains(query.SQL, col) {
			t.Errorf("query missing column %q\nGot: %s", col, query.SQL)
		}
	}
}

func TestBuildResolutionQuery_QueriesBamTableOnly(t *testing.T) {
	query, err := BuildResolutionQuery("mart", []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must reference mart.bam
	if !strings.Contains(query.SQL, "mart.bam") {
		t.Errorf("query should reference mart.bam\nGot: %s", query.SQL)
	}

	// Must NOT reference other mart tables
	if strings.Contains(query.SQL, "mart.lims") {
		t.Errorf("query should not reference mart.lims\nGot: %s", query.SQL)
	}
	if strings.Contains(query.SQL, "mart.output") {
		t.Errorf("query should not reference mart.output\nGot: %s", query.SQL)
	}
}

func TestBuildResolutionQuery_FiltersBamFormatOnly(t *testing.T) {
	query, err := BuildResolutionQuery("mart", []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must filter to format = 'bam'
	if !strings.Contains(query.SQL, "lower(b.format) = 'bam'") {
		t.Errorf("query should filter to format = 'bam'\nGot: %s", query.SQL)
	}
}
