package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/lattice"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/warehouse"
)

// parseDuration parses a duration string that supports day suffixes (e.g. "30d", "7d")
// as well as standard Go duration strings (e.g. "24h", "720h").
// Returns the parsed duration or an error if the format is invalid.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	// Check for day suffix pattern (e.g. "30d", "7d")
	dayPattern := regexp.MustCompile(`^(\d+)d$`)
	if matches := dayPattern.FindStringSubmatch(s); matches != nil {
		days, err := strconv.Atoi(matches[1])
		if err != nil {
			return 0, fmt.Errorf("invalid day count in duration %q: %w", s, err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}

	// Fall back to standard Go duration parsing (e.g. "24h", "720h", "30m")
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: must be a Go duration (e.g. 24h, 720h) or days (e.g. 30d): %w", s, err)
	}
	return d, nil
}

// resolveTargetVpcArn determines the VPC ARN the share is keyed on, preferring an
// explicit --target-vpc-arn flag over the researcher's config entry.
//
// There is deliberately no fallback. The authorization callout looks this value up by
// exact match on a DynamoDB GSI against the VPC ARN htsget-rs forwards, so a derived
// or guessed ARN does not degrade gracefully — it writes a share that can never
// authorize anything, and the failure only surfaces later as an unexplained deny.
func resolveTargetVpcArn(cfg *config.Config, flagValue, accountID string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" {
		return cfg.ResolveTargetVpcArn(accountID)
	}
	if err := config.ValidateTargetVpcArn(flagValue, accountID); err != nil {
		return "", fmt.Errorf("--target-vpc-arn: %w", err)
	}
	return flagValue, nil
}

var shareCmd = &cobra.Command{
	Use:   "share",
	Short: "Share libraries with a researcher account",
	Long: `Resolve library IDs to S3 locations, create a Share_Record in the
Authorization Service, and deliver a manifest to the researcher account.`,
	RunE: runShare,
}

func init() {
	shareCmd.Flags().StringSlice("libraries", nil, "Comma-separated list of library IDs to share")
	shareCmd.Flags().String("account", "", "Target researcher AWS account ID")
	shareCmd.Flags().String("expiry", "", "Optional expiry duration (e.g. 30d, 24h)")
	shareCmd.Flags().String("target-vpc-arn", "",
		"ARN of the researcher VPC associated with the htsget VPC Lattice service network "+
			"(e.g. arn:aws:ec2:ap-southeast-2:123456789012:vpc/vpc-0123456789abcdef0). "+
			"Overrides researchers.<account>.targetVpcArn from the config file.")

	_ = shareCmd.MarkFlagRequired("libraries")
	_ = shareCmd.MarkFlagRequired("account")

	rootCmd.AddCommand(shareCmd)
}

