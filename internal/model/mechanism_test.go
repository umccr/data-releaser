package model

import (
	"strings"
	"testing"
)

func TestStoredMechanism(t *testing.T) {
	tests := []struct {
		name      string
		attribute string
		want      Mechanism
	}{
		{
			// Records written before the table was shared carry no attribute, and are
			// all htsget shares. Reading one as anything else would lose a live share.
			name:      "absent attribute is an htsget share",
			attribute: "",
			want:      MechanismHtsget,
		},
		{
			name:      "htsget",
			attribute: "htsget",
			want:      MechanismHtsget,
		},
		{
			name:      "s3",
			attribute: "s3",
			want:      MechanismS3,
		},
		{
			// Returned as it stands rather than defaulted, so a record written by some
			// later mechanism matches neither of today's.
			name:      "unrecognised value is not defaulted",
			attribute: "something-later",
			want:      Mechanism("something-later"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StoredMechanism(tt.attribute); got != tt.want {
				t.Errorf("StoredMechanism(%q): expected %q, got %q", tt.attribute, tt.want, got)
			}
		})
	}
}

func TestParseMechanism(t *testing.T) {
	for _, name := range MechanismNames() {
		t.Run(name, func(t *testing.T) {
			got, err := ParseMechanism(name)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != name {
				t.Errorf("expected %q, got %q", name, got)
			}
		})
	}

	// An unnamed mechanism is a refusal, not a default: which mechanism a mutating
	// command acts on is never guessed.
	for _, input := range []string{"", "htsget ", "s3-direct", "HTSGET"} {
		t.Run("rejects "+input, func(t *testing.T) {
			_, err := ParseMechanism(input)
			if err == nil {
				t.Fatalf("expected %q to be rejected", input)
			}
			for _, name := range MechanismNames() {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error should name the %q mechanism as available, got: %v", name, err)
				}
			}
		})
	}
}

func TestMechanismsAreIndependentCopies(t *testing.T) {
	first := Mechanisms()
	first[0] = "clobbered"

	if second := Mechanisms(); second[0] == "clobbered" {
		t.Error("Mechanisms returned a slice aliasing package state")
	}
}
