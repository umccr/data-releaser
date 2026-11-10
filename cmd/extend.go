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
	Use:   "extend <mechanism>",
	Short: "Move the expiry of an existing grant",
	Long: `Move the expiry of an existing grant so that access continues without the
data being shared again.

The mechanism is named as part of the invocation and is never inferred. Both
mechanisms take the record id as their only positional argument and the same
duration syntax. The available mechanisms are listed below.`,
}

var extendHtsgetCmd = &cobra.Command{
	Use:   "htsget <share-id>",
	Short: "Extend the expiry of an active share",
	Long: `Update the expiry timestamp of an active share record to the current
time plus the specified duration, and regenerate the researcher manifest.`,
	Args: cobra.ExactArgs(1),
	RunE: runExtend,
}

func init() {
	extendHtsgetCmd.Flags().String("duration", "", "New duration from now (e.g. 30d, 24h)")

	_ = extendHtsgetCmd.MarkFlagRequired("duration")

	extendCmd.AddCommand(extendHtsgetCmd)
	requireMechanism(extendCmd)
	rootCmd.AddCommand(extendCmd)
}

func runExtend(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shareID := strings.TrimSpace(args[0])

	// The duration, checked before anything is loaded or called.
	durationStr, err := cmd.Flags().GetString("duration")
	if err != nil {
		return fmt.Errorf("reading --duration flag: %w", err)
	}

	duration, err := parseDuration(durationStr)
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}

	// Validate minimum 1 day (same as share creation)
	if duration < 24*time.Hour {
		return fmt.Errorf("duration must be at least 1 day (24h), got %s", duration)
	}

	// The config and the AWS clients it names.
	// The extension itself only needs the ledger; the manifest that follows needs a
	// bucket and the htsget base URL, and both are checked here so a misconfigured
	// manifest is reported before the expiry is moved rather than after.
	cfg, err := requireConfig(
		config.SectionLedger,
		config.SectionManifest,
		config.SectionHtsgetEndpoint,
	)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		return err
	}

	// Read first, because a share granted without an expiry has nothing to move.
	store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Ledger.Table)

	existing, err := store.Get(ctx, shareID)
	if err != nil {
		if errors.Is(err, auth.ErrShareNotFound) {
			return fmt.Errorf("share %q not found", shareID)
		}
		return fmt.Errorf("failed to retrieve share: %w", err)
	}

	// Check if unlimited: if ExpiresAt is nil, report and exit
	if existing.ExpiresAt == nil {
		fmt.Printf("Share %s already has unlimited duration; no extension needed.\n", shareID)
		return nil
	}

	// The expiry itself, minted by the store from its own clock.
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

	// The researcher's own description carries the expiry, so it goes stale until this.
	// The extension is already durable, so the confirmation is printed either way; a
	// manifest failure is still returned as an error, because the researcher's manifest
	// now understates their access and the operator needs to retry.
	manifestErr := extendManifest(ctx, cfg, factory, store, updated.ResearcherAccountID)

	// What was moved, from the record as it now stands.
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
