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
		STSClient:    factory.STSClient(),
		AthenaClient: factory.AthenaClient(),
		S3Client:     factory.S3Client(),
		HTTPClient:   httpClient,
		Config:       cfg,
	}

	checks := validate.ValidateProvider(ctx, opts)

	// Display provider-side results
	fmt.Println("Provider-side validation results:")
	fmt.Println("─────────────────────────────────")

	hasFailure := false
	for _, c := range checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
			hasFailure = true
		}
		fmt.Printf("  [%s] %s\n", status, c.Name)
		if c.Error != "" {
			fmt.Printf("         %s\n", c.Error)
		}
	}

	// Run researcher-side checks if --account is provided
	accountID, _ := cmd.Flags().GetString("account")
	if accountID != "" {
		fmt.Println()
		fmt.Printf("Researcher-side validation results (account %s):\n", accountID)
		fmt.Println("─────────────────────────────────────────────────────")

		researcherOpts := validate.ValidateResearcherOpts{
			RAMClient:    factory.RAMClient(),
			LambdaClient: factory.LambdaClient(),
			AccountID:    accountID,
		}

		researcherChecks := validate.ValidateResearcher(ctx, researcherOpts)
		checks = append(checks, researcherChecks...)

		for _, c := range researcherChecks {
			status := "PASS"
			if !c.Passed {
				status = "FAIL"
				hasFailure = true
			}
			fmt.Printf("  [%s] %s\n", status, c.Name)
			if c.Error != "" {
				fmt.Printf("         %s\n", c.Error)
			}
		}
	}

	// Run canary test-setup mode if --test-setup is set (Req 15.6, 15.8)
	testSetup, _ := cmd.Flags().GetBool("test-setup")
	if testSetup {
		fmt.Println()
		fmt.Println("Canary test-setup mode:")
		fmt.Println("───────────────────────")

		store := auth.NewShareStore(factory.DynamoDBClient(), ShareRecordsTableName)
		canaryOpts := validate.CanaryOpts{
			ShareStore:   store,
			LambdaClient: factory.LambdaClient(),
			Config:       cfg.Validation.Canary,
		}
		canaryCheck := validate.RunCanaryTest(ctx, canaryOpts)
		checks = append(checks, canaryCheck)

		status := "PASS"
		if !canaryCheck.Passed {
			status = "FAIL"
			hasFailure = true
		}
		fmt.Printf("  [%s] %s\n", status, canaryCheck.Name)
		if canaryCheck.Error != "" {
			fmt.Printf("         %s\n", canaryCheck.Error)
		}
	}

	fmt.Println()
	passed := 0
	for _, c := range checks {
		if c.Passed {
			passed++
		}
	}
	fmt.Printf("Summary: %d/%d checks passed\n", passed, len(checks))

	if hasFailure {
		return fmt.Errorf("one or more validation checks failed")
	}

	return nil
}
