package iampolicy

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/micahhausler/aws-iam-policy/policy"
)

// resources parses a resource element as it appears in a document, so the tests exercise the
// same shapes a deployed policy arrives in -- a bare string as readily as a list.
func resources(t *testing.T, element string) *policy.StringOrSlice {
	t.Helper()

	var parsed policy.StringOrSlice
	if err := json.Unmarshal([]byte(element), &parsed); err != nil {
		t.Fatalf("reading resource element %s: %v", element, err)
	}
	return &parsed
}

// TestResourceMatchers covers what separates a grant over a bucket's data from one that only
// looks like it. The near misses matter more than the hits: a bucket whose name is a prefix
// of another's, and a wildcard that stops short of the bucket name, decide whether drift is
// found or silently passed over.
func TestResourceMatchers(t *testing.T) {
	const bucket = "hot-cache-cohort-a"

	tests := []struct {
		element     string
		wantObjects bool
		wantBucket  bool
	}{
		{element: `"arn:aws:s3:::hot-cache-cohort-a/*"`, wantObjects: true},
		{element: `"arn:aws:s3:::hot-cache-cohort-a"`, wantBucket: true},
		{
			element:     `["arn:aws:s3:::hot-cache-cohort-a", "arn:aws:s3:::hot-cache-cohort-a/*"]`,
			wantObjects: true,
			wantBucket:  true,
		},
		// A grant scoped to one prefix still reaches the data, which is the difference
		// between this and asking whether every object is covered.
		{element: `"arn:aws:s3:::hot-cache-cohort-a/reads/*"`, wantObjects: true},
		{element: `"arn:aws:s3:::hot-cache-cohort-a/reads/one.bam"`, wantObjects: true},
		// A wildcard that stops short of the bucket's own name subsumes it.
		{element: `"arn:aws:s3:::hot-cache-*"`, wantObjects: true, wantBucket: true},
		{element: `"*"`, wantObjects: true, wantBucket: true},
		{element: `"arn:aws:s3:::*"`, wantObjects: true, wantBucket: true},
		// Another bucket whose name merely starts the same way.
		{element: `"arn:aws:s3:::hot-cache-cohort-abridged/*"`},
		{element: `"arn:aws:s3:::hot-cache-cohort-abridged"`},
		// Not an S3 resource at all, so it says nothing about this bucket.
		{element: `"arn:aws:kms:ap-southeast-2:916218008751:key/hot-cache-cohort-a"`},
		{element: `"hot-cache-cohort-a/*"`},
	}

	for _, tt := range tests {
		t.Run(tt.element, func(t *testing.T) {
			element := resources(t, tt.element)

			if got := ResourceNamesObjectIn(element, bucket); got != tt.wantObjects {
				t.Errorf("ResourceNamesObjectIn = %v, want %v", got, tt.wantObjects)
			}
			if got := ResourceNamesBucket(element, bucket); got != tt.wantBucket {
				t.Errorf("ResourceNamesBucket = %v, want %v", got, tt.wantBucket)
			}
		})
	}
}

func TestActionGrants(t *testing.T) {
	tests := []struct {
		element string
		action  string
		want    bool
	}{
		{element: `"s3:GetObject"`, action: "s3:GetObject", want: true},
		{element: `"s3:getobject"`, action: "s3:GetObject", want: true},
		{element: `["s3:PutObject", "s3:GetObject"]`, action: "s3:GetObject", want: true},
		{element: `"s3:*"`, action: "s3:GetObject", want: true},
		{element: `"*"`, action: "s3:GetObject", want: true},
		{element: `"kms:*"`, action: "kms:Decrypt", want: true},
		{element: `["s3:PutObject", "s3:ListBucket"]`, action: "s3:GetObject"},
		// A key policy statement that withdraws the data grants no decryption, which is
		// what keeps it out of a report about who can read.
		{element: `["kms:DisableKey", "kms:ScheduleKeyDeletion"]`, action: "kms:Decrypt"},
		{element: `"kms:*"`, action: "s3:GetObject"},
	}

	for _, tt := range tests {
		t.Run(tt.element+" covers "+tt.action, func(t *testing.T) {
			if got := ActionGrants(resources(t, tt.element), tt.action); got != tt.want {
				t.Errorf("ActionGrants = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPrincipalsOf covers the form that has to be read by value rather than by kind: the
// policy library reports a bare-string principal as the kind "All" whether it is the
// wildcard or an account root ARN, and treating the two alike would report every grant as a
// grant to everyone.
func TestPrincipalsOf(t *testing.T) {
	const account = "123456789012"

	tests := []struct {
		element      string
		wantAccounts []string
		wantOthers   []string
		wantAll      bool
	}{
		{element: `"*"`, wantAll: true},
		{element: `{"AWS": "*"}`, wantAll: true},
		{element: `"arn:aws:iam::123456789012:root"`, wantAccounts: []string{account}},
		{element: `{"AWS": "arn:aws:iam::123456789012:root"}`, wantAccounts: []string{account}},
		{element: `{"AWS": "123456789012"}`, wantAccounts: []string{account}},
		{
			element:      `{"AWS": ["arn:aws:iam::999999999999:root", "123456789012"]}`,
			wantAccounts: []string{"999999999999", account},
		},
		{element: `{"AWS": "arn:aws:iam::123456789012:role/reader"}`, wantAccounts: []string{account}},
		// An unrelated account's ARN carrying the digits somewhere in a role path names the
		// account it belongs to and no other.
		{
			element:      `{"AWS": "arn:aws:iam::999999999999:role/123456789012/audit"}`,
			wantAccounts: []string{"999999999999"},
		},
		{element: `{"Service": "s3.amazonaws.com"}`},
		{element: `{"AWS": "not-an-arn"}`, wantOthers: []string{"not-an-arn"}},
	}

	for _, tt := range tests {
		t.Run(tt.element, func(t *testing.T) {
			var principal policy.Principal
			if err := json.Unmarshal([]byte(tt.element), &principal); err != nil {
				t.Fatalf("reading principal element: %v", err)
			}

			got := PrincipalsOf(&principal)
			if got.All != tt.wantAll {
				t.Errorf("All = %v, want %v", got.All, tt.wantAll)
			}
			assertSame(t, "accounts", tt.wantAccounts, got.Accounts)
			assertSame(t, "others", tt.wantOthers, got.Others)

			wantIncludes := tt.wantAll || slices.Contains(tt.wantAccounts, account)
			if got := got.IncludesAccount(account); got != wantIncludes {
				t.Errorf("IncludesAccount(%s) = %v, want %v", account, got, wantIncludes)
			}
		})
	}
}

func assertSame(t *testing.T, what string, want, got []string) {
	t.Helper()

	if !slices.Equal(want, got) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
