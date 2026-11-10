package cmd

// Validation asks both mechanisms the same question in two different ways: is what this
// tool believes about access still true?
//
// For the htsget mechanism the answer is mostly about reachability, because the record
// itself is the enforcement point -- the callout reads it per request, so the record and
// reality cannot drift apart. For the direct-read mechanism they can, and routinely
// could: access there is two policy documents that AWS evaluates per request and that a
// person can edit in a console. So its central check is a comparison, and reconciliation
// is the one thing here that writes.
//
// Naming no mechanism covers both, since a read-only question narrowed by accident hides
// access rather than granting it. Naming one narrows the checks, and only then is that
// mechanism's configuration required outright: a bare run reports an unconfigured
// mechanism as a failed check and carries on with the other, which is the same posture
// every other check here takes towards failure.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
	"github.com/umccr/data-releaser/internal/validate"
)

var validateCmd = &cobra.Command{
	Use:   "validate [mechanism]",
	Short: "Validate the deployed sharing infrastructure",
	Long: `Run preflight and reconciliation checks on the sharing infrastructure.
Reports per-check pass/fail diagnostics without creating or modifying any
persistent record, unless --test-setup or --reconcile is given.

Validation is read-only, so naming no mechanism checks all of them. Naming one
(s3 or htsget) narrows the checks to that mechanism.

For the s3 mechanism the central check is a comparison: access there is enforced
by the cache bucket's policy and the policy of the key encrypting it, either of
which can be edited outside this tool, so each release is compared against what
is actually deployed. --reconcile replaces the deployed documents of every
release that differs with the ones the ledger describes.

Every check runs whatever the ones before it found, and any failure exits
non-zero.`,
	Args: optionalMechanismArg,
	RunE: runValidate,
}

func init() {
	validateCmd.Flags().String("account", "", "Restrict the checks to one researcher AWS account")
	validateCmd.Flags().Bool("test-setup", false, "Run canary test-setup mode (creates and cleans up a synthetic share)")
	validateCmd.Flags().Bool("reconcile", false,
		"Replace the deployed bucket and key policies of every s3 release whose access differs "+
			"from the ledger's. This writes: it is never implied by a plain validation run")

	rootCmd.AddCommand(validateCmd)
}

// printCheck renders one check result to w and reports whether it was a failure.
// A skipped check renders as SKIP and is never a failure.
func printCheck(w io.Writer, c validate.Check) bool {
	status := "PASS"
	failed := false
	switch {
	case c.Skipped:
		status = "SKIP"
	case !c.Passed:
		status = "FAIL"
		failed = true
	}

	fmt.Fprintf(w, "  [%s] %s\n", status, c.Name)
	// Indented as a block rather than only on its first line: a check reporting several
	// problems at once -- a section of configuration with four fields missing, say --
	// otherwise runs its continuation lines back to the margin and reads as though the
	// listing had ended.
	for line := range strings.SplitSeq(strings.TrimRight(c.Error, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(w, "         %s\n", strings.TrimSpace(line))
		}
	}
	return failed
}

// printSection prints a heading and its results, and reports whether any of them failed.
func printSection(w io.Writer, heading string, checks []validate.Check) bool {
	fmt.Fprintln(w, heading)
	fmt.Fprintln(w, underline(heading))

	failed := false
	for _, c := range checks {
		if printCheck(w, c) {
			failed = true
		}
	}
	return failed
}

// underline draws a rule the width of the heading above it.
func underline(heading string) string {
	rule := make([]rune, 0, len([]rune(heading)))
	for range heading {
		rule = append(rule, '─')
	}
	return string(rule)
}

