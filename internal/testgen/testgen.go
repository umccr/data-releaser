// Package testgen provides shared property-based-test generators built on
// pgregory.net/rapid. The generators produce Share_Records (valid and edge
// cases: expired, revoked, unlimited/no-expiry, empty s3Prefixes), VPC ARNs /
// requester identities, and operation sequences (create/revoke/extend/expiry)
// over a set of shares.
//
// The package is imported from *_test.go files across the module. It is a
// regular (non-test) package so the generators can be reused everywhere; it has
// no dependencies beyond rapid and the domain model.
package testgen

import (
	"fmt"
	"time"

	"pgregory.net/rapid"

	"github.com/umccr/data-releaser/internal/model"
)

// AccountID generates a syntactically valid 12-digit AWS account ID.
func AccountID(t *rapid.T) string {
	return rapid.StringMatching(`[1-9][0-9]{11}`).Draw(t, "accountId")
}

// VPCArn generates a VPC Lattice service-network-association ARN for a randomly
// generated account. This mirrors the requester identity the authorization
// callout keys on (targetVpcArn / GSI1).
func VPCArn(t *rapid.T) string {
	return VPCArnForAccount(AccountID(t))
}

// VPCArnForAccount builds a VPC Lattice service-network-association ARN for the
// given account ID, matching the shape produced by the share command's
// deriveTargetVpcArn helper.
func VPCArnForAccount(accountID string) string {
	return fmt.Sprintf("arn:aws:vpc-lattice:ap-southeast-2:%s:servicenetworkserviceassociation/*", accountID)
}

// DistinctAccountIDs generates two distinct account IDs, useful for
// per-researcher isolation properties.
func DistinctAccountIDs(t *rapid.T) (string, string) {
	a := AccountID(t)
	var b string
	for {
		b = AccountID(t)
		if b != a {
			return a, b
		}
	}
}

// LibraryID generates a library ID that satisfies the warehouse library-ID
// validation rule (only alphanumeric, dash, underscore, and dot).
func LibraryID(t *rapid.T) string {
	return rapid.StringMatching(`[A-Za-z0-9_.-]{1,12}`).Draw(t, "libraryId")
}

// LibraryIDs generates a non-empty set of unique library IDs (1..5).
func LibraryIDs(t *rapid.T) []string {
	return rapid.SliceOfNDistinct(
		rapid.StringMatching(`[A-Za-z0-9_.-]{1,12}`),
		1, 5,
		func(s string) string { return s },
	).Draw(t, "libraryIds")
}

// S3Prefixes generates 0..3 unique S3 prefixes. Zero prefixes is a deliberate
// edge case (a Share_Record with an empty s3Prefixes set).
func S3Prefixes(t *rapid.T) []string {
	return rapid.SliceOfNDistinct(
		rapid.Custom(func(t *rapid.T) string {
			bucket := rapid.StringMatching(`[a-z0-9-]{3,12}`).Draw(t, "bucket")
			prefix := rapid.StringMatching(`[a-z0-9/-]{1,12}`).Draw(t, "prefix")
			return fmt.Sprintf("s3://%s/%s/", bucket, prefix)
		}),
		0, 3,
		func(s string) string { return s },
	).Draw(t, "s3Prefixes")
}

// ShareKind enumerates the effective-status flavours a generated Share_Record
// can take.
type ShareKind string

const (
	// KindActiveLimited is an active share with a future expiry.
	KindActiveLimited ShareKind = "active-limited"
	// KindActiveUnlimited is an active share with no expiry (unlimited).
	KindActiveUnlimited ShareKind = "active-unlimited"
	// KindExpired is a share whose expiry is in the past.
	KindExpired ShareKind = "expired"
	// KindRevoked is a share that has been revoked.
	KindRevoked ShareKind = "revoked"
)

// AllKinds is the full set of share kinds a generator may draw.
var AllKinds = []ShareKind{KindActiveLimited, KindActiveUnlimited, KindExpired, KindRevoked}

// ShareRecord generates a Share_Record relative to the given reference time
// `now`, drawing an arbitrary kind (active-limited, active-unlimited, expired,
// or revoked). The record's account and VPC ARN are generated consistently.
func ShareRecord(t *rapid.T, now time.Time) model.ShareRecord {
	accountID := AccountID(t)
	return ShareRecordForAccount(t, now, accountID, VPCArnForAccount(accountID))
}

// ShareRecordForAccount generates a Share_Record for a fixed account ID and VPC
// ARN, relative to `now`. This is used by the per-researcher isolation and
// idempotency properties where the account must be held constant.
func ShareRecordForAccount(t *rapid.T, now time.Time, accountID, vpcArn string) model.ShareRecord {
	kind := rapid.SampledFrom(AllKinds).Draw(t, "shareKind")
	return ShareRecordOfKind(t, now, accountID, vpcArn, kind)
}

