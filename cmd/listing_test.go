package cmd

// These tests cover the one place the record types meet.
//
// They are kept apart on purpose -- an htsget share, a copy and a grant answer different
// questions -- and the listing is where an operator asking "what does account X have?"
// gets one answer rather than three. What that costs is a projection, and what the
// projection has to get right is the part a reader would never notice was wrong: a record
// present in the table but absent from the table on screen reads as access that does not
// exist, and a copy rendered as though it were a grant reads as access nobody was given.
// So the tests below are mostly about what must appear -- every state, every kind, and the
// states that no longer grant anything.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/umccr/data-releaser/internal/auth"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/testgen"
)

// listingResearcher is the account that holds records of both kinds, which is the case the
// listing exists for: one researcher, two mechanisms, one answer.
const listingResearcher = "534840902377"

// listingLedger is one table holding every kind of record a listing has to report, in
// every state it has to report them in.
type listingLedger struct {
	shares *auth.ShareStore
	copies *dataset.CopyStore
	grants *dataset.GrantStore

	// expired and revoked are the two grants seeded against the staged copy, kept so a
	// test can name them rather than searching the rendered table for an identifier the
	// ledger minted.
	expired *dataset.Grant
	revoked *dataset.Grant
}

// mixedLedger seeds one table with records of both mechanisms and of both this
// mechanism's kinds, spanning every state a listing has to report.
//
// Everything is written through the stores the running commands write through rather than
// as raw items, so the table holds exactly what they would put there -- including the
// markers that keep each store from reading records that are not its own.
func mixedLedger(t *testing.T) listingLedger {
	t.Helper()

	doubles := newReleaseDoubles()

	staged := doubles.seedCopy(t, copyID(1), dataset.CopyStateStaged)
	for i, state := range []dataset.CopyState{dataset.CopyStateCopying, dataset.CopyStatePurged} {
		c := copyFixture(copyID(i+2), state)
		c.CreatedAt = releaseClock.Add(-time.Duration(i+2) * time.Hour)
		doubles.seedCopies(t, c)
	}

	// One grant whose expiry is already behind us and one that was withdrawn. The first
	// is the interesting one: the record still reads as granted, and only the comparison
	// against the clock makes the row say expired.
	lapsed := releaseClock.Add(-time.Minute)
	grants := doubles.seedGrants(t, staged,
		grantSeed{account: listingResearcher, expiresAt: &lapsed},
		grantSeed{account: listingResearcher, vpc: secondVpc, revoked: true},
	)

	shares := auth.NewShareStore(doubles.ledger, config.DefaultLedgerTable)
	expiry := releaseClock.Add(7 * 24 * time.Hour)
	if _, _, err := shares.CreateIdempotent(context.Background(), auth.CreateInput{
		ResearcherAccountID: listingResearcher,
		TargetVpcArn:        testgen.VPCArnForAccount(listingResearcher),
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://warehouse/L2400001/"},
		ExpiresAt:           &expiry,
	}); err != nil {
		t.Fatalf("seeding an htsget share: %v", err)
	}

	return listingLedger{
		shares:  shares,
		copies:  doubles.copyStore(),
		grants:  doubles.grantStore(),
		expired: grants[0],
		revoked: grants[1],
	}
}

// listingFor renders what a bare or narrowed invocation would report from the seeded
// table, through the same reads the command makes.
func listingFor(t *testing.T, filter mechanismFilter, ledger listingLedger, account string) string {
	t.Helper()

	ctx := context.Background()
	var rows []listingRow

	if filter.covers(mechanismHtsget) {
		records, err := listShares(ctx, ledger.shares, account)
		if err != nil {
			t.Fatalf("reading htsget shares: %v", err)
		}
		rows = append(rows, htsgetRows(records, releaseClock)...)
	}
	if filter.covers(mechanismS3) {
		grants, err := listGrants(ctx, ledger.grants, account)
		if err != nil {
			t.Fatalf("reading dataset grants: %v", err)
		}
		rows = append(rows, grantRows(grants, releaseClock)...)

		// Copies name no researcher, so a narrowed listing leaves them out entirely.
		if account == "" {
			copies, err := ledger.copies.ListAll(ctx)
			if err != nil {
				t.Fatalf("reading dataset copies: %v", err)
			}
			rows = append(rows, copyRows(copies)...)
		}
	}

	sortRows(rows)

	var rendered strings.Builder
	renderListing(&rendered, rows)
	return rendered.String()
}

