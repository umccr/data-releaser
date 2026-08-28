package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/umccr/data-releaser/internal/auth"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
)

// manifestTarget is the resolved destination of a researcher's manifest.
type manifestTarget struct {
	bucket string
	key    string
}

// resolveManifestTarget resolves the Manifest_Bucket and the account's per-account key.
// Both are needed before any share is written, so that a share is never persisted only
// to fail on the manifest upload.
func resolveManifestTarget(cfg *config.Config, accountID string) (manifestTarget, error) {
	bucket := strings.TrimSpace(cfg.Manifest.Bucket)
	if bucket == "" {
		return manifestTarget{}, fmt.Errorf("manifest.bucket is not configured; the Manifest_Bucket name must be sourced from the Infrastructure_Stack output")
	}

	key, err := cfg.Manifest.RenderManifestKey(accountID)
	if err != nil {
		return manifestTarget{}, err
	}

	return manifestTarget{bucket: bucket, key: key}, nil
}

// regenerateManifest rebuilds the researcher's manifest from every share currently
// recorded for the account and syncs it to the Manifest_Bucket. It is the single
// implementation of the "the account's shares changed, republish its manifest" step that
// share, extend, and revoke all perform.
//
// The manifest is derived from the full share list rather than the mutated share alone:
// a researcher has one manifest covering all their shares, so it must be
// rebuilt from the current set. When no active shares remain the generator returns nil
// and Sync deletes the object.
func regenerateManifest(ctx context.Context, cfg *config.Config, writer *manifest.Writer, store *auth.ShareStore, accountID string, target manifestTarget) error {
	allShares, err := store.ListByAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("listing shares for manifest generation: %w", err)
	}

	now := time.Now().UTC()
	doc := manifest.NewGenerator(cfg.Endpoints.HtsgetBaseURL).Generate(deref(allShares), now)

	return writer.Sync(ctx, target.bucket, target.key, doc)
}

// deref flattens share pointers into values for the generator, which filters to active
// shares itself.
func deref(records []*model.ShareRecord) []model.ShareRecord {
	out := make([]model.ShareRecord, 0, len(records))
	for _, r := range records {
		out = append(out, *r)
	}
	return out
}
