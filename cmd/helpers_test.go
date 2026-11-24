package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/umccr/data-releaser/internal/config"
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

func TestResolveTargetVpcArn(t *testing.T) {
	const account = "777788889999"
	const arn = "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-0123456789abcdef0"

	cfg := &config.Config{
		Researchers: map[string]config.ResearcherConfig{
			account: {TargetVpcArn: arn},
		},
	}

	// The flag takes precedence over the config entry.
	flagArn := "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-aaaaaaaaaaaaaaaaa"
	got, err := resolveTargetVpcArn(cfg, flagArn, account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != flagArn {
		t.Errorf("resolveTargetVpcArn with flag = %q, want %q", got, flagArn)
	}

	// Falling back to the config entry.
	got, err = resolveTargetVpcArn(cfg, "", account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != arn {
		t.Errorf("resolveTargetVpcArn from config = %q, want %q", got, arn)
	}

	// An unconfigured account is an error, not a derived placeholder: a guessed ARN
	// would create a share that silently never authorizes anything.
	if _, err := resolveTargetVpcArn(cfg, "", "111111111111"); err == nil {
		t.Error("expected an error for an account with no configured target VPC ARN")
	}

	// The legacy vpc-lattice ARN shape is rejected — VPC Lattice reports the EC2 VPC ARN.
	legacy := "arn:aws:vpc-lattice:ap-southeast-2:777788889999:servicenetworkserviceassociation/*"
	if _, err := resolveTargetVpcArn(cfg, legacy, account); err == nil {
		t.Error("expected a vpc-lattice service-network-association ARN to be rejected")
	}

	// An ARN for a different account than the share targets is rejected.
	otherAccount := "arn:aws:ec2:ap-southeast-2:111111111111:vpc/vpc-0123456789abcdef0"
	if _, err := resolveTargetVpcArn(cfg, otherAccount, account); err == nil {
		t.Error("expected an ARN from a different account to be rejected")
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

// TestRenderListing_ShowsAllStatuses verifies the listing renders active, expired,
// and revoked records — expired and revoked shares must remain visible.
func TestRenderListing_ShowsAllStatuses(t *testing.T) {
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
	renderListing(&buf, htsgetRows(records, now))
	out := buf.String()

	// Header present.
	if !strings.Contains(out, "MECHANISM") || !strings.Contains(out, "STATE") {
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