// ShareRecordOfKind generates a Share_Record of a specific kind for the given
// account and VPC ARN, relative to `now`.
func ShareRecordOfKind(t *rapid.T, now time.Time, accountID, vpcArn string, kind ShareKind) model.ShareRecord {
	// CreatedAt is always in the past.
	createdOffset := time.Duration(rapid.Int64Range(1, 720).Draw(t, "createdHoursAgo")) * time.Hour
	createdAt := now.Add(-createdOffset)

	rec := model.ShareRecord{
		ShareID:             ULID(t),
		ResearcherAccountID: accountID,
		TargetVpcArn:        vpcArn,
		LibraryIDs:          LibraryIDs(t),
		S3Prefixes:          S3Prefixes(t),
		CreatedAt:           createdAt,
	}

	switch kind {
	case KindActiveUnlimited:
		// ExpiresAt nil, RevokedAt nil -> active, unlimited.
	case KindActiveLimited:
		fut := time.Duration(rapid.Int64Range(1, 8760).Draw(t, "expiresInHours")) * time.Hour
		exp := now.Add(fut)
		rec.ExpiresAt = &exp
	case KindExpired:
		past := time.Duration(rapid.Int64Range(1, 720).Draw(t, "expiredHoursAgo")) * time.Hour
		exp := now.Add(-past)
		rec.ExpiresAt = &exp
	case KindRevoked:
		// A revoked share may or may not also carry an expiry; revocation wins.
		if rapid.Bool().Draw(t, "revokedHasExpiry") {
			fut := time.Duration(rapid.Int64Range(1, 8760).Draw(t, "revokedExpiresInHours")) * time.Hour
			exp := now.Add(fut)
			rec.ExpiresAt = &exp
		}
		rev := now.Add(-time.Duration(rapid.Int64Range(1, 720).Draw(t, "revokedHoursAgo")) * time.Hour)
		rec.RevokedAt = &rev
	}

	return rec
}

// ULID generates a monotonically-unique 26-character identifier suitable for a
// ShareID. It is not a real ULID but is unique within a single generated
// sequence, which is all the property tests require.
func ULID(t *rapid.T) string {
	return "01" + rapid.StringMatching(`[0-9A-HJKMNP-TV-Z]{24}`).Draw(t, "shareId")
}

// ShareRecords generates a slice of 1..maxN Share_Records relative to `now`,
// all for the given account and VPC ARN.
func ShareRecords(t *rapid.T, now time.Time, accountID, vpcArn string, maxN int) []model.ShareRecord {
	n := rapid.IntRange(1, maxN).Draw(t, "numShares")
	recs := make([]model.ShareRecord, 0, n)
	seen := make(map[string]bool)
	for i := 0; i < n; i++ {
		rec := ShareRecordForAccount(t, now, accountID, vpcArn)
		// Ensure unique share IDs within the slice.
		for seen[rec.ShareID] {
			rec.ShareID = ULID(t)
		}
		seen[rec.ShareID] = true
		recs = append(recs, rec)
	}
	return recs
}

// OpKind identifies a mutation applied to an existing share in an operation
// sequence.
type OpKind string

const (
	// OpRevoke revokes the targeted share.
	OpRevoke OpKind = "revoke"
	// OpExtend extends the targeted share by a duration.
	OpExtend OpKind = "extend"
	// OpExpire forces the targeted share to be expired (sets a past expiry).
	OpExpire OpKind = "expire"
)

// Op is a single mutation in an operation sequence. Target indexes into the
// initial share slice (callers should take Target modulo len(shares)).
type Op struct {
	Kind     OpKind
	Target   int
	Duration time.Duration
}

// OpSequence is a generated initial set of shares plus a sequence of mutations
// (revoke/extend/expire) applied to them. It drives the manifest-convergence
// property, which asserts the generated manifest reflects exactly the remaining
// active grants after applying the sequence.
type OpSequence struct {
	Initial []model.ShareRecord
	Ops     []Op
}

// Operations generates an OpSequence: 1..maxShares initial shares for a single
// account, followed by 0..maxOps mutations targeting those shares.
func Operations(t *rapid.T, now time.Time, maxShares, maxOps int) OpSequence {
	accountID := AccountID(t)
	vpcArn := VPCArnForAccount(accountID)
	initial := ShareRecords(t, now, accountID, vpcArn, maxShares)

	numOps := rapid.IntRange(0, maxOps).Draw(t, "numOps")
	ops := make([]Op, 0, numOps)
	for i := 0; i < numOps; i++ {
		kind := rapid.SampledFrom([]OpKind{OpRevoke, OpExtend, OpExpire}).Draw(t, "opKind")
		op := Op{
			Kind:   kind,
			Target: rapid.IntRange(0, len(initial)-1).Draw(t, "opTarget"),
		}
		if kind == OpExtend {
			op.Duration = time.Duration(rapid.Int64Range(24, 8760).Draw(t, "extendHours")) * time.Hour
		}
		ops = append(ops, op)
	}

	return OpSequence{Initial: initial, Ops: ops}
}