// TestBareListingReportsEveryKindInEveryState checks the default: naming no mechanism
// covers all of them, and every record appears whatever state it is in.
//
// The revoked, purged and failed rows are the ones worth insisting on. A listing that
// showed only what currently gives access would answer "who can read right now?", where the
// question being asked is "what has been transferred and what has been granted?" -- and an
// audit needs the second.
func TestBareListingReportsEveryKindInEveryState(t *testing.T) {
	ledger := mixedLedger(t)

	rendered := listingFor(t, mechanismFilter{}, ledger, "")

	for _, want := range []string{"MECHANISM", "TARGET VPC", "STATE"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected the column %q, got:\n%s", want, rendered)
		}
	}
	if !strings.Contains(rendered, string(mechanismS3)) || !strings.Contains(rendered, string(mechanismHtsget)) {
		t.Errorf("expected both mechanisms to be named, got:\n%s", rendered)
	}

	// A copy and a grant keep their own vocabularies. The expired row is derived rather
	// than stored: the record still reads as granted, and only the comparison against the
	// clock makes it expired.
	for _, state := range []string{"expired", "revoked", "staged", "copying", "purged", "active"} {
		if !strings.Contains(rendered, state) {
			t.Errorf("expected a row in state %q, got:\n%s", state, rendered)
		}
	}

	for i := 1; i <= 3; i++ {
		if !strings.Contains(rendered, copyID(i)) {
			t.Errorf("expected copy %s to be listed, got:\n%s", copyID(i), rendered)
		}
	}
	for _, grant := range []*dataset.Grant{ledger.expired, ledger.revoked} {
		if !strings.Contains(rendered, grant.GrantID) {
			t.Errorf("expected grant %s to be listed, got:\n%s", grant.GrantID, rendered)
		}
	}
	if !strings.Contains(rendered, releaseDataset) {
		t.Errorf("expected the s3 rows to be described by their dataset, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "L2400001") {
		t.Errorf("expected an htsget share to be described by its libraries, got:\n%s", rendered)
	}
}

// TestNarrowedListingExcludesTheOtherMechanism checks that naming one mechanism reports
// only that one, in both directions. Getting this wrong in either direction is the same
// mistake: a row belonging to the mechanism that was not asked for, or a row missing from
// the one that was.
func TestNarrowedListingExcludesTheOtherMechanism(t *testing.T) {
	ledger := mixedLedger(t)

	onlyS3 := listingFor(t, mechanismFilter{named: mechanismS3}, ledger, "")
	if !strings.Contains(onlyS3, releaseDataset) {
		t.Errorf("expected the dataset copies and grants, got:\n%s", onlyS3)
	}
	if strings.Contains(onlyS3, "L2400001") {
		t.Errorf("a listing narrowed to s3 reported an htsget share:\n%s", onlyS3)
	}

	onlyHtsget := listingFor(t, mechanismFilter{named: mechanismHtsget}, ledger, "")
	if !strings.Contains(onlyHtsget, "L2400001") {
		t.Errorf("expected the htsget shares, got:\n%s", onlyHtsget)
	}
	if strings.Contains(onlyHtsget, releaseDataset) {
		t.Errorf("a listing narrowed to htsget reported a dataset record:\n%s", onlyHtsget)
	}
}

// TestAccountFilterAppliesToBothMechanisms checks that narrowing by researcher covers both
// mechanisms, since the point of one filter is one answer.
//
// Copies drop out entirely, which is right: an account filter asks what that account has,
// and nobody has a copy. A dataset transferred here but granted to no one is not part of
// the answer to that question.
func TestAccountFilterAppliesToBothMechanisms(t *testing.T) {
	ledger := mixedLedger(t)

	held := listingFor(t, mechanismFilter{}, ledger, listingResearcher)
	if !strings.Contains(held, "L2400001") {
		t.Errorf("expected the account's htsget share, got:\n%s", held)
	}
	if !strings.Contains(held, ledger.expired.GrantID) {
		t.Errorf("expected the account's dataset grant, got:\n%s", held)
	}
	for i := 1; i <= 3; i++ {
		if strings.Contains(held, copyID(i)) {
			t.Errorf("copy %s appeared under an account filter:\n%s", copyID(i), held)
		}
	}

	other := listingFor(t, mechanismFilter{}, ledger, otherAccount)
	if strings.Contains(other, listingResearcher) {
		t.Errorf("an account filter reported another account's records:\n%s", other)
	}
}

// TestListingOrdersNewestFirstAcrossMechanisms checks the merge. Each store returns its own
// records newest first; putting two of them in one table needs the order imposed again, or
// the listing appears to shuffle between runs.
func TestListingOrdersNewestFirstAcrossMechanisms(t *testing.T) {
	now := releaseClock
	rows := []listingRow{
		{mechanism: "s3", id: "older", created: now.Add(-2 * time.Hour)},
		{mechanism: "htsget", id: "newest", created: now},
		{mechanism: "s3", id: "middle", created: now.Add(-time.Hour)},
	}
	sortRows(rows)

	for i, want := range []string{"newest", "middle", "older"} {
		if rows[i].id != want {
			t.Errorf("row %d = %q, want %q", i, rows[i].id, want)
		}
	}

	// Two records created in the same second keep a stable order rather than appearing to
	// move between runs.
	tied := []listingRow{
		{id: "a", created: now},
		{id: "b", created: now},
	}
	sortRows(tied)
	if tied[0].id != "b" || tied[1].id != "a" {
		t.Errorf("a tie broke inconsistently: %q then %q", tied[0].id, tied[1].id)
	}
}