func runValidate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	filter, err := newMechanismFilter(args)
	if err != nil {
		return err
	}

	accountID, _ := cmd.Flags().GetString("account")
	testSetup, _ := cmd.Flags().GetBool("test-setup")
	reconcile, _ := cmd.Flags().GetBool("reconcile")

	// Both flags name work belonging to one mechanism. Accepting them against the other
	// would report on something that never ran.
	if reconcile && !filter.covers(mechanismS3) {
		return errors.New("--reconcile applies to the s3 mechanism, whose deployed policies are what it re-applies")
	}
	if testSetup && !filter.covers(mechanismHtsget) {
		return errors.New("--test-setup applies to the htsget mechanism, whose authorization callout it exercises")
	}

	out := cmd.OutOrStdout()
	var results []validate.Check
	failed := false

	if filter.covers(mechanismHtsget) {
		checks, err := htsgetChecks(ctx, accountID, testSetup, filter.named == mechanismHtsget)
		if err != nil {
			return err
		}
		results = append(results, checks...)
		if printSection(out, "htsget mechanism:", checks) {
			failed = true
		}
	}

	if filter.covers(mechanismS3) {
		if len(results) > 0 {
			fmt.Fprintln(out)
		}
		checks, err := datasetChecks(ctx, cmd, accountID, reconcile, filter.named == mechanismS3)
		if err != nil {
			return err
		}
		results = append(results, checks...)
		if printSection(out, "s3 mechanism:", checks) {
			failed = true
		}
	}

	fmt.Fprintln(out)
	reportCheckSummary(out, results)

	if failed {
		return errors.New("one or more validation checks failed")
	}
	return nil
}

// reportCheckSummary counts what ran. Skipped checks are excluded from the denominator:
// reporting them as part of the total would make a fully healthy deployment look
// partially broken.
func reportCheckSummary(w io.Writer, results []validate.Check) {
	passed, skipped := 0, 0
	for _, c := range results {
		switch {
		case c.Skipped:
			skipped++
		case c.Passed:
			passed++
		}
	}

	fmt.Fprintf(w, "Summary: %d/%d checks passed", passed, len(results)-skipped)
	if skipped > 0 {
		fmt.Fprintf(w, " (%d skipped)", skipped)
	}
	fmt.Fprintln(w)
}

// unconfiguredMechanism turns a mechanism's missing configuration into either a refusal or
// a failed check, depending on whether the operator asked for that mechanism by name.
//
// Asked for by name, there is nothing to report and saying so plainly is the whole answer.
// In a bare run the other mechanism still has checks worth running, and an unconfigured
// mechanism is itself a finding -- so it is reported as one and the run continues, which
// is what every other failure here does.
func unconfiguredMechanism(named bool, name string, err error) ([]validate.Check, error) {
	if named {
		return nil, err
	}
	return []validate.Check{{Name: name + " configuration", Error: err.Error()}}, nil
}

