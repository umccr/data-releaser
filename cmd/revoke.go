package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
)

var revokeCmd = &cobra.Command{
	Use:   "revoke [share-id]",
	Short: "Revoke an active data share",
	Long: `Mark a Share_Record as revoked in the Authorization Service.

The revocation itself is immediate and durable. Authorization decisions already
cached by htsget-rs continue to be served until they expire, so propagation is
bounded by the Cache-TTL ceiling; there is no operator override.`,
	Args: cobra.ExactArgs(1),
	RunE: runRevoke,
}

func init() {
	rootCmd.AddCommand(revokeCmd)
}

func runRevoke(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shareID := args[0]

	// --- Step 1: Load config and create AWS client factory ---
	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// --- Step 2: Revoke the share via auth service (Req 3.1) ---
	store := auth.NewShareStore(factory.DynamoDBClient(), shareRecordsTable())

	record, err := store.Revoke(ctx, shareID)
	if err != nil {
		// Handle specific state-conflict errors (Req 3.3, 3.5, 3.6, 3.7)
		switch {
		case errors.Is(err, auth.ErrShareNotFound):
			fmt.Fprintf(os.Stderr, "Error: share %q not found\n", shareID)
		case errors.Is(err, auth.ErrShareAlreadyRevoked):
			fmt.Fprintf(os.Stderr, "Error: share %q is already revoked\n", shareID)
		case errors.Is(err, auth.ErrShareExpired):
			fmt.Fprintf(os.Stderr, "Error: share %q has already expired\n", shareID)
		default:
			fmt.Fprintf(os.Stderr, "Error: failed to revoke share: %v\n", err)
		}
		return err
	}

	// --- Step 3: Regenerate manifest (Req 7.3, 7.4) ---
	allShares, err := store.ListByAccount(ctx, record.ResearcherAccountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to list shares for manifest regeneration: %v\n", err)
		// Don't fail the revoke command — the record was already revoked
	} else {
		now := time.Now().UTC()
		var activeShares []model.ShareRecord
		for _, s := range allShares {
			if s.EffectiveStatus(now) == model.ShareStatusActive {
				activeShares = append(activeShares, *s)
			}
		}

		generator := manifest.NewGenerator(cfg.Endpoints.HtsgetBaseURL)
		manifestDoc := generator.Generate(activeShares, now)

		manifestBucket := strings.TrimSpace(cfg.Manifest.Bucket)
		manifestKey, keyErr := cfg.Manifest.RenderManifestKey(record.ResearcherAccountID)
		switch {
		case manifestBucket == "":
			fmt.Fprintf(os.Stderr, "Warning: manifest.bucket is not configured; skipping manifest regeneration (revocation still applied)\n")
		case keyErr != nil:
			fmt.Fprintf(os.Stderr, "Warning: could not resolve manifest key (revocation still applied): %v\n", keyErr)
		default:
			writer := manifest.NewWriter(factory.S3Client())
			if syncErr := writer.Sync(ctx, manifestBucket, manifestKey, manifestDoc); syncErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: manifest sync failed (revocation still applied): %v\n", syncErr)
			}
		}
	}

	// --- Step 4: Output confirmation (Req 3.4) ---
	revokedAt := record.RevokedAt.Format(time.RFC3339)
	fmt.Printf("Share revoked successfully:\n")
	fmt.Printf("  Share ID:    %s\n", record.ShareID)
	fmt.Printf("  Revoked at:  %s\n", revokedAt)

	// The revocation is immediate and durable; only its observation by htsget-rs is
	// delayed, because authorization decisions already cached there are served until
	// they expire. There is no cache to flush and no operator override.
	fmt.Fprintf(os.Stderr, "Note: propagation is bounded by the Cache-TTL ceiling.\n")

	return nil
}
