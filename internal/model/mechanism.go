package model

import (
	"fmt"
	"slices"
	"strings"
)

// Mechanism names one of the two ways this tool grants access, and is the discriminator
// on records in the shared ledger table.
//
// One table holds both kinds of record so that "what can account X read?" has a single
// answer. The records themselves have almost nothing in common: they carry different
// fields, enforce expiry by different means, and are read by different code. So every
// read of the table has to say which kind of record it wants, and this is the attribute
// that answers it.
type Mechanism string

const (
	// MechanismHtsget grants range-limited reads through htsget, where each request is
	// authorized by a callout that reads the record.
	MechanismHtsget Mechanism = "htsget"

	// MechanismS3 grants whole-object reads directly from S3, where access is decided
	// by the bucket and key policies rather than by anything of ours.
	MechanismS3 Mechanism = "s3"
)

// MechanismAttribute is the ledger item attribute holding the mechanism. Its value is
// one of the Mechanism constants, and its absence means htsget — see StoredMechanism.
const MechanismAttribute = "mechanism"

// mechanisms lists every mechanism, in the order an operator should see them.
var mechanisms = []Mechanism{MechanismHtsget, MechanismS3}

// Mechanisms returns every mechanism, for listing the available choices.
func Mechanisms() []Mechanism {
	return slices.Clone(mechanisms)
}

// MechanismNames returns every mechanism as the word used to name it on the command
// line, for reporting the available choices to an operator.
func MechanismNames() []string {
	names := make([]string, 0, len(mechanisms))
	for _, m := range mechanisms {
		names = append(names, string(m))
	}
	return names
}

func (m Mechanism) String() string {
	return string(m)
}

// IsValid reports whether m is a mechanism this tool knows about.
func (m Mechanism) IsValid() bool {
	return slices.Contains(mechanisms, m)
}

// ParseMechanism converts an operator-supplied mechanism name, rejecting anything else
// including the empty string: which mechanism a mutating command acts on is never
// guessed, because granting the wrong kind of access does not announce itself.
func ParseMechanism(s string) (Mechanism, error) {
	candidate := Mechanism(s)
	if candidate.IsValid() {
		return candidate, nil
	}
	if s == "" {
		return "", fmt.Errorf("no mechanism given; expected one of %s", strings.Join(MechanismNames(), ", "))
	}
	return "", fmt.Errorf("unknown mechanism %q; expected one of %s", s, strings.Join(MechanismNames(), ", "))
}

// StoredMechanism interprets the mechanism attribute of a ledger item.
//
// An absent attribute means htsget. Every record written before the table was shared is
// an htsget share and none of them carry the attribute, so the absence has to keep
// meaning htsget — nothing rewrites those items, and reading one as anything else would
// lose a live share.
//
// A value that is not recognised is returned as it stands rather than defaulted, so a
// record written by some later mechanism matches neither of today's and is left alone by
// both.
func StoredMechanism(attribute string) Mechanism {
	if attribute == "" {
		return MechanismHtsget
	}
	return Mechanism(attribute)
}
