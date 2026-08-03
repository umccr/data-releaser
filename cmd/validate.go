package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/validate"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the deployed sharing infrastructure",
	Long: `Run preflight and end-to-end checks on the sharing infrastructure.
Reports per-check pass/fail diagnostics without creating or modifying
any persistent Share_Record (unless --test-setup is specified).`,
	RunE: runValidate,
}

func init() {
	validateCmd.Flags().String("account", "", "Target researcher AWS account ID for researcher-side checks")
	validateCmd.Flags().Bool("test-setup", false, "Run canary test-setup mode (creates and cleans up a synthetic share)")

	rootCmd.AddCommand(validateCmd)
}

// printCheck renders one check result and reports whether it was a failure.
// A skipped check renders as SKIP and is never a failure.
func printCheck(c validate.Check) bool {
	status := "PASS"
	failed := false
	switch {
	case c.Skipped:
		status = "SKIP"
	case !c.Passed:
		status = "FAIL"
		failed = true
	}

	fmt.Printf("  [%s] %s\n", status, c.Name)
	if c.Error != "" {
		fmt.Printf("         %s\n", c.Error)
	}
	return failed
}

func runValidate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// Run provider-side checks
	httpClient := &http.Client{Timeout: 10 * time.Second}

	opts := validate.ValidateProviderOpts{
		STSClient:             factory.STSClient(),
		AthenaClient:          factory.AthenaClient(),
		S3Client:              factory.S3Client(),
		DynamoDBClient:        factory.DynamoDBClient(),
		APIGatewayClient:      factory.APIGatewayClient(),
		HTTPClient:            httpClient,
		Config:                cfg,
		ShareRecordsTableName: shareRecordsTable(),
	}

	checks := validate.ValidateProvider(ctx, opts)

	// Display provider-side results
	fmt.Println("Provider-side validation results:")
	fmt.Println("─────────────────────────────────")

	hasFailure := false
	for _, c := range checks {
		if printCheck(c) {
			hasFailure = true
		}
	}

	// Run researcher-side checks if --account is provided
	accountID, _ := cmd.Flags().GetString("account")
	if accountID != "" {
		fmt.Println()
		fmt.Printf("Researcher-side validation results (account %s):\n", accountID)
		fmt.Println("─────────────────────────────────────────────────────")

		researcherOpts := validate.ValidateResearcherOpts{
			RAMClient:      factory.RAMClient(),
			LambdaClient:   factory.LambdaClient(),
			S3Client:       factory.S3Client(),
			AccountID:      accountID,
			ManifestBucket: cfg.Manifest.Bucket,
		}
		// Resolve the account's manifest prefix for the cross-account read-grant check (Req 15.9).
		if prefix, prefixErr := cfg.Manifest.RenderManifestPrefix(accountID); prefixErr == nil {
			researcherOpts.ManifestPrefix = prefix
		}

		researcherChecks := validate.ValidateResearcher(ctx, researcherOpts)
		checks = append(checks, researcherChecks...)

		for _, c := range researcherChecks {
			if printCheck(c) {
				hasFailure = true
			}
		}
	}

	// Run canary test-setup mode if --test-setup is set (Req 15.6, 15.8)
	testSetup, _ := cmd.Flags().GetBool("test-setup")
	if testSetup {
		fmt.Println()
		fmt.Println("Canary test-setup mode:")
		fmt.Println("───────────────────────")

		store := auth.NewShareStore(factory.DynamoDBClient(), shareRecordsTable())
		canaryOpts := validate.CanaryOpts{
			ShareStore:   store,
			LambdaClient: factory.LambdaClient(),
			Config:       cfg.Validation.Canary,
		}
		canaryCheck := validate.RunCanaryTest(ctx, canaryOpts)
		checks = append(checks, canaryCheck)

		if printCheck(canaryCheck) {
			hasFailure = true
		}
	}

	fmt.Println()
	passed, skipped := 0, 0
	for _, c := range checks {
		switch {
		case c.Skipped:
			skipped++
		case c.Passed:
			passed++
		}
	}
	// Skipped checks are excluded from the denominator: reporting them as part of the
	// total would make a fully healthy deployment look partially broken.
	fmt.Printf("Summary: %d/%d checks passed", passed, len(checks)-skipped)
	if skipped > 0 {
		fmt.Printf(" (%d skipped)", skipped)
	}
	fmt.Println()

	if hasFailure {
		return fmt.Errorf("one or more validation checks failed")
	}

	return nil
}