func runShare(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// --- Step 1: Parse and validate inputs ---
	libraries, err := cmd.Flags().GetStringSlice("libraries")
	if err != nil {
		return fmt.Errorf("reading --libraries flag: %w", err)
	}
	account, err := cmd.Flags().GetString("account")
	if err != nil {
		return fmt.Errorf("reading --account flag: %w", err)
	}
	expiryStr, err := cmd.Flags().GetString("expiry")
	if err != nil {
		return fmt.Errorf("reading --expiry flag: %w", err)
	}
	targetVpcArnFlag, err := cmd.Flags().GetString("target-vpc-arn")
	if err != nil {
		return fmt.Errorf("reading --target-vpc-arn flag: %w", err)
	}

	// Validate: reject empty library list (Req 1.2)
	if len(libraries) == 0 {
		return fmt.Errorf("at least one library ID is required (--libraries must not be empty)")
	}

	// Trim whitespace from library IDs and filter empty strings
	var cleanLibraries []string
	for _, lib := range libraries {
		trimmed := strings.TrimSpace(lib)
		if trimmed != "" {
			cleanLibraries = append(cleanLibraries, trimmed)
		}
	}
	if len(cleanLibraries) == 0 {
		return fmt.Errorf("at least one library ID is required (--libraries must not be empty)")
	}

	// Validate: reject missing account (Req 1.2)
	account = strings.TrimSpace(account)
	if account == "" {
		return fmt.Errorf("researcher account ID is required (--account must not be empty)")
	}

	// Validate and parse expiry (Req 2.1, 2.2)
	var expiresAt *time.Time
	if expiryStr != "" {
		duration, err := parseDuration(expiryStr)
		if err != nil {
			return fmt.Errorf("invalid expiry: %w", err)
		}
		if duration < 24*time.Hour {
			return fmt.Errorf("expiry duration must be at least 1 day (24h), got %s", duration)
		}
		expiry := time.Now().UTC().Add(duration)
		expiresAt = &expiry
	}

	// --- Step 2: Load config and create AWS client factory ---
	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	// Resolve the target VPC ARN before doing any work. It is pure input validation and
	// failing here avoids a partially-completed share whose authorization can never match.
	targetVpcArn, err := resolveTargetVpcArn(cfg, targetVpcArnFlag, account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// --- Step 3: Run preflight - verify VPC Lattice/RAM connectivity (Req 5.4) ---
	checker := lattice.NewChecker(factory.RAMClient())
	if err := lattice.Preflight(ctx, checker, account); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// --- Step 3b: Preflight - verify the Manifest_Bucket exists and is writable by
	// the Data Control identity BEFORE any Share_Record is created (Req 7.10, 17.7, 1.8).
	// This removes the window where a share is persisted and only then fails on the
	// manifest upload.
	writer := manifest.NewWriter(factory.S3Client())
	manifestBucket := strings.TrimSpace(cfg.Manifest.Bucket)
	if manifestBucket == "" {
		cfgErr := fmt.Errorf("manifest.bucket is not configured; the Manifest_Bucket name must be sourced from the Infrastructure_Stack output")
		fmt.Fprintf(os.Stderr, "Error: %v\n", cfgErr)
		return cfgErr
	}
	// Resolve the per-account manifest key (also validates the key template).
	manifestKey, err := cfg.Manifest.RenderManifestKey(account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}
	if err := writer.VerifyBucketWritable(ctx, manifestBucket); err != nil {
		wrapped := fmt.Errorf("manifest bucket %q is missing or not writable by the Data Control identity; "+
			"the Infrastructure_Stack is not deployed or is misconfigured: %w", manifestBucket, err)
		fmt.Fprintf(os.Stderr, "Error: %v\n", wrapped)
		return wrapped
	}

	// --- Step 4: Resolve libraries via Athena (Req 1.3) ---
	whClient := warehouse.NewClient(factory.AthenaClient(), cfg.Warehouse)
	resolved, err := whClient.ResolveLibraries(ctx, cleanLibraries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to resolve libraries: %v\n", err)
		return err
	}

	// --- Step 5: Validate resolution (Req 1.6, 1.7) ---
	if err := warehouse.ValidateResolution(cleanLibraries, resolved); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// --- Step 6: Extract S3 prefixes (Req 1.4) ---
	// The prefixes are what authorization is granted on. The per-file list is recorded
	// alongside them purely so the manifest can name each file's htsget request path.
	prefixes := warehouse.ExtractPrefixes(resolved)
	files := warehouse.ExtractFiles(resolved)

	// --- Step 7: Idempotency check and create share record (Req 1.5, 12.1) ---
	store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Authorization.ShareRecordsTable)

	createInput := auth.CreateInput{
		ResearcherAccountID: account,
		TargetVpcArn:        targetVpcArn,
		LibraryIDs:          cleanLibraries,
		S3Prefixes:          prefixes,
		Files:               files,
		ExpiresAt:           expiresAt,
	}

	record, reused, err := store.CreateIdempotent(ctx, createInput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to create share record: %v\n", err)
		return err
	}

	// --- Step 8: Generate manifest (Req 7.1) ---
	// Query all active shares for this researcher to build a complete manifest
	allShares, err := store.ListByAccount(ctx, account)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to list shares for manifest generation: %v\n", err)
		return err
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

	// --- Step 9: Upload manifest (Req 7.2) ---
	if err := writer.Sync(ctx, manifestBucket, manifestKey, manifestDoc); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: share record created but manifest upload failed: %v\n", err)
		return fmt.Errorf("manifest upload failed (share %s was created): %w", record.ShareID, err)
	}

	// --- Step 10: Output confirmation (Req 1.9) ---
	if reused {
		fmt.Printf("Existing share reused (idempotent match):\n")
	} else {
		fmt.Printf("Share created successfully:\n")
	}
	fmt.Printf("  Share ID:  %s\n", record.ShareID)
	fmt.Printf("  Account:   %s\n", record.ResearcherAccountID)
	fmt.Printf("  Libraries: %d (%s)\n", len(record.LibraryIDs), strings.Join(record.LibraryIDs, ", "))
	if record.ExpiresAt != nil {
		fmt.Printf("  Expiry:    %s\n", record.ExpiresAt.Format(time.RFC3339))
	} else {
		fmt.Printf("  Expiry:    unlimited\n")
	}

	return nil
}
