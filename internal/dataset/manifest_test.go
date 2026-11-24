package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/manifest"
)

// Fixtures for the researcher-facing document, kept apart from the policy, ledger, and
// copy fixtures so that a change to any of those cannot quietly move what these assert.
const (
	// docAccount is the account the documents under test are written for.
	docAccount = "123456789012"
	// docStranger is a second researcher account, used to check that access belonging
	// to one account never appears in another's document.
	docStranger = "210987654321"

	docRegion    = "ap-southeast-2"
	docOwner     = "999988887777"
	docBucket    = "data-control-researcher-manifests"
	docKeyPrefix = "datasets/"

	docCohortA = "s3://ceph-source/datasets/cohort-a/"
	docCohortB = "s3://ceph-source/datasets/cohort-b/"

	docVpc = "vpc-0a1b2c3d4e5f60001"
)

// docClock pins the moment the documents are generated at, so a generated timestamp and
// the expiries either side of it are things a test can state exactly.
var docClock = time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)

// docGrantID renders an identifier of the same shape and length as a real one, for the
// tests that need several distinguishable grants.
func docGrantID(n int) string {
	return fmt.Sprintf("01JMANIFEST%015d", n)
}

// docCacheBucket, docKeyArn and docCopyID are derived from the dataset rather than from
// the grant, because a dataset has one cache bucket and one key however many grants read
// from it.
func docCacheBucket(datasetURI string) string {
	return "hot-cache-" + docDatasetName(datasetURI)
}

func docKeyArn(datasetURI string) string {
	return fmt.Sprintf("arn:aws:kms:%s:%s:key/%s", docRegion, docOwner, docDatasetName(datasetURI))
}

func docCopyID(datasetURI string) string {
	return "01JCOPY" + docDatasetName(datasetURI)
}

func docDatasetName(datasetURI string) string {
	return strings.Trim(strings.TrimPrefix(datasetURI, "s3://ceph-source/datasets/"), "/")
}

// docGrant returns a grant of one dataset to docAccount, in the given state. Tests that
// care about a different account, VPC, or copy adjust the returned record.
func docGrant(id, datasetURI string, state GrantState, expiresAt *time.Time) *Grant {
	return &Grant{
		GrantID:             id,
		CopyID:              docCopyID(datasetURI),
		DatasetURI:          datasetURI,
		HotCacheBucket:      docCacheBucket(datasetURI),
		KeyArn:              docKeyArn(datasetURI),
		ResearcherAccountID: docAccount,
		TargetVpcID:         docVpc,
		State:               state,
		CreatedAt:           docClock.Add(-72 * time.Hour),
		ExpiresAt:           expiresAt,
	}
}

// docGrantIDs lists the grants a document describes, in the order it lists them.
func docGrantIDs(document *Manifest) []string {
	if document == nil {
		return nil
	}
	ids := make([]string, 0, len(document.Datasets))
	for _, entry := range document.Datasets {
		ids = append(ids, entry.GrantID)
	}
	return ids
}

// A document lists a grant only while that grant gives access. Every other state is
// absent, which is what makes the document's presence in a bucket say something: an entry
// means access now, not access once.
func TestManifestBuildListsOnlyGrantsThatGiveAccess(t *testing.T) {
	future := docClock.Add(24 * time.Hour)
	past := docClock.Add(-24 * time.Hour)

	tests := []struct {
		name    string
		state   GrantState
		expires *time.Time
		listed  bool
	}{
		{name: "access until a future date", state: GrantStateGranted, expires: &future, listed: true},
		{name: "access with no end date", state: GrantStateGranted, expires: nil, listed: true},
		{name: "access that has already ended", state: GrantStateGranted, expires: &past},
		// The boundary counts as ended, matching the time condition the deployed
		// policies carry: it admits a request strictly before the expiry.
		{name: "access ending at this very moment", state: GrantStateGranted, expires: &docClock},
		{name: "access withdrawn", state: GrantStateRevoked, expires: &future},
		{name: "access withdrawn after it had already lapsed", state: GrantStateRevoked, expires: &past},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grant := docGrant(docGrantID(1), docCohortA, tc.state, tc.expires)
			document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{grant}, docClock)

			if !tc.listed {
				if document != nil {
					t.Fatalf("expected no document, got one describing %v", docGrantIDs(document))
				}
				return
			}
			if document == nil {
				t.Fatal("expected a document describing the grant, got none")
			}
			if got := docGrantIDs(document); !slices.Equal(got, []string{grant.GrantID}) {
				t.Errorf("described grants: expected %v, got %v", []string{grant.GrantID}, got)
			}
		})
	}
}

