package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
	"github.com/umccr/data-releaser/internal/model"
)

var listCmd = &cobra.Command{
	Use:   "list [mechanism]",
	Short: "List data shares across both release mechanisms",
	Long: `Report what is shared with whom, optionally filtered by researcher account.

Listing is read-only, so naming no mechanism covers all of them. Naming one
(s3 or htsget) narrows the report to that mechanism.

Every record is reported whatever state it is in, including revoked, expired,
and purged ones: the question is what has been released, not only what is
readable at this moment. For the s3 mechanism that means both the copies of a
dataset held here and the grants of access to them, since a copy nobody has been
given is still something this account holds.`,
	Args: optionalMechanismArg,
	RunE: runList,
}

func init() {
	listCmd.Flags().String("account", "", "Filter by researcher AWS account ID")

	rootCmd.AddCommand(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	filter, err := newMechanismFilter(args)
	if err != nil {
		return err
	}

	account, _ := cmd.Flags().GetString("account")

	// Reading the ledger is all this needs. Nothing about how either mechanism enforces
	// access is consulted to report what has been granted — which is also why the
	// listing cannot be trusted as proof that the deployed policies agree with it. That
	// is what reconciliation is for.
	cfg, err := requireConfig(config.SectionLedger)
	if err != nil {
		return err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		return err
	}
	client := factory.DynamoDBClient()

	// One clock for both mechanisms, so that a record on either side of the expiry
	// boundary is not reported as expired in one half of the table and active in the
	// other.
	now := time.Now().UTC()

	var rows []listingRow

	if filter.covers(mechanismHtsget) {
		shares, err := listShares(ctx, auth.NewShareStore(client, cfg.Ledger.Table), account)
		if err != nil {
			return fmt.Errorf("failed to query htsget shares: %w", err)
		}
		rows = append(rows, htsgetRows(shares, now)...)
	}

	if filter.covers(mechanismS3) {
		grants, err := listGrants(ctx, dataset.NewGrantStore(client, cfg.Ledger.Table), account)
		if err != nil {
			return fmt.Errorf("failed to query dataset grants: %w", err)
		}
		rows = append(rows, grantRows(grants, now)...)

		// A copy names no researcher, so it belongs in an unnarrowed listing and in no
		// account's: filtering by account asks what that account holds, and nobody holds
		// a copy. Leaving copies out of a narrowed listing is the same judgement the
		// grant query makes for itself by being keyed on the researcher.
		if account == "" {
			copies, err := dataset.NewCopyStore(client, cfg.Ledger.Table).ListAll(ctx)
			if err != nil {
				return fmt.Errorf("failed to query dataset copies: %w", err)
			}
			rows = append(rows, copyRows(copies)...)
		}
	}

	out := cmd.OutOrStdout()

	if len(rows) == 0 {
		fmt.Fprintln(out, emptyListingMessage(filter, account))
		return nil
	}

	// Each store returns its own records newest first; merging two of them needs the
	// order imposed again.
	sortRows(rows)
	renderListing(out, rows)
	return nil
}

// listShares reads the htsget records, narrowed to one researcher account when one was
// named.
func listShares(ctx context.Context, store *auth.ShareStore, account string) ([]*model.ShareRecord, error) {
	if account != "" {
		return store.ListByAccount(ctx, account)
	}
	return store.ListAll(ctx)
}

// listGrants reads the dataset grants, narrowed to one researcher account when one was
// named.
func listGrants(ctx context.Context, store *dataset.GrantStore, account string) ([]*dataset.Grant, error) {
	if account != "" {
		return store.ListByAccount(ctx, account)
	}
	return store.ListAll(ctx)
}

// emptyListingMessage says what was looked for when nothing came back, so that an empty
// result cannot be read as a broader question than the one that was actually asked.
func emptyListingMessage(filter mechanismFilter, account string) string {
	subject := "shares, copies or grants"
	switch {
	case !filter.covers(mechanismS3):
		subject = "htsget shares"
	case !filter.covers(mechanismHtsget):
		subject = "s3 copies or grants"
	}
	if account != "" {
		return fmt.Sprintf("No %s found for account %s.", subject, account)
	}
	return fmt.Sprintf("No %s found.", subject)
}