// htsgetChecks runs the checks belonging to the mechanism that serves reads through the
// authorization callout.
func htsgetChecks(
	ctx context.Context,
	accountID string,
	testSetup bool,
	named bool,
) ([]validate.Check, error) {
	// Every check below reads one of these sections, and a check run against an
	// unconfigured field reports the deployment as broken when it is the config file that
	// is incomplete. Naming the missing fields instead — all of them, in one run — is the
	// more useful answer to "why does validation fail".
	cfg, err := requireConfig(
		config.SectionWarehouse,
		config.SectionLedger,
		config.SectionManifest,
		config.SectionAuthorizationEndpoint,
		config.SectionHtsgetEndpoint,
		config.SectionResearchers,
	)
	if err != nil {
		return unconfiguredMechanism(named, "htsget", err)
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	checks := validate.ValidateProvider(ctx, validate.ValidateProviderOpts{
		STSClient:             factory.STSClient(),
		AthenaClient:          factory.AthenaClient(),
		S3Client:              factory.S3Client(),
		DynamoDBClient:        factory.DynamoDBClient(),
		APIGatewayClient:      factory.APIGatewayClient(),
		HTTPClient:            httpClient,
		Config:                cfg,
		ShareRecordsTableName: cfg.Ledger.Table,
	})

	if accountID != "" {
		researcherOpts := validate.ValidateResearcherOpts{
			RAMClient:      factory.RAMClient(),
			LambdaClient:   factory.LambdaClient(),
			S3Client:       factory.S3Client(),
			AccountID:      accountID,
			ManifestBucket: cfg.Manifest.Bucket,
		}
		// Resolve the account's manifest prefix for the cross-account read-grant check.
		if prefix, prefixErr := cfg.Manifest.RenderManifestPrefix(accountID); prefixErr == nil {
			researcherOpts.ManifestPrefix = prefix
		}
		checks = append(checks, validate.ValidateResearcher(ctx, researcherOpts)...)
	}

	if testSetup {
		store := auth.NewShareStore(factory.DynamoDBClient(), cfg.Ledger.Table)
		canaryOpts := validate.CanaryOpts{
			ShareStore:   store,
			LambdaClient: factory.LambdaClient(),
			Config:       cfg.Validation.Canary,
		}
		// The synthetic share is keyed on the canary account's configured VPC ARN, the
		// same value a real share for that account would use. A resolution failure is
		// left to RunCanaryTest to report as a failed check rather than aborting the
		// command, so the other results still print.
		if cfg.Validation.Canary != nil {
			canaryOpts.TargetVpcArn, _ = cfg.ResolveTargetVpcArn(cfg.Validation.Canary.Account)
		}
		checks = append(checks, validate.RunCanaryTest(ctx, canaryOpts))
	}

	return checks, nil
}

// datasetChecks runs the checks belonging to the mechanism that grants a researcher direct
// reads of a copy held here: the prerequisites a copy depends on, the comparison of each
// release against its deployed documents, and the state of every key a dataset is
// encrypted under.
func datasetChecks(
	ctx context.Context,
	cmd *cobra.Command,
	accountID string,
	reconcile bool,
	named bool,
) ([]validate.Check, error) {
	// The policy settings are named because the documents each release is compared
	// against are rendered rather than described: a comparison made with settings other
	// than the ones the deployed documents were written with would report a difference in
	// everything.
	cfg, err := requireConfig(
		config.SectionAccount,
		config.SectionLedger,
		config.SectionReleasePolicies,
		config.SectionCopyService,
		config.SectionSourceStore,
	)
	if err != nil {
		return unconfiguredMechanism(named, "s3", err)
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}

	validation := validate.ValidateDataset(ctx, validate.ValidateDatasetOpts{
		Copies:              dataset.NewCopyStore(factory.DynamoDBClient(), cfg.Ledger.Table),
		Grants:              dataset.NewGrantStore(factory.DynamoDBClient(), cfg.Ledger.Table),
		LedgerTable:         cfg.Ledger.Table,
		S3Client:            factory.S3Client(),
		KMSClient:           factory.KMSClient(),
		StepFunctionsClient: factory.StepFunctionsClient(),
		PolicySettings:      releasePolicySettings(cfg),
		Copy: validate.DatasetCopyTargets{
			StateMachineARN:     cfg.S3.Copy.StateMachineArn,
			AccountID:           cfg.S3.Copy.Account,
			WorkingBucket:       cfg.S3.Copy.WorkingBucket,
			WorkingBucketPrefix: cfg.S3.Copy.WorkingBucketPrefix,
		},
		Source: validate.DatasetSourceStore{
			EndpointURL:       cfg.S3.Source.EndpointURL,
			CredentialsSecret: cfg.S3.Source.CredentialsSecret,
		},
		ResearcherAccountID: accountID,
		Reconcile:           reconcile,
	})

	// The repairs are printed apart from the observations, because each of them wrote to
	// a bucket or a key policy and an operator reading the output should not have to work
	// out which of the two a line is.
	if len(validation.Repairs) > 0 {
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "s3 mechanism, policies re-applied from the ledger:")
		fmt.Fprintln(out, underline("s3 mechanism, policies re-applied from the ledger:"))
		for _, repair := range validation.Repairs {
			printCheck(out, repair)
		}
		fmt.Fprintln(out)
	}

	return validation.Results(), nil
}