// The same filter applied across a mixed ledger: whatever else the account has held, the
// document describes only what it can read now.
func TestManifestBuildIgnoresEndedAccessAlongsideCurrentAccess(t *testing.T) {
	future := docClock.Add(24 * time.Hour)
	past := docClock.Add(-24 * time.Hour)

	active := docGrant(docGrantID(1), docCohortA, GrantStateGranted, &future)
	unlimited := docGrant(docGrantID(2), docCohortB, GrantStateGranted, nil)
	expired := docGrant(docGrantID(3), docCohortA, GrantStateGranted, &past)
	withdrawn := docGrant(docGrantID(4), docCohortB, GrantStateRevoked, &future)

	document := NewManifestBuilder(docRegion).Build(
		docAccount,
		[]*Grant{expired, active, unlimited, withdrawn},
		docClock,
	)
	if document == nil {
		t.Fatal("expected a document describing the two current grants, got none")
	}

	want := []string{active.GrantID, unlimited.GrantID}
	if got := docGrantIDs(document); !slices.Equal(got, want) {
		t.Errorf("described grants: expected %v, got %v", want, got)
	}
}

// Nothing current means no document at all, rather than one describing an empty list.
// Publishing turns that into a deletion, so an account whose last access ended is not
// left holding a description of it.
func TestManifestBuildReturnsNothingWhenNoAccessRemains(t *testing.T) {
	past := docClock.Add(-time.Hour)

	tests := []struct {
		name    string
		account string
		grants  []*Grant
	}{
		{name: "no grants at all", account: docAccount},
		{
			name:    "every grant ended",
			account: docAccount,
			grants: []*Grant{
				docGrant(docGrantID(1), docCohortA, GrantStateGranted, &past),
				docGrant(docGrantID(2), docCohortB, GrantStateRevoked, nil),
				docGrant(docGrantID(3), docCohortA, GrantStateRevoked, &past),
			},
		},
		{
			name:    "no account to write a document for",
			account: "  ",
			grants: []*Grant{
				docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			document := NewManifestBuilder(docRegion).Build(tc.account, tc.grants, docClock)
			if document != nil {
				t.Fatalf("expected no document, got one describing %v", docGrantIDs(document))
			}
		})
	}
}

// A document is readable by the account it names, so a grant belonging to someone else
// must never reach it. A caller passing a mixed list is a mistake, and the safe reading
// of that mistake is to drop what was not asked for rather than disclose it.
func TestManifestBuildDescribesNoOtherAccountsAccess(t *testing.T) {
	mine := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)
	theirs := docGrant(docGrantID(2), docCohortB, GrantStateGranted, nil)
	theirs.ResearcherAccountID = docStranger

	builder := NewManifestBuilder(docRegion)

	document := builder.Build(docAccount, []*Grant{mine, theirs}, docClock)
	if document == nil {
		t.Fatal("expected a document describing my own grant, got none")
	}
	if got := docGrantIDs(document); !slices.Equal(got, []string{mine.GrantID}) {
		t.Errorf("described grants: expected only %q, got %v", mine.GrantID, got)
	}
	for _, entry := range document.Datasets {
		if entry.DatasetURI == theirs.DatasetURI {
			t.Errorf("document written for %s names dataset %q granted to %s",
				docAccount, entry.DatasetURI, docStranger)
		}
	}

	// The stranger's own document is built the same way and equally excludes mine.
	stranger := builder.Build(docStranger, []*Grant{mine, theirs}, docClock)
	if stranger == nil {
		t.Fatal("expected a document for the other account, got none")
	}
	if got := docGrantIDs(stranger); !slices.Equal(got, []string{theirs.GrantID}) {
		t.Errorf("other account's described grants: expected only %q, got %v", theirs.GrantID, got)
	}
}

