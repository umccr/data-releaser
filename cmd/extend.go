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
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// --- Step 3: Get the share first to check if it's unlimited (Req 9.5) ---
	store := auth.NewShareStore(factory.DynamoDBClient(), shareRecordsTable())

	existing, err := store.Get(ctx, shareID)
	if err != nil {
		if errors.Is(err, auth.ErrShareNotFound) {
			fmt.Fprintf(os.Stderr, "Error: share %q not found\n", shareID)
			return err
		}
		fmt.Fprintf(os.Stderr, "Error: failed to retrieve share: %v\n", err)
		return err
	}

	// Check if unlimited: if ExpiresAt is nil, report and exit (Req 9.5)
	if existing.ExpiresAt == nil {
		fmt.Printf("Share %s already has unlimited duration; no extension needed.\n", shareID)
		return nil
	}

	// --- Step 4: Extend the share (Req 9.1) ---
	updated, err := store.Extend(ctx, shareID, duration)
	if err != nil {
		if errors.Is(err, auth.ErrShareNotFound) {
			fmt.Fprintf(os.Stderr, "Error: share %q not found\n", shareID)
		} else if errors.Is(err, auth.ErrShareAlreadyRevoked) {
			fmt.Fprintf(os.Stderr, "Error: share %q has been revoked and cannot be extended\n", shareID)
		} else if errors.Is(err, auth.ErrShareExpired) {
			fmt.Fprintf(os.Stderr, "Error: share %q has already expired and cannot be extended\n", shareID)
		} else {
			fmt.Fprintf(os.Stderr, "Error: failed to extend share: %v\n", err)
		}
		return err
	}

	// --- Step 5: Regenerate manifest for the affected researcher account (Req 9.3) ---
	allShares, err := store.ListByAccount(ctx, updated.ResearcherAccountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: share extended but manifest regeneration failed: %v\n", err)
		fmt.Printf("Share extended successfully:\n")
		fmt.Printf("  Share ID:   %s\n", updated.ShareID)
		fmt.Printf("  New Expiry: %s\n", updated.ExpiresAt.Format(time.RFC3339))
		return fmt.Errorf("manifest regeneration failed (share %s was extended): %w", updated.ShareID, err)
	}

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
	if manifestBucket == "" {
		fmt.Fprintf(os.Stderr, "Warning: share extended but manifest.bucket is not configured; manifest not updated\n")
		fmt.Printf("Share extended successfully:\n")
		fmt.Printf("  Share ID:   %s\n", updated.ShareID)
		fmt.Printf("  New Expiry: %s\n", updated.ExpiresAt.Format(time.RFC3339))
		return fmt.Errorf("manifest.bucket not configured (share %s was extended)", updated.ShareID)
	}
	manifestKey, err := cfg.Manifest.RenderManifestKey(updated.ResearcherAccountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: share extended but manifest key could not be resolved: %v\n", err)
		fmt.Printf("Share extended successfully:\n")
		fmt.Printf("  Share ID:   %s\n", updated.ShareID)
		fmt.Printf("  New Expiry: %s\n", updated.ExpiresAt.Format(time.RFC3339))
		return fmt.Errorf("manifest key resolution failed (share %s was extended): %w", updated.ShareID, err)
	}

	writer := manifest.NewWriter(factory.S3Client())
	if err := writer.Sync(ctx, manifestBucket, manifestKey, manifestDoc); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: share extended but manifest upload failed: %v\n", err)
		fmt.Printf("Share extended successfully:\n")
		fmt.Printf("  Share ID:   %s\n", updated.ShareID)
		fmt.Printf("  New Expiry: %s\n", updated.ExpiresAt.Format(time.RFC3339))
		return fmt.Errorf("manifest upload failed (share %s was extended): %w", updated.ShareID, err)
	}

	// --- Step 6: Output confirmation (Req 9.3) ---
	fmt.Printf("Share extended successfully:\n")
	fmt.Printf("  Share ID:   %s\n", updated.ShareID)
	fmt.Printf("  Account:    %s\n", updated.ResearcherAccountID)
	fmt.Printf("  Libraries:  %d (%s)\n", len(updated.LibraryIDs), strings.Join(updated.LibraryIDs, ", "))
	fmt.Printf("  New Expiry: %s\n", updated.ExpiresAt.Format(time.RFC3339))

	return nil
}
