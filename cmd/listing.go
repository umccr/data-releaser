package cmd

// This file renders the listing: one table covering both release mechanisms.
//
// The record types disagree with each other on purpose — an htsget share, a copy and a
// grant hold different fields, enforce expiry by different means, and move through
// different lifecycles — but an operator asking "what does account X have?" wants one
// answer, not three. So the records meet here and nowhere else, and what they meet as is
// a row of strings to print. There is no shared record type behind this, and nothing
// reads a row back: a projection that only ever goes one way cannot pull three lifecycles
// towards each other.

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"

	"github.com/umccr/data-releaser/internal/dataset"
	"github.com/umccr/data-releaser/internal/model"
)

// maxLibrariesDisplay is the number of library IDs shown before the rest are counted
// instead.
const maxLibrariesDisplay = 3

// absentValue stands in for a column a record has no value for, which is not the same as
// an empty one: a copy has no researcher, no VPC and no expiry at any point in its life,
// because who may read what it delivered is what a grant records and never what a copy
// does.
const absentValue = "-"

// listingRow is one line of the table.
//
// The fields are display strings rather than the values they came from, because the
// record kinds disagree about the underlying types (an expiry that may be absent, a state
// drawn from two different vocabularies, a VPC written two different ways) and the table
// only ever prints them.
type listingRow struct {
	mechanism string
	// id is the operand the extend and revoke commands take for this record.
	id      string
	account string
	// shared describes what the record is about: libraries for an htsget share, the
	// dataset URI for a copy or a grant of one.
	shared    string
	targetVpc string
	created   time.Time
	expiry    string
	state     string
}

// htsgetRows projects htsget share records onto the table's columns.
func htsgetRows(records []*model.ShareRecord, now time.Time) []listingRow {
	rows := make([]listingRow, 0, len(records))
	for _, r := range records {
		rows = append(rows, listingRow{
			mechanism: string(mechanismHtsget),
			id:        r.ShareID,
			account:   valueOrAbsent(r.ResearcherAccountID),
			shared:    valueOrAbsent(formatLibraries(r.LibraryIDs)),
			targetVpc: formatTargetVpc(r.TargetVpcArn),
			created:   r.CreatedAt,
			expiry:    formatExpiry(r.ExpiresAt),
			state:     string(r.EffectiveStatus(now)),
		})
	}
	return rows
}

// grantRows projects dataset grants onto the table's columns.
//
// Every grant is rendered whatever state it is in. A listing that showed only the ones
// currently giving access would answer "who can read data right now?", where the question
// being asked is "what has been granted?" — and a withdrawn grant is part of that answer.
func grantRows(grants []*dataset.Grant, now time.Time) []listingRow {
	rows := make([]listingRow, 0, len(grants))
	for _, g := range grants {
		rows = append(rows, listingRow{
			mechanism: string(mechanismS3),
			id:        g.GrantID,
			account:   valueOrAbsent(g.ResearcherAccountID),
			shared:    valueOrAbsent(g.DatasetURI),
			targetVpc: formatTargetVpc(g.TargetVpcID),
			created:   g.CreatedAt,
			expiry:    formatExpiry(g.ExpiresAt),
			state:     string(g.EffectiveState(now)),
		})
	}
	return rows
}

// copyRows projects dataset copies onto the table's columns.
//
// A copy fills three of them with the absent value and means it. It names no researcher
// and no VPC because who may read what it delivered is a grant's business, and it has no
// expiry for the same reason: rendering that absence as unlimited would read as unlimited
// access to a dataset nobody has been given at all. The state column carries the copy's
// own vocabulary — what became of the transfer — which is a different question from
// whether anyone can read, and the two are deliberately not folded into one word.
//
// Every copy is rendered whatever state it is in, the failed and the purged included: the
// question is what has been transferred into this account, not what is readable now. No
// clock is needed for it, since a copy's state is recorded rather than derived from an
// expiry.
func copyRows(copies []*dataset.Copy) []listingRow {
	rows := make([]listingRow, 0, len(copies))
	for _, c := range copies {
		rows = append(rows, listingRow{
			mechanism: string(mechanismS3),
			id:        c.CopyID,
			account:   absentValue,
			shared:    valueOrAbsent(c.DatasetURI),
			targetVpc: absentValue,
			created:   c.CreatedAt,
			expiry:    absentValue,
			state:     string(c.State),
		})
	}
	return rows
}

// sortRows orders the merged rows the way each store already returns its own records:
// newest first, tie-broken on the identifier so two records created in the same second
// keep a stable order between runs rather than appearing to move.
func sortRows(rows []listingRow) {
	slices.SortFunc(rows, func(a, b listingRow) int {
		if order := b.created.Compare(a.created); order != 0 {
			return order
		}
		return strings.Compare(b.id, a.id)
	})
}

// renderListing writes the table to w. Rows are printed in the order given.
func renderListing(w io.Writer, rows []listingRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MECHANISM\tID\tACCOUNT\tSHARED\tTARGET VPC\tCREATED\tEXPIRY\tSTATE")

	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.mechanism,
			row.id,
			row.account,
			row.shared,
			row.targetVpc,
			row.created.Format(time.RFC3339),
			row.expiry,
			row.state,
		)
	}

	tw.Flush()
}

// formatTargetVpc renders a researcher's VPC as the bare identifier, whichever form the
// record stored it in.
//
// The two mechanisms store it differently for good reason: an htsget share holds the full
// EC2 VPC ARN, because that is what Lattice reports as the source of an invocation and
// what the authorization callout matches on, while a dataset grant holds the bare "vpc-…"
// that the bucket policy condition compares against. Neither wants converting in
// storage. The column, though, exists to be scanned down and compared across mechanisms,
// and the region and account an ARN carries are already known from the record's own
// account and the deployment's region.
//
// A value that is not a VPC ARN prints as it was stored rather than being blanked or
// trimmed, since a VPC written in the wrong shape is precisely the thing a listing should
// let an operator notice.
func formatTargetVpc(stored string) string {
	if strings.TrimSpace(stored) == "" {
		return absentValue
	}
	parsed, err := arn.Parse(stored)
	if err != nil {
		return stored
	}
	_, id, found := strings.Cut(parsed.Resource, "/")
	if !found || id == "" {
		return stored
	}
	return id
}

// formatLibraries joins library IDs with commas, counting the rest once there are more
// than the column can usefully show.
func formatLibraries(ids []string) string {
	if len(ids) <= maxLibrariesDisplay {
		return strings.Join(ids, ",")
	}
	displayed := strings.Join(ids[:maxLibrariesDisplay], ",")
	return fmt.Sprintf("%s (+%d more)", displayed, len(ids)-maxLibrariesDisplay)
}

// formatExpiry formats an expiry timestamp, naming the absence of one as unlimited.
func formatExpiry(expiresAt *time.Time) string {
	if expiresAt == nil {
		return "unlimited"
	}
	return expiresAt.Format(time.RFC3339)
}

// valueOrAbsent keeps an empty column from reading as a blank the eye skips over.
func valueOrAbsent(value string) string {
	if value == "" {
		return absentValue
	}
	return value
}