// A nil in the list is a caller's slip, not a reason to fail: it describes no access, so
// it is skipped like access that has ended.
func TestManifestBuildSkipsAbsentGrants(t *testing.T) {
	active := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)

	document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{nil, active, nil}, docClock)
	if document == nil {
		t.Fatal("expected a document describing the one real grant, got none")
	}
	if got := docGrantIDs(document); !slices.Equal(got, []string{active.GrantID}) {
		t.Errorf("described grants: expected %v, got %v", []string{active.GrantID}, got)
	}
}

// What a reader needs in order to find the data: where it is, which key decrypts it,
// where they must read it from, and when it stops working. All of it comes off the grant,
// which carries the copy's bucket and key alongside its own fields.
func TestManifestBuildDescribesWhereToReadTheDataFrom(t *testing.T) {
	expiry := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	grant := docGrant(docGrantID(7), docCohortA, GrantStateGranted, &expiry)

	document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{grant}, docClock)
	if document == nil {
		t.Fatal("expected a document, got none")
	}

	if document.AccountID != docAccount {
		t.Errorf("account: expected %q, got %q", docAccount, document.AccountID)
	}
	if !document.GeneratedAt.Equal(docClock) {
		t.Errorf("generated at: expected %s, got %s", docClock, document.GeneratedAt)
	}
	if document.Note == "" {
		t.Error("note: expected the document to carry its own caution, got an empty string")
	}
	if len(document.Datasets) != 1 {
		t.Fatalf("expected exactly one entry, got %d", len(document.Datasets))
	}

	entry := document.Datasets[0]
	for _, field := range []struct {
		what string
		got  string
		want string
	}{
		{"grant", entry.GrantID, grant.GrantID},
		{"dataset", entry.DatasetURI, docCohortA},
		{"bucket", entry.Bucket, docCacheBucket(docCohortA)},
		{"region", entry.Region, docRegion},
		// The bucket holds one dataset, so its objects sit at the root.
		{"prefix", entry.Prefix, ""},
		// Named so a reader is not left working out why a listing shows a prefix they
		// are refused.
		{"excluded prefix", entry.ExcludedPrefix, ControlPrefix + "/"},
		{"key", entry.KeyArn, docKeyArn(docCohortA)},
		{"VPC to read from", entry.TargetVpcID, docVpc},
	} {
		if field.got != field.want {
			t.Errorf("%s: expected %q, got %q", field.what, field.want, field.got)
		}
	}
	if entry.ExpiresAt == nil {
		t.Fatal("expiry: expected the recorded expiry, got none")
	}
	if !entry.ExpiresAt.Equal(expiry) {
		t.Errorf("expiry: expected %s, got %s", expiry, *entry.ExpiresAt)
	}
}

// Timestamps in the document are absolute, so a reader in another zone reads the same
// instant the ledger recorded.
func TestManifestBuildStatesTimestampsInUTC(t *testing.T) {
	elsewhere := time.FixedZone("AEST", 10*60*60)
	expiry := time.Date(2026, 8, 10, 14, 0, 0, 0, elsewhere)
	grant := docGrant(docGrantID(1), docCohortA, GrantStateGranted, &expiry)

	document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{grant}, docClock.In(elsewhere))
	if document == nil {
		t.Fatal("expected a document, got none")
	}
	if document.GeneratedAt.Location() != time.UTC {
		t.Errorf("generated at: expected UTC, got %s", document.GeneratedAt.Location())
	}
	if !document.GeneratedAt.Equal(docClock) {
		t.Errorf("generated at: expected %s, got %s", docClock, document.GeneratedAt)
	}

	got := document.Datasets[0].ExpiresAt
	if got == nil {
		t.Fatal("expiry: expected the recorded expiry, got none")
	}
	if got.Location() != time.UTC {
		t.Errorf("expiry: expected UTC, got %s", got.Location())
	}
	if !got.Equal(expiry) {
		t.Errorf("expiry: expected %s, got %s", expiry, *got)
	}
}

