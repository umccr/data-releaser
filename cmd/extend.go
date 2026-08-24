package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/manifest"
)

var extendCmd = &cobra.Command{
	Use:   "extend [share-id]",
	Short: "Extend the expiry of an active share",
	Long: `Update the expiry timestamp of an active Share_Record to the current
time plus the specified duration, and regenerate the researcher manifest.`,
	Args: cobra.ExactArgs(1),
	RunE: runExtend,
}

func init() {
	extendCmd.Flags().String("duration", "", "New duration from now (e.g. 30d, 24h)")

	_ = extendCmd.MarkFlagRequired("duration")

	rootCmd.AddCommand(extendCmd)
}

func runExtend(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shareID := strings.TrimSpace(args[0])

	// --- Step 1: Parse and validate duration ---
	durationStr, err := cmd.Flags().GetString("duration")
	if err != nil {
		return fmt.Errorf("reading --duration flag: %w", err)
	}

	duration, err := parseDuration(durationStr)
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}

	// Validate minimum 1 day (same as share creation, Req 2.2)
	if duration < 24*time.Hour {
		return fmt.Errorf("duration must be at least 1 day (24h), got %s", duration)
	}

	// --- Step 2: Load config and create AWS client factory ---
	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		return err
	}

	// --- Step 3: Get the share first to check if it's unlimited (Req 9.5) ---
	store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Authorization.ShareRecordsTable)

	existing, err := store.Get(ctx, shareID)
	if err != nil {
		if errors.Is(err, auth.ErrShareNotFound) {
			return fmt.Errorf("share %q not found", shareID)
		}
		return fmt.Errorf("failed to retrieve share: %w", err)
	}

	// Check if unlimited: if ExpiresAt is nil, report and exit (Req 9.5)
	if existing.ExpiresAt == nil {
		fmt.Printf("Share %s already has unlimited duration; no extension needed.\n", shareID)
		return nil
	}

	// --- Step 4: Extend the share (Req 9.1) ---
	updated, err := store.Extend(ctx, shareID, duration)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrShareNotFound):
			return fmt.Errorf("share %q not found", shareID)
		case errors.Is(err, auth.ErrShareAlreadyRevoked):
			return fmt.Errorf("share %q has been revoked and cannot be extended", shareID)
		case errors.Is(err, auth.ErrShareExpired):
			return fmt.Errorf("share %q has already expired and cannot be extended", shareID)
		default:
			return fmt.Errorf("failed to extend share: %w", err)
		}
	}

	// --- Step 5: Regenerate manifest for the affected researcher account (Req 9.3) ---
	// The extension is already durable, so the confirmation is printed either way; a
	// manifest failure is still returned as an error, because the researcher's manifest
	// now understates their access and the operator needs to retry.
	manifestErr := extendManifest(ctx, cfg, factory, store, updated.ResearcherAccountID)

	// --- Step 6: Output confirmation (Req 9.3) ---
	fmt.Printf("Share extended successfully:\n")
	fmt.Printf("  Share ID:   %s\n", updated.ShareID)
	fmt.Printf("  Account:    %s\n", updated.ResearcherAccountID)
	fmt.Printf("  Libraries:  %d (%s)\n", len(updated.LibraryIDs), strings.Join(updated.LibraryIDs, ", "))
	fmt.Printf("  New Expiry: %s\n", updated.ExpiresAt.Format(time.RFC3339))

	if manifestErr != nil {
		return fmt.Errorf("manifest regeneration failed (share %s was extended): %w", updated.ShareID, manifestErr)
	}

	return nil
}

// extendManifest republishes the researcher's manifest after an extension.
func extendManifest(ctx context.Context, cfg *config.Config, factory *internalaws.ClientFactory, store *auth.ShareStore, accountID string) error {
	target, err := resolveManifestTarget(cfg, accountID)
	if err != nil {
		return err
	}
	return regenerateManifest(ctx, cfg, manifest.NewWriter(factory.S3Client()), store, accountID, target)
}
