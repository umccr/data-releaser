package dataset

// The ledger record for one grant of read access to one copy of a dataset.
//
// A grant is what a researcher actually holds: their account, the VPC their reads must
// arrive from, and when the access ends. It points at the copy it reads from and carries
// that copy's bucket and key alongside, because those cannot change for a given copy and
// a record that has to be joined to something else before it can be understood is a worse
// record. A grant renders its own policies, prints its own row in a listing, and is
// compared against what is deployed, all without reading anything else.
//
// Several grants may be live on one copy at once. One dataset released to two researchers
// is two grants, two granting statements in the bucket policy, and two decrypt statements
// in the key policy -- not one record that has to describe both. Withdrawing one leaves
// the other exactly as it was.

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// GrantState is the lifecycle position of one grant.
//
// Both stored states are persisted verbatim. Expiry is derived by comparing the recorded
// expiry against the current time, so a grant whose expiry has passed remains a granted
// record that can be extended, rather than one some scheduler had to rewrite.
type GrantState string

const (
	// GrantStateGranted means the researcher's account is named in the deployed policies.
	GrantStateGranted GrantState = "granted"

	// GrantStateRevoked means those statements have been taken out. The objects and the
	// key are untouched, so the copy can be granted again -- as a new grant, since this
	// one's withdrawal is part of the record of what happened.
	GrantStateRevoked GrantState = "revoked"

	// GrantStateExpired is derived from the recorded expiry and is never persisted. The
	// granting statements are still deployed; the time condition in them no longer admits
	// the researcher, and moving the expiry forward restores access.
	GrantStateExpired GrantState = "expired"
)

// grantStates lists the states a grant may be persisted in, in lifecycle order.
var grantStates = []GrantState{
	GrantStateGranted,
	GrantStateRevoked,
}

// GrantStates returns the states a grant may be persisted in. GrantStateExpired is
// absent: it is derived on read, and writing it would lose the recorded expiry that makes
// an expired grant extendable.
func GrantStates() []GrantState {
	return slices.Clone(grantStates)
}

// IsStored reports whether s is a state a grant may be written in.
func (s GrantState) IsStored() bool {
	return slices.Contains(grantStates, s)
}

func (s GrantState) String() string {
	return string(s)
}

// ParseGrantState converts a persisted state string into a GrantState, rejecting the
// derived one: reading it back would mean something wrote a state it computed instead of
// the state it observed.
func ParseGrantState(s string) (GrantState, error) {
	candidate := GrantState(s)
	if candidate.IsStored() {
		return candidate, nil
	}
	names := make([]string, 0, len(grantStates))
	for _, state := range grantStates {
		names = append(names, string(state))
	}
	if candidate == GrantStateExpired {
		return "", fmt.Errorf(
			"%q is derived from the recorded expiry and is never stored; expected one of %s",
			s, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("unknown grant state %q; expected one of %s", s, strings.Join(names, ", "))
}

// Grant is the ledger record for one researcher account's access to one copy.
type Grant struct {
	// GrantID identifies this grant. It is the operand the extend and revoke commands
	// take.
	GrantID string `json:"grantId"`

	// CopyID is the copy this grant reads from. A copy may carry several grants; a grant
	// reads from exactly one.
	CopyID string `json:"copyId"`

	// DatasetURI, HotCacheBucket and KeyArn are the copy's, carried here so that a grant
	// renders its own policies and prints its own row without a join. They cannot change
	// for a given copy.
	DatasetURI     string `json:"datasetUri"`
	HotCacheBucket string `json:"hotCacheBucket"`
	KeyArn         string `json:"keyArn"`

	// ResearcherAccountID is the external account granted read access.
	ResearcherAccountID string `json:"researcherAccountId"`

	// TargetVpcID is the bare VPC identifier reads must arrive from; requests reaching S3
	// from anywhere else are denied. It is the bare identifier rather than an ARN because
	// that is the form the bucket policy condition compares against.
	TargetVpcID string `json:"targetVpcId"`

	// State is the stored lifecycle position. Read it through EffectiveState so that a
	// passed expiry is accounted for.
	State GrantState `json:"state"`

	// CreatedAt is when the access was granted, which is what a listing orders by: it is
	// the moment that matters to whoever is asking who holds what.
	CreatedAt time.Time `json:"createdAt"`

	// ExpiresAt is when access ceases. Nil means unlimited, in which case the granting
	// policies carry no time condition at all rather than a far-future one.
	ExpiresAt *time.Time `json:"expiresAt"`

	// RevokedAt is when the granting statements were taken out. Nil means not revoked,
	// and it is never cleared: a grant that ended is part of the record of what happened.
	RevokedAt *time.Time `json:"revokedAt"`
}

// EffectiveState reports the state of the grant as at now.
//
// Revocation is recorded explicitly and takes precedence by virtue of being stored:
// withdrawing an expired grant moves it out of granted, and the record then reports that
// instead.
func (g *Grant) EffectiveState(now time.Time) GrantState {
	if g.State == GrantStateGranted && g.IsExpired(now) {
		return GrantStateExpired
	}
	return g.State
}

// IsExpired reports whether the recorded expiry has passed as at now. An unlimited grant
// never expires. The boundary counts as expired, matching how the deployed time condition
// admits requests strictly before the expiry.
func (g *Grant) IsExpired(now time.Time) bool {
	return g.ExpiresAt != nil && !g.ExpiresAt.After(now)
}

// IsUnlimited reports whether the grant carries no expiry, which is the default when none
// is asked for.
func (g *Grant) IsUnlimited() bool {
	return g.ExpiresAt == nil
}

// IsActive reports whether the grant currently gives the researcher access.
func (g *Grant) IsActive(now time.Time) bool {
	return g.EffectiveState(now) == GrantStateGranted
}

// IsDeployed reports whether the grant's statements are in the deployed documents.
//
// An expired grant still carries them -- expiry is a condition inside the statements, not
// their removal -- so it is deployed even though it admits nobody. This is what the
// renderer asks, and it is deliberately not the same question as whether anyone can
// currently read.
func (g *Grant) IsDeployed() bool {
	return g.State == GrantStateGranted
}

// IsExtendable reports whether the expiry may still be moved. Only revocation closes that
// off: a grant whose expiry has passed keeps its statements and its recorded expiry, so
// moving the expiry forward is all it takes to restore access.
func (g *Grant) IsExtendable() bool {
	return g.State == GrantStateGranted
}
