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
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/manifest"
)

var revokeCmd = &cobra.Command{
	Use:   "revoke <mechanism>",
	Short: "Stop a researcher account's access",
	Long: `Stop a researcher account's access to data that was previously shared.

The mechanism is named as part of the invocation and is never inferred, because
the record being revoked and the way access stops differ between them. The
available mechanisms are listed below.`,
}

var revokeHtsgetCmd = &cobra.Command{
	Use:   "htsget <share-id>",
	Short: "Revoke an active data share",
	Long: `Mark a share record as revoked in the authorization service.

The revocation itself is immediate and durable. Authorization decisions already
cached by htsget-rs continue to be served until they expire, so propagation is
bounded by the Cache-TTL ceiling; there is no operator override.`,
	Args: cobra.ExactArgs(1),
	RunE: runRevoke,
}

func init() {
	revokeCmd.AddCommand(revokeHtsgetCmd)
	requireMechanism(revokeCmd)
	rootCmd.AddCommand(revokeCmd)
}

func runRevoke(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shareID := args[0]

	// The config and the AWS clients it names.
	// Only the ledger is required. Revoking must not be blocked by anything the granting
	// path needs: the manifest that follows is republished on a best-effort basis, and a
	// missing manifest target is reported as a warning rather than stopping a revocation
	// that has already taken effect.
	cfg, err := requireConfig(config.SectionLedger)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		return err
	}

	// Withdrawing the share is what stops the authorization callout admitting it.
	store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Ledger.Table)

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

	// The researcher's own description of their access, rebuilt from what is left of it.
	// A manifest failure is reported but does not fail the command: the revocation is
	// already durable, and re-running revoke would only report a conflict.
	if target, targetErr := resolveManifestTarget(cfg, record.ResearcherAccountID); targetErr != nil {
		warnf(cmd, "manifest not regenerated (revocation still applied): %v", targetErr)
	} else if syncErr := regenerateManifest(ctx, cfg, manifest.NewWriter(factory.S3Client()), store, record.ResearcherAccountID, target); syncErr != nil {
		warnf(cmd, "manifest regeneration failed (revocation still applied): %v", syncErr)
	}

	// What was withdrawn, from the record as it now stands.
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