// A document already built describes the access as it stood when it was built. A caller
// that goes on to move an expiry must not find the document it was handed has changed
// underneath it.
func TestManifestBuildCopiesTheExpiryRatherThanSharingIt(t *testing.T) {
	expiry := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	grant := docGrant(docGrantID(1), docCohortA, GrantStateGranted, &expiry)

	// Held separately, since moving the expiry below writes through the same variable the
	// grant points at.
	asBuilt := expiry

	document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{grant}, docClock)
	if document == nil {
		t.Fatal("expected a document, got none")
	}

	*grant.ExpiresAt = expiry.Add(365 * 24 * time.Hour)

	got := document.Datasets[0].ExpiresAt
	if got == nil || !got.Equal(asBuilt) {
		t.Errorf("expiry: expected the document to keep %s, got %v", asBuilt, got)
	}
}

// Two grants of the same dataset to the same account can differ in the VPC they are read
// from and in when they end, so each gets its own entry. Collapsing them would have to
// misreport one of them.
func TestManifestBuildKeepsOneEntryPerGrant(t *testing.T) {
	firstExpiry := docClock.Add(24 * time.Hour)
	first := docGrant(docGrantID(1), docCohortA, GrantStateGranted, &firstExpiry)
	first.TargetVpcID = "vpc-0aaaaaaaaaaaaaaa1"

	second := docGrant(docGrantID(2), docCohortA, GrantStateGranted, nil)
	second.TargetVpcID = "vpc-0bbbbbbbbbbbbbbb2"

	document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{first, second}, docClock)
	if document == nil {
		t.Fatal("expected a document, got none")
	}
	if len(document.Datasets) != 2 {
		t.Fatalf("expected one entry per grant, got %d for 2 grants of the same dataset",
			len(document.Datasets))
	}

	byGrant := make(map[string]ManifestDataset, len(document.Datasets))
	for _, entry := range document.Datasets {
		byGrant[entry.GrantID] = entry
	}

	if got := byGrant[first.GrantID]; got.TargetVpcID != first.TargetVpcID ||
		got.ExpiresAt == nil || !got.ExpiresAt.Equal(firstExpiry) {
		t.Errorf("first grant: expected VPC %q ending %s, got VPC %q ending %v",
			first.TargetVpcID, firstExpiry, got.TargetVpcID, got.ExpiresAt)
	}
	if got := byGrant[second.GrantID]; got.TargetVpcID != second.TargetVpcID || got.ExpiresAt != nil {
		t.Errorf("second grant: expected VPC %q with no end date, got VPC %q ending %v",
			second.TargetVpcID, got.TargetVpcID, got.ExpiresAt)
	}

	// Both read the same dataset, so both point at the one bucket and the one key.
	for _, entry := range document.Datasets {
		if entry.Bucket != docCacheBucket(docCohortA) || entry.KeyArn != docKeyArn(docCohortA) {
			t.Errorf("grant %s: expected bucket %q and key %q, got %q and %q",
				entry.GrantID, docCacheBucket(docCohortA), docKeyArn(docCohortA),
				entry.Bucket, entry.KeyArn)
		}
	}
}

// The same access produces the same document however the ledger happened to return it,
// so republishing an unchanged grant does not churn the object.
func TestManifestBuildOrdersEntriesTheSameWayEveryTime(t *testing.T) {
	a1 := docGrant(docGrantID(3), docCohortA, GrantStateGranted, nil)
	a2 := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)
	b := docGrant(docGrantID(2), docCohortB, GrantStateGranted, nil)

	want := []string{a2.GrantID, a1.GrantID, b.GrantID}
	builder := NewManifestBuilder(docRegion)

	orderings := map[string][]*Grant{
		"ledger order":  {a1, a2, b},
		"reverse order": {b, a1, a2},
		"mixed order":   {a2, b, a1},
	}
	for name, order := range orderings {
		t.Run(name, func(t *testing.T) {
			document := builder.Build(docAccount, order, docClock)
			if document == nil {
				t.Fatal("expected a document, got none")
			}
			if got := docGrantIDs(document); !slices.Equal(got, want) {
				t.Errorf("entry order: expected %v, got %v", want, got)
			}
		})
	}
}

