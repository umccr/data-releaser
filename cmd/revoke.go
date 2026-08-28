package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/manifest"
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
		return err
	}

	// --- Step 2: Revoke the share via auth service ---
	store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Authorization.ShareRecordsTable)

	record, err := store.Revoke(ctx, shareID)
	if err != nil {
		// Handle specific state-conflict errors
		switch {
		case errors.Is(err, auth.ErrShareNotFound):
			return fmt.Errorf("share %q not found", shareID)
		case errors.Is(err, auth.ErrShareAlreadyRevoked):
			return fmt.Errorf("share %q is already revoked", shareID)
		case errors.Is(err, auth.ErrShareExpired):
			return fmt.Errorf("share %q has already expired", shareID)
		default:
			return fmt.Errorf("failed to revoke share: %w", err)
		}
	}

	// --- Step 3: Regenerate manifest ---
	// A manifest failure is reported but does not fail the command: the revocation is
	// already durable, and re-running revoke would only report a conflict.
	if target, targetErr := resolveManifestTarget(cfg, record.ResearcherAccountID); targetErr != nil {
		warnf(cmd, "manifest not regenerated (revocation still applied): %v", targetErr)
	} else if syncErr := regenerateManifest(ctx, cfg, manifest.NewWriter(factory.S3Client()), store, record.ResearcherAccountID, target); syncErr != nil {
		warnf(cmd, "manifest regeneration failed (revocation still applied): %v", syncErr)
	}

	// --- Step 4: Output confirmation ---
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
