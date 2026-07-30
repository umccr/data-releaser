package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/umccr/data-releaser/internal/model"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"", 0, true},
		{"abc", 0, true},
		{"d", 0, true},
		{"-5d", 0, true}, // does not match the day pattern and is not a valid Go duration form here
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseDuration(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDuration(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDeriveTargetVpcArn(t *testing.T) {
	account := "534840902377"
	arn := deriveTargetVpcArn(account)
	if !strings.Contains(arn, account) {
		t.Errorf("expected ARN to contain account %q, got %q", account, arn)
	}
	if !strings.HasPrefix(arn, "arn:aws:vpc-lattice:") {
		t.Errorf("expected a vpc-lattice ARN, got %q", arn)
	}
}

func TestFormatLibraries(t *testing.T) {
	cases := []struct {
		ids  []string
		want string
	}{
		{[]string{}, ""},
		{[]string{"L1"}, "L1"},
		{[]string{"L1", "L2", "L3"}, "L1,L2,L3"},
		{[]string{"L1", "L2", "L3", "L4"}, "L1,L2,L3 (+1 more)"},
		{[]string{"L1", "L2", "L3", "L4", "L5"}, "L1,L2,L3 (+2 more)"},
	}
	for _, c := range cases {
		if got := formatLibraries(c.ids); got != c.want {
			t.Errorf("formatLibraries(%v) = %q, want %q", c.ids, got, c.want)
		}
	}
}

func TestFormatExpiry(t *testing.T) {
	if got := formatExpiry(nil); got != "unlimited" {
		t.Errorf("formatExpiry(nil) = %q, want \"unlimited\"", got)
	}
	ts := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := formatExpiry(&ts); got != ts.Format(time.RFC3339) {
		t.Errorf("formatExpiry(%v) = %q, want %q", ts, got, ts.Format(time.RFC3339))
	}
}

// TestRenderShareTable_ShowsAllStatuses verifies the listing renders active, expired,
// and revoked records — expired and revoked shares must remain visible (Req 8.1).
func TestRenderShareTable_ShowsAllStatuses(t *testing.T) {
	now := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)
	revokedAt := now.Add(-1 * time.Hour)

	records := []*model.ShareRecord{
		{
			ShareID:             "active-share",
			ResearcherAccountID: "111111111111",
			LibraryIDs:          []string{"L1"},
			CreatedAt:           past,
			ExpiresAt:           &future,
		},
		{
			ShareID:             "expired-share",
			ResearcherAccountID: "222222222222",
			LibraryIDs:          []string{"L2"},
			CreatedAt:           past,
			ExpiresAt:           &past,
		},
		{
			ShareID:             "revoked-share",
			ResearcherAccountID: "333333333333",
			LibraryIDs:          []string{"L3"},
			CreatedAt:           past,
			RevokedAt:           &revokedAt,
		},
	}

	var buf bytes.Buffer
	renderShareTable(&buf, records, now)
	out := buf.String()

	// Header present.
	if !strings.Contains(out, "SHARE ID") || !strings.Contains(out, "STATUS") {
		t.Errorf("expected a header row, got:\n%s", out)
	}
	// All three records visible regardless of status.
	for _, id := range []string{"active-share", "expired-share", "revoked-share"} {
		if !strings.Contains(out, id) {
			t.Errorf("expected record %q to be visible in the listing, got:\n%s", id, out)
		}
	}
	// Effective statuses rendered.
	for _, status := range []string{"active", "expired", "revoked"} {
		if !strings.Contains(out, status) {
			t.Errorf("expected status %q in output, got:\n%s", status, out)
		}
	}
}
