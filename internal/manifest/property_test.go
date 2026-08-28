package manifest

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/testgen"
)

// After any sequence of
// create/revoke/extend/expiry operations, the generated manifest reflects exactly
// the library IDs of the remaining active grants, and is nil (i.e. deleted by the
// writer's Sync) when no active grants remain.
func TestManifestConvergence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		now := time.Now().UTC()
		seq := testgen.Operations(t, now, 5, 8)

		// Work on a copy so we can apply the mutation sequence in order.
		shares := make([]model.ShareRecord, len(seq.Initial))
		copy(shares, seq.Initial)

		for _, op := range seq.Ops {
			idx := op.Target
			if idx < 0 || idx >= len(shares) {
				continue
			}
			s := &shares[idx]
			status := s.EffectiveStatus(now)

			switch op.Kind {
			case testgen.OpRevoke:
				// Revoking only affects a currently-active share; conflicts on an
				// already-revoked/expired share leave state unchanged.
				if status == model.ShareStatusActive {
					rev := now.Add(-time.Minute)
					s.RevokedAt = &rev
				}
			case testgen.OpExtend:
				// Extending only applies to an active share.
				if status == model.ShareStatusActive {
					exp := now.Add(op.Duration)
					s.ExpiresAt = &exp
				}
			case testgen.OpExpire:
				// Natural expiry: force the expiry into the past. A revoked share
				// stays revoked (revocation wins in EffectiveStatus).
				past := now.Add(-time.Hour)
				s.ExpiresAt = &past
			}
		}

		// Expected: the set of library IDs across the remaining active shares.
		expected := make(map[string]bool)
		for i := range shares {
			if shares[i].EffectiveStatus(now) != model.ShareStatusActive {
				continue
			}
			for _, lib := range shares[i].LibraryIDs {
				expected[lib] = true
			}
		}

		gen := NewGenerator("https://htsget.example.org")
		m := gen.Generate(shares, now)

		if len(expected) == 0 {
			if m != nil {
				t.Fatalf("expected nil manifest (deletion) when no active grants remain, got %d libraries", len(m.Libraries))
			}
			return
		}

		if m == nil {
			t.Fatalf("expected a manifest with %d libraries, got nil", len(expected))
		}

		got := make(map[string]bool)
		for _, lib := range m.Libraries {
			if got[lib.LibraryID] {
				t.Fatalf("manifest lists library %q more than once", lib.LibraryID)
			}
			got[lib.LibraryID] = true
		}

		if len(got) != len(expected) {
			t.Fatalf("manifest library set size mismatch: got %d, want %d", len(got), len(expected))
		}
		for lib := range expected {
			if !got[lib] {
				t.Fatalf("manifest is missing active library %q", lib)
			}
		}
		for lib := range got {
			if !expected[lib] {
				t.Fatalf("manifest includes non-active library %q", lib)
			}
		}
	})
}
