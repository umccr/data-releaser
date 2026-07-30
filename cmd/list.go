package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/model"
)

// maxLibrariesDisplay is the maximum number of library IDs to show before truncating.
const maxLibrariesDisplay = 3

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List active data shares",
	Long: `Query the Authorization Service for all Share_Records and display
them in a tabular format, optionally filtered by researcher account.`,
	RunE: runList,
}

func init() {
	listCmd.Flags().String("account", "", "Filter by researcher AWS account ID")

	rootCmd.AddCommand(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Get the account filter flag.
	account, _ := cmd.Flags().GetString("account")

	// Create AWS client factory using loaded config.
	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// Create the share store.
	store := auth.NewShareStore(factory.DynamoDBClient(), ShareRecordsTableName)

	// Query shares.
	var records []*model.ShareRecord
	if account != "" {
		records, err = store.ListByAccount(ctx, account)
	} else {
		records, err = store.ListAll(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to query shares: %v\n", err)
		return err
	}

	// Handle empty results (Req 8.4, 8.6).
	if len(records) == 0 {
		if account != "" {
			fmt.Printf("No shares found for account %s.\n", account)
		} else {
			fmt.Println("No shares found.")
		}
		return nil
	}

	// Display tabular output (Req 8.2). All records are rendered regardless of
	// effective status, so expired and revoked shares remain visible (Req 8.1).
	renderShareTable(os.Stdout, records, time.Now().UTC())
	return nil
}

// renderShareTable writes the tabular share listing to w. Every record is rendered,
// including expired and revoked shares (they must remain visible per Req 8.1), with the
// effective status computed against now. Records are shown in the order provided (the
// store returns them ordered by createdAt descending, Req 8.5).
func renderShareTable(w io.Writer, records []*model.ShareRecord, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SHARE ID\tACCOUNT\tLIBRARIES\tCREATED\tEXPIRY\tSTATUS")

	for _, r := range records {
		libraries := formatLibraries(r.LibraryIDs)
		created := r.CreatedAt.Format(time.RFC3339)
		expiry := formatExpiry(r.ExpiresAt)
		status := string(r.EffectiveStatus(now))

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ShareID,
			r.ResearcherAccountID,
			libraries,
			created,
			expiry,
			status,
		)
	}

	tw.Flush()
}

// formatLibraries joins library IDs with commas, truncating if there are too many.
func formatLibraries(ids []string) string {
	if len(ids) <= maxLibrariesDisplay {
		return strings.Join(ids, ",")
	}
	displayed := strings.Join(ids[:maxLibrariesDisplay], ",")
	return fmt.Sprintf("%s (+%d more)", displayed, len(ids)-maxLibrariesDisplay)
}

// formatExpiry formats the expiry timestamp, returning "unlimited" if nil.
func formatExpiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "unlimited"
	}
	return expiresAt.Format(time.RFC3339)
}
