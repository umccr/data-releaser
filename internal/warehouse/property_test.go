package warehouse

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/testgen"
)

// At the
// warehouse-resolution stage: for any combination of no-shareable-reads (missing)
// and archived libraries in the input, ValidateResolution reports an error, so
// the share pipeline aborts before any share record is created. When every
// requested library resolves to a non-archived row, validation passes.
//
// The end-to-end pipeline half lives in
// internal/auth.
func TestNoPartialShareOnResolutionProblems(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		requested := testgen.LibraryIDs(t)

		var resolved []model.ResolvedLibrary
		anyProblem := false
		for i, id := range requested {
			kind := rapid.SampledFrom([]string{"ok", "missing", "archived"}).Draw(t, "resolveKind")
			switch kind {
			case "missing":
				// No row for this library -> no shareable reads.
				anyProblem = true
			case "archived":
				anyProblem = true
				resolved = append(resolved, model.ResolvedLibrary{
					LibraryID:    id,
					PortalRunID:  fmt.Sprintf("run-%d", i),
					Bucket:       "bucket",
					Key:          fmt.Sprintf("data/%s/sample.bam", id),
					Format:       "bam",
					StorageClass: rapid.SampledFrom([]string{"DEEP_ARCHIVE", "GLACIER", "GLACIER_IR"}).Draw(t, "archivedClass"),
				})
			default: // "ok"
				resolved = append(resolved, model.ResolvedLibrary{
					LibraryID:    id,
					PortalRunID:  fmt.Sprintf("run-%d", i),
					Bucket:       "bucket",
					Key:          fmt.Sprintf("data/%s/sample.bam", id),
					Format:       "bam",
					StorageClass: rapid.SampledFrom([]string{"STANDARD", "INTELLIGENT_TIERING", "STANDARD_IA"}).Draw(t, "liveClass"),
				})
			}
		}

		err := ValidateResolution(requested, resolved)
		if anyProblem && err == nil {
			t.Fatalf("expected ValidateResolution to reject problematic resolution, got nil error")
		}
		if !anyProblem && err != nil {
			t.Fatalf("expected ValidateResolution to accept a clean resolution, got: %v", err)
		}
	})
}