// An unlimited grant says so explicitly rather than leaving the field out. Absent and
// null read the same to most parsers, but an explicit null is the difference between "no
// end date" and "this document does not say", and a reader should not have to guess
// which. It is also not a promise: access can still be withdrawn.
func TestManifestUnlimitedAccessCarriesAnExplicitNullExpiry(t *testing.T) {
	grant := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)

	document := NewManifestBuilder(docRegion).Build(docAccount, []*Grant{grant}, docClock)
	if document == nil {
		t.Fatal("expected a document, got none")
	}
	if document.Datasets[0].ExpiresAt != nil {
		t.Fatalf("expiry: expected none, got %s", *document.Datasets[0].ExpiresAt)
	}

	entry := marshalledEntry(t, document)
	raw, present := entry["expiresAt"]
	if !present {
		t.Fatal("serialised entry: expected an expiresAt field, got none")
	}
	if string(raw) != "null" {
		t.Errorf("serialised expiry: expected null, got %s", raw)
	}
}

// marshalledEntry serialises a document and returns its single entry's raw fields, so a
// test can assert on what a reader actually receives rather than on the Go value.
func marshalledEntry(t *testing.T, document *Manifest) map[string]json.RawMessage {
	t.Helper()

	data, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("serialising the document: %v", err)
	}
	var envelope struct {
		Datasets []map[string]json.RawMessage `json:"datasets"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("reading back the serialised document: %v", err)
	}
	if len(envelope.Datasets) != 1 {
		t.Fatalf("expected exactly one serialised entry, got %d", len(envelope.Datasets))
	}
	return envelope.Datasets[0]
}

// jsonFields returns the field names a struct serialises to, in declaration order.
func jsonFields(t *testing.T, value any) []string {
	t.Helper()

	typ := reflect.TypeOf(value)
	if typ.Kind() != reflect.Struct {
		t.Fatalf("expected a struct, got %s", typ.Kind())
	}
	names := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	return names
}

// The document is descriptive and nothing else: no read consults it, and editing it
// grants nothing. That is a constraint on the schema as much as on the code, so the
// schema is pinned. A field carrying a permission, a policy fragment, or a lifecycle
// decision would invite a reader to treat the document as the authority on what they may
// do, and it is not -- the bucket policy and the key policy are.
func TestManifestSchemaNamesNothingAccessDependsOn(t *testing.T) {
	envelope := jsonFields(t, Manifest{})
	wantEnvelope := []string{"note", "accountId", "generatedAt", "datasets"}
	if !slices.Equal(envelope, wantEnvelope) {
		t.Errorf("document fields: expected %v, got %v", wantEnvelope, envelope)
	}

	entry := jsonFields(t, ManifestDataset{})
	wantEntry := []string{
		"grantId", "datasetUri", "bucket", "region", "prefix",
		"excludedPrefix", "keyArn", "targetVpcId", "expiresAt",
	}
	if !slices.Equal(entry, wantEntry) {
		t.Errorf("entry fields: expected %v, got %v", wantEntry, entry)
	}

	// Nothing in either shape may look like an authorisation decision. "state" is on the
	// list deliberately: only current access is described at all, so a state field would
	// be either always the same value or a second, contradictory account of whether the
	// reader has access.
	forbidden := []string{
		"action", "allow", "credential", "deny", "effect", "grant", "permission",
		"policy", "principal", "role", "secret", "state", "statement",
	}
	// grantId is the single name allowed to carry one of those words. It is the
	// identifier of the ledger record the entry was built from -- what an operator quotes
	// when asked about the access -- and an opaque identifier decides nothing. Exempting
	// the exact name rather than dropping the word keeps "grants", "grantedBy", and
	// anything else the word could turn up in refused.
	const identifier = "grantId"

	for _, name := range slices.Concat(envelope, entry) {
		if name == identifier {
			continue
		}
		lowered := strings.ToLower(name)
		for _, word := range forbidden {
			if strings.Contains(lowered, word) {
				t.Errorf("field %q reads as an authorisation decision (%q); the document "+
					"describes where the data is, not what may be done with it", name, word)
			}
		}
	}
}

// The publisher is built from the grant store and from nothing else, so this is the one
// question it may ask of the ledger. Stating it here means a method removed or resigned on
// the store fails at compile time rather than at the first publish after a policy has
// already been applied.
var _ accountGrantLister = (*GrantStore)(nil)

// ledgerGrants stands in for the ledger read that publishing depends on: every grant
// recorded for one account, whatever state each is in.
type ledgerGrants struct {
	byAccount map[string][]*Grant
	err       error

	// asked records the accounts publishing looked up, so a test can check it asked for
	// the one it was publishing for and nothing else.
	asked []string
}

func (l *ledgerGrants) ListByAccount(_ context.Context, researcherAccountID string) ([]*Grant, error) {
	l.asked = append(l.asked, researcherAccountID)
	if l.err != nil {
		return nil, l.err
	}
	return l.byAccount[researcherAccountID], nil
}

// docLedger returns a ledger holding the given grants under the account each names.
func docLedger(grants ...*Grant) *ledgerGrants {
	ledger := &ledgerGrants{byAccount: make(map[string][]*Grant)}
	for _, grant := range grants {
		account := grant.ResearcherAccountID
		ledger.byAccount[account] = append(ledger.byAccount[account], grant)
	}
	return ledger
}

// docKey is where one account's document is delivered.
func docKey(accountID string) string {
	return docKeyPrefix + accountID + ".json"
}

func docTarget() ManifestTarget {
	return ManifestTarget{
		Bucket: docBucket,
		RenderKey: func(accountID string) (string, error) {
			return docKey(accountID), nil
		},
	}
}

// newDocPublisher returns a publisher writing through a recording S3 double, with the
// clock pinned so a generated timestamp is something a test can state exactly.
func newDocPublisher(t *testing.T, ledger *ledgerGrants) (*awstest.S3, *ManifestPublisher) {
	t.Helper()

	objects := &awstest.S3{}
	publisher, err := NewManifestPublisher(
		ledger,
		manifest.NewWriter(objects),
		NewManifestBuilder(docRegion),
		docTarget(),
	)
	if err != nil {
		t.Fatalf("building a publisher: %v", err)
	}
	publisher.now = func() time.Time { return docClock }
	return objects, publisher
}

func TestPublishManifestWritesTheAccountsCurrentAccess(t *testing.T) {
	expiry := docClock.Add(24 * time.Hour)
	active := docGrant(docGrantID(1), docCohortA, GrantStateGranted, &expiry)
	ended := docGrant(docGrantID(2), docCohortB, GrantStateRevoked, nil)
	objects, publisher := newDocPublisher(t, docLedger(active, ended))

	published, err := publisher.Publish(context.Background(), docAccount)
	if err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if published == nil {
		t.Fatal("expected the published document to be returned, got none")
	}
	if len(objects.DeleteCalls) != 0 {
		t.Errorf("expected no deletion, got %d", len(objects.DeleteCalls))
	}
	if len(objects.PutCalls) != 1 {
		t.Fatalf("expected exactly one write, got %d", len(objects.PutCalls))
	}

	put := objects.PutCalls[0]
	if got := aws.ToString(put.Bucket); got != docBucket {
		t.Errorf("bucket written to: expected %q, got %q", docBucket, got)
	}
	if got, want := aws.ToString(put.Key), docKey(docAccount); got != want {
		t.Errorf("key written to: expected %q, got %q", want, got)
	}
	if got := aws.ToString(put.ContentType); got != "application/json" {
		t.Errorf("content type: expected application/json, got %q", got)
	}

	// What the researcher actually receives, read back the way they would read it.
	body, err := io.ReadAll(put.Body)
	if err != nil {
		t.Fatalf("reading the written body: %v", err)
	}
	var delivered Manifest
	if err := json.Unmarshal(body, &delivered); err != nil {
		t.Fatalf("parsing the written document: %v", err)
	}
	if delivered.AccountID != docAccount {
		t.Errorf("delivered account: expected %q, got %q", docAccount, delivered.AccountID)
	}
	if !delivered.GeneratedAt.Equal(docClock) {
		t.Errorf("delivered timestamp: expected %s, got %s", docClock, delivered.GeneratedAt)
	}
	if got := docGrantIDs(&delivered); !slices.Equal(got, []string{active.GrantID}) {
		t.Errorf("delivered grants: expected %v, got %v", []string{active.GrantID}, got)
	}
}

// Access ending is the case that has to reach S3: the document is removed rather than
// left describing access the account no longer has. Returning no document is not enough
// on its own -- a stale object in the bucket outlives the code that stopped listing it.
func TestPublishManifestRemovesTheDocumentWhenNoAccessRemains(t *testing.T) {
	past := docClock.Add(-time.Hour)

	tests := []struct {
		name   string
		grants []*Grant
	}{
		{name: "the account never held a grant"},
		{
			name:   "its last grant expired",
			grants: []*Grant{docGrant(docGrantID(1), docCohortA, GrantStateGranted, &past)},
		},
		{
			name:   "its access was withdrawn",
			grants: []*Grant{docGrant(docGrantID(1), docCohortA, GrantStateRevoked, nil)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects, publisher := newDocPublisher(t, docLedger(tc.grants...))

			published, err := publisher.Publish(context.Background(), docAccount)
			if err != nil {
				t.Fatalf("publishing: %v", err)
			}
			if published != nil {
				t.Errorf("expected no document, got one describing %v", docGrantIDs(published))
			}
			if len(objects.PutCalls) != 0 {
				t.Errorf("expected nothing written, got %d write(s)", len(objects.PutCalls))
			}
			if len(objects.DeleteCalls) != 1 {
				t.Fatalf("expected the document to be removed, got %d deletion(s)",
					len(objects.DeleteCalls))
			}

			deleted := objects.DeleteCalls[0]
			if got := aws.ToString(deleted.Bucket); got != docBucket {
				t.Errorf("bucket removed from: expected %q, got %q", docBucket, got)
			}
			if got, want := aws.ToString(deleted.Key), docKey(docAccount); got != want {
				t.Errorf("key removed: expected %q, got %q", want, got)
			}
		})
	}
}

// The document is rebuilt from every grant the account holds, not from whichever one just
// changed: one account has one document covering all of its access.
func TestPublishManifestRebuildsFromEveryGrantTheAccountHolds(t *testing.T) {
	first := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)
	second := docGrant(docGrantID(2), docCohortB, GrantStateGranted, nil)
	ledger := docLedger(first, second)
	_, publisher := newDocPublisher(t, ledger)

	published, err := publisher.Publish(context.Background(), docAccount)
	if err != nil {
		t.Fatalf("publishing: %v", err)
	}
	want := []string{first.GrantID, second.GrantID}
	if got := docGrantIDs(published); !slices.Equal(got, want) {
		t.Errorf("described grants: expected %v, got %v", want, got)
	}
	if !slices.Equal(ledger.asked, []string{docAccount}) {
		t.Errorf("accounts looked up: expected %v, got %v", []string{docAccount}, ledger.asked)
	}
}

// The disclosure guard, checked through the publisher: even if the ledger read were to
// hand back a grant belonging to someone else, the document delivered to this account
// does not name it.
func TestPublishManifestDeliversNoOtherAccountsAccess(t *testing.T) {
	mine := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)
	theirs := docGrant(docGrantID(2), docCohortB, GrantStateGranted, nil)
	theirs.ResearcherAccountID = docStranger

	ledger := &ledgerGrants{byAccount: map[string][]*Grant{
		docAccount: {mine, theirs},
	}}
	objects, publisher := newDocPublisher(t, ledger)

	if _, err := publisher.Publish(context.Background(), docAccount); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if len(objects.PutCalls) != 1 {
		t.Fatalf("expected exactly one write, got %d", len(objects.PutCalls))
	}
	body, err := io.ReadAll(objects.PutCalls[0].Body)
	if err != nil {
		t.Fatalf("reading the written body: %v", err)
	}
	if strings.Contains(string(body), docCohortB) || strings.Contains(string(body), theirs.GrantID) {
		t.Errorf("document delivered to %s names another account's grant:\n%s", docAccount, body)
	}
}

func TestPublishManifestReportsFailures(t *testing.T) {
	active := docGrant(docGrantID(1), docCohortA, GrantStateGranted, nil)

	tests := []struct {
		name string
		// arrange breaks one step of publishing and returns nothing; the publisher and
		// its doubles are built fresh per case.
		arrange      func(ledger *ledgerGrants, objects *awstest.S3, publisher *ManifestPublisher)
		account      string
		wantContains []string
	}{
		{
			name:         "no account named",
			account:      "   ",
			wantContains: []string{"researcher account"},
		},
		{
			name: "the document's location cannot be resolved",
			arrange: func(_ *ledgerGrants, _ *awstest.S3, publisher *ManifestPublisher) {
				publisher.target.RenderKey = func(string) (string, error) {
					return "", errors.New("no manifest prefix is configured")
				}
			},
			wantContains: []string{docAccount, "no manifest prefix is configured"},
		},
		{
			name: "the ledger cannot be read",
			arrange: func(ledger *ledgerGrants, _ *awstest.S3, _ *ManifestPublisher) {
				ledger.err = errors.New("the ledger table is unreachable")
			},
			wantContains: []string{docAccount, "the ledger table is unreachable"},
		},
		{
			name: "the document cannot be written",
			arrange: func(_ *ledgerGrants, objects *awstest.S3, _ *ManifestPublisher) {
				objects.PutObjectFn = func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
					return nil, errors.New("access denied")
				}
			},
			// The location is part of the report, because retrying is the remedy and an
			// operator needs to know what to retry against.
			wantContains: []string{docAccount, docBucket, docKey(docAccount), "access denied"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ledger := docLedger(active)
			objects, publisher := newDocPublisher(t, ledger)
			if tc.arrange != nil {
				tc.arrange(ledger, objects, publisher)
			}
			account := tc.account
			if account == "" {
				account = docAccount
			}

			published, err := publisher.Publish(context.Background(), account)
			if err == nil {
				t.Fatalf("expected publishing to fail, got a document describing %v",
					docGrantIDs(published))
			}
			if published != nil {
				t.Errorf("expected no document alongside the failure, got %v",
					docGrantIDs(published))
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("failure report: expected it to mention %q, got %q", want, err)
				}
			}
		})
	}
}

// Publishing happens after the policies are already in place, where a misconfigured
// destination is no longer something an operator can simply retry past. So the
// destination is checked when the publisher is built, not at the first publish.
func TestNewManifestPublisherRefusesAnIncompleteSetup(t *testing.T) {
	ledger := docLedger()
	writer := manifest.NewWriter(&awstest.S3{})
	builder := NewManifestBuilder(docRegion)

	tests := []struct {
		name         string
		grants       accountGrantLister
		writer       *manifest.Writer
		builder      *ManifestBuilder
		target       ManifestTarget
		wantContains string
	}{
		{
			name:         "no ledger to read",
			writer:       writer,
			builder:      builder,
			target:       docTarget(),
			wantContains: "ledger",
		},
		{
			name:         "nothing to write with",
			grants:       ledger,
			builder:      builder,
			target:       docTarget(),
			wantContains: "writer",
		},
		{
			name:         "nothing to describe the access with",
			grants:       ledger,
			writer:       writer,
			target:       docTarget(),
			wantContains: "builder",
		},
		{
			name:         "no bucket to publish into",
			grants:       ledger,
			writer:       writer,
			builder:      builder,
			target:       ManifestTarget{RenderKey: docTarget().RenderKey},
			wantContains: "manifest bucket",
		},
		{
			name:         "no key to publish under",
			grants:       ledger,
			writer:       writer,
			builder:      builder,
			target:       ManifestTarget{Bucket: docBucket},
			wantContains: "key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			publisher, err := NewManifestPublisher(tc.grants, tc.writer, tc.builder, tc.target)
			if err == nil {
				t.Fatal("expected building the publisher to fail, got a publisher")
			}
			if publisher != nil {
				t.Error("expected no publisher alongside the failure, got one")
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("failure report: expected it to mention %q, got %q", tc.wantContains, err)
			}
		})
	}
}
