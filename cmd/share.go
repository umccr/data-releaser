package cmd

import (
	"context"
	"fmt"
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

// dayPattern matches a whole-day duration such as "30d", the shorthand Go's own
// duration parser does not accept.
var dayPattern = regexp.MustCompile(`^(\d+)d$`)

// parseDuration parses a duration string that supports day suffixes (e.g. "30d", "7d")
// as well as standard Go duration strings (e.g. "24h", "720h").
// Returns the parsed duration or an error if the format is invalid.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

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
	if err := model.ValidateTargetVpcArn(flagValue, accountID); err != nil {
		return "", fmt.Errorf("--target-vpc-arn: %w", err)
	}
	return flagValue, nil
}

// shareCmd is the verb; the mechanism below it decides how access is enforced. It
// carries no flags of its own, because the mechanisms do not take the same inputs.
var shareCmd = &cobra.Command{
	Use:   "share <mechanism>",
	Short: "Grant a researcher account access to data",
	Long: `Grant a researcher account access to data.

The mechanism is named as part of the invocation and is never inferred from the
flags given: each mechanism takes its own inputs and enforces access by its own
means. The available mechanisms are listed below.`,
}

var shareHtsgetCmd = &cobra.Command{
	Use:   "htsget",
	Short: "Share libraries with a researcher account through htsget-rs",
	Long: `Resolve library IDs to S3 locations, create a share record in the
Authorization Service, and deliver a manifest to the researcher account.`,
	RunE: runShare,
}

func init() {
	shareHtsgetCmd.Flags().StringSlice("libraries", nil, "Comma-separated list of library IDs to share")
	shareHtsgetCmd.Flags().String("account", "", "Target researcher AWS account ID")
	shareHtsgetCmd.Flags().String("expiry", "", "Optional expiry duration (e.g. 30d, 24h)")
	shareHtsgetCmd.Flags().String("target-vpc-arn", "",
		"ARN of the researcher VPC associated with the htsget VPC Lattice service network "+
			"(e.g. arn:aws:ec2:ap-southeast-2:123456789012:vpc/vpc-0123456789abcdef0). "+
			"Overrides researchers.<account>.targetVpcArn from the config file.")

	_ = shareHtsgetCmd.MarkFlagRequired("libraries")
	_ = shareHtsgetCmd.MarkFlagRequired("account")

	shareCmd.AddCommand(shareHtsgetCmd)
	requireMechanism(shareCmd)
	rootCmd.AddCommand(shareCmd)
}

func runShare(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Everything checkable is checked before anything is loaded or called.
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

	// Validate: reject empty library list
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

	// Validate: reject missing account
	account = strings.TrimSpace(account)
	if account == "" {
		return fmt.Errorf("researcher account ID is required (--account must not be empty)")
	}

	// Validate and parse expiry
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

	// The config and the AWS clients it names.
	// Resolving libraries needs the warehouse, the manifest needs a bucket and the
	// htsget base URL, and the researcher's VPC ARN comes from the researchers section
	// unless the flag supplied one.
	cfg, err := requireConfig(
		config.SectionWarehouse,
		config.SectionLedger,
		config.SectionManifest,
		config.SectionHtsgetEndpoint,
		config.SectionResearchers,
	)
	if err != nil {
		return err
	}

	// Resolve the target VPC ARN before doing any work. It is pure input validation and
	// failing here avoids a partially-completed share whose authorization can never match.
	targetVpcArn, err := resolveTargetVpcArn(cfg, targetVpcArnFlag, account)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		return err
	}

	// The connectivity the authorization callout depends on, checked before anything is written.
	checker := lattice.NewChecker(factory.RAMClient())
	if err := lattice.Preflight(ctx, checker, account); err != nil {
		return err
	}

	// The manifest bucket is checked for existence and writability before any share
	// record is created, which removes the window where a share is persisted and only
	// then fails on the manifest upload.
	writer := manifest.NewWriter(factory.S3Client())
	target, err := resolveManifestTarget(cfg, account)
	if err != nil {
		return err
	}
	if err := writer.VerifyBucketWritable(ctx, target.bucket); err != nil {
		return fmt.Errorf("manifest bucket %q is missing or not writable by the Data Control identity; "+
			"the infrastructure stack is not deployed or is misconfigured: %w", target.bucket, err)
	}

	// The libraries, resolved to the locations they actually live in.
	whClient := warehouse.NewClient(factory.AthenaClient(), cfg.Warehouse)
	resolved, err := whClient.ResolveLibraries(ctx, cleanLibraries)
	if err != nil {
		return fmt.Errorf("failed to resolve libraries: %w", err)
	}

	// A library that resolved to nothing is a share that would grant nothing.
	if err := warehouse.ValidateResolution(cleanLibraries, resolved); err != nil {
		return err
	}

	// The prefixes the share record carries.
	// The prefixes are what authorization is granted on. The per-file list is recorded
	// alongside them purely so the manifest can name each file's htsget request path.
	prefixes := warehouse.ExtractPrefixes(resolved)
	files := warehouse.ExtractFiles(resolved)

	// The record, created once however many times the same share is asked for.
	store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Ledger.Table)

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
		return fmt.Errorf("failed to create share record: %w", err)
	}

	// The researcher's own description of the access, rebuilt and delivered.
	if err := regenerateManifest(ctx, cfg, writer, store, account, target); err != nil {
		return fmt.Errorf("manifest upload failed (share %s was created): %w", record.ShareID, err)
	}

	// What was shared, from the record as it now stands.
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
