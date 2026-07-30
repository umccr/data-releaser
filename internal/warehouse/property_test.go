package warehouse

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/testgen"
)

// TestProperty4_NoPartialShareOnResolutionProblems validates Property 4 at the
// warehouse-resolution stage: for any combination of no-shareable-reads (missing)
// and archived libraries in the input, ValidateResolution reports an error, so
// the share pipeline aborts before any Share_Record is created. When every
// requested library resolves to a non-archived row, validation passes.
//
// This is the resolution half of task 3.4; the end-to-end pipeline half lives in
// internal/auth (TestProperty4_NoPartialShareEndToEnd).
//
// Validates: Requirements 1.6, 1.7, 1.8
func TestProperty4_NoPartialShareOnResolutionProblems(t *testing.T) {
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