// TestCopyRowsDistinguishNoExpiryFromNoAccess checks a distinction the eye would otherwise
// miss. A grant made without an expiry is unlimited; a copy has no expiry for a quite
// different reason -- it gives nobody anything -- and rendering that as unlimited would
// read as unlimited access where there is none.
func TestCopyRowsDistinguishNoExpiryFromNoAccess(t *testing.T) {
	unlimited := &dataset.Grant{
		GrantID:             "unlimited-grant",
		CopyID:              copyID(1),
		DatasetURI:          releaseDataset,
		ResearcherAccountID: releaseResearcher,
		TargetVpcID:         releaseVpc,
		State:               dataset.GrantStateGranted,
		CreatedAt:           releaseClock,
	}

	grantRow := grantRows([]*dataset.Grant{unlimited}, releaseClock)[0]
	copyRow := copyRows([]*dataset.Copy{copyFixture(copyID(1), dataset.CopyStateStaged)})[0]

	if grantRow.expiry != "unlimited" {
		t.Errorf("a grant with no expiry rendered as %q, want unlimited", grantRow.expiry)
	}
	if copyRow.expiry != absentValue {
		t.Errorf("a copy rendered its expiry as %q, want %q", copyRow.expiry, absentValue)
	}
	if copyRow.account != absentValue || copyRow.targetVpc != absentValue {
		t.Errorf("a copy named an account or VPC: %+v", copyRow)
	}
	if copyRow.state != string(dataset.CopyStateStaged) {
		t.Errorf("a copy rendered state %q; it keeps its own vocabulary", copyRow.state)
	}
}

// TestTargetVpcColumnIsComparableAcrossMechanisms checks the one column the two record
// kinds store differently.
//
// An htsget share holds the full VPC ARN because that is what the authorization callout
// matches on, and a dataset release holds the bare identifier because that is what the
// bucket policy condition compares against. The column exists to be scanned down and
// compared, so both are rendered as the bare identifier -- while a value in neither form is
// printed as stored, since a VPC written wrongly is exactly what a listing should let an
// operator notice.
func TestTargetVpcColumnIsComparableAcrossMechanisms(t *testing.T) {
	cases := map[string]string{
		"arn:aws:ec2:ap-southeast-2:534840902377:vpc/" + releaseVpc: releaseVpc,
		releaseVpc:             releaseVpc,
		"":                     absentValue,
		"vpc-lattice-nonsense": "vpc-lattice-nonsense",
	}
	for stored, want := range cases {
		if got := formatTargetVpc(stored); got != want {
			t.Errorf("formatTargetVpc(%q) = %q, want %q", stored, got, want)
		}
	}
}

// TestEmptyListingSaysWhatWasLookedFor checks that nothing found is not reported as a
// broader answer than the question asked. "No shares or releases found" and "no s3 releases
// found for account X" mean quite different things to anyone reading the output.
func TestEmptyListingSaysWhatWasLookedFor(t *testing.T) {
	cases := []struct {
		filter  mechanismFilter
		account string
		want    string
	}{
		{mechanismFilter{}, "", "No shares, copies or grants found."},
		{mechanismFilter{named: mechanismS3}, "", "No s3 copies or grants found."},
		{mechanismFilter{named: mechanismHtsget}, "", "No htsget shares found."},
		{mechanismFilter{named: mechanismS3}, listingResearcher,
			"No s3 copies or grants found for account " + listingResearcher + "."},
	}
	for _, c := range cases {
		if got := emptyListingMessage(c.filter, c.account); got != c.want {
			t.Errorf("emptyListingMessage(%v, %q) = %q, want %q", c.filter, c.account, got, c.want)
		}
	}
}

// TestHtsgetRowsKeepTheirOwnStateVocabulary checks that the join happens on the display
// fields and nowhere else: each mechanism reports the state its own record type derives,
// and neither is translated into the other's vocabulary.
func TestHtsgetRowsKeepTheirOwnStateVocabulary(t *testing.T) {
	past := releaseClock.Add(-time.Hour)
	records := []*model.ShareRecord{
		{ShareID: "expired", ResearcherAccountID: listingResearcher, CreatedAt: past, ExpiresAt: &past},
		{ShareID: "revoked", ResearcherAccountID: listingResearcher, CreatedAt: past, RevokedAt: &past},
	}

	rows := htsgetRows(records, releaseClock)
	if rows[0].state != string(model.ShareStatusExpired) {
		t.Errorf("expired share rendered state %q", rows[0].state)
	}
	if rows[1].state != string(model.ShareStatusRevoked) {
		t.Errorf("revoked share rendered state %q", rows[1].state)
	}
	for _, row := range rows {
		if row.mechanism != string(mechanismHtsget) {
			t.Errorf("row %s named mechanism %q", row.id, row.mechanism)
		}
	}
}
