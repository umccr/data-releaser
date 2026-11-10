package dataset

// This file maps a copy onto an item in the ledger table.
//
// The table is shared two ways. It is shared with the htsget mechanism, which defined it
// and owns the names of its keys, so a copy fills the existing key attributes rather than
// adding its own and marks every item with the mechanism it belongs to. And it is shared
// between this mechanism's own two kinds of record, so a copy also carries a kind, which
// is what tells it from a grant sitting under the same dataset in the same index.
//
// A read that does not get back the kind it asked for refuses the item rather than
// interpreting it. A grant read as a copy would report a transfer that some other record
// actually performed, in a state that does not parse; an htsget share read as either would
// report a copy of no dataset, in no bucket.
//
// The attribute names both kinds write, the key they are both found under, and the way
// timestamps are spelled are declared here for the same reason the kind is: they only mean
// anything if a copy and a grant agree on them, so they belong to neither on its own.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/model"
)

// Attribute names both kinds of record share with the other mechanism's records. Their
// spellings belong to the mechanism that defined the table first, so a copy and a grant
// fill the existing attributes rather than the table growing a second set of keys it
// would have to be migrated for.
const (
	// recordIDAttribute is the table's partition key. A copy and a grant each put their
	// own identifier there, which is why one identifier can only ever name one of them.
	recordIDAttribute = "shareId"

	// researcherAccountAttribute and createdAtAttribute are the partition and sort key
	// of the per-researcher index. Only a grant writes the account -- a transfer is
	// given to nobody -- but both write the creation time, so records of every kind
	// order consistently wherever they appear together.
	researcherAccountAttribute = "researcherAccountId"
	createdAtAttribute         = "createdAt"

	// datasetURIAttribute is the partition key of the index the records of one dataset
	// are found through. Both kinds write it: the copies of a dataset and the grants
	// against them are asked for the same way, and the kind attribute is what tells the
	// answers apart.
	datasetURIAttribute = "datasetUri"

	// stateAttribute is a word DynamoDB reserves, so any expression naming it needs an
	// alias. Every expression here aliases every name for that reason, rather than
	// having to remember which one is the reserved one.
	stateAttribute = "state"
)

// Attribute names only a copy writes: what the transfer delivered, the destruction of the
// key protecting what it left behind, and the removal of what it left behind.
const (
	objectCountAttribute         = "objectCount"
	totalBytesAttribute          = "totalBytes"
	copySummaryLocationAttribute = "copySummaryLocation"
	purgedAtAttribute            = "purgedAt"
	keyDeletionDateAttribute     = "keyDeletionDate"
	deletedAtAttribute           = "deletedAt"
)

// errOtherMechanism reports that a ledger item belongs to the htsget mechanism. Reads are
// filtered on the mechanism, so this is the backstop for anything that gets through: an
// htsget share unmarshalled here would read as a copy of no dataset, in no bucket, in a
// state that does not parse.
var errOtherMechanism = errors.New("ledger item belongs to another release mechanism")

// kindAttribute says which of this mechanism's two kinds of record an item is. The
// mechanism attribute says the record is ours; this one says which of ours it is.
//
// It is declared here rather than beside either kind because the two only mean anything
// as a pair: neither read is safe unless both kinds write it.
const kindAttribute = "kind"

// itemKind is the value kindAttribute holds.
type itemKind string

const (
	// kindCopy marks the record of one transfer.
	kindCopy itemKind = "copy"

	// kindGrant marks the record of one researcher's access to what a transfer
	// delivered.
	kindGrant itemKind = "grant"
)

// errOtherKind reports that a ledger item belongs to this mechanism but is not the kind of
// record the read wanted.
//
// Every read is filtered on the kind, so this is the backstop for whatever gets through --
// including a record written before copies and grants were separate, which carries no kind
// at all and so is neither.
var errOtherKind = errors.New("ledger item is not the kind of record expected")

// checkKind reports whether a stored item is the kind the caller asked for, naming what it
// found instead when it is not.
func checkKind(id, stored string, want itemKind) error {
	if itemKind(stored) == want {
		return nil
	}
	return fmt.Errorf("%w: %q is %s, not a %s", errOtherKind, id, describeKind(stored), want)
}

// describeKind names the kind of a stored item for a report. A record written before
// copies and grants were separate carries no kind at all, and saying so is more use to
// whoever reads the message than an empty pair of quotes.
func describeKind(stored string) string {
	if stored == "" {
		return "a record carrying no kind"
	}
	return "a " + stored
}

// dynamoCopy is the ledger representation of a Copy. Timestamps are RFC3339 strings and
// the state is its stored spelling, so an item is legible in the console and comparable in
// an expression.
//
// One absence is deliberate: there is no researcher account, so a copy stays out of the
// per-researcher index entirely. A copy has no researcher -- that is precisely what a
// grant adds -- and an item there under a blank account would answer "what can this
// account read?" with a transfer nobody was ever given.
//
// The dataset URI is not omitted under any circumstances: it is the partition key of the
// index the copies of one dataset are found through, and a copy missing from it is a copy
// staging would start a second billed transfer alongside.
type dynamoCopy struct {
	CopyID string `dynamodbav:"shareId"`

	// Mechanism marks the item as this mechanism's, so a read of the shared table can
	// tell it from an htsget share. Absence means htsget, so this is never omitted.
	Mechanism string `dynamodbav:"mechanism"`

	// Kind marks it as a copy rather than a grant. Absence means it predates the two
	// being separate, so this is never omitted either.
	Kind string `dynamodbav:"kind"`

	DatasetURI     string `dynamodbav:"datasetUri"`
	HotCacheBucket string `dynamodbav:"hotCacheBucket"`
	KeyArn         string `dynamodbav:"keyArn"`

	// State is a reserved word in DynamoDB, so any expression naming it needs an
	// attribute-name alias.
	State string `dynamodbav:"state"`

	CreatedAt string `dynamodbav:"createdAt"`

	// CopyExecutionArn is recorded before the transfer is under way, and the counts and
	// the summary only once it has reported, so all four are omitted while empty rather
	// than stored as zeroes that would read as a transfer that delivered nothing.
	CopyExecutionArn    string `dynamodbav:"copyExecutionArn,omitempty"`
	ObjectCount         int64  `dynamodbav:"objectCount,omitempty"`
	TotalBytes          int64  `dynamodbav:"totalBytes,omitempty"`
	CopySummaryLocation string `dynamodbav:"copySummaryLocation,omitempty"`

	PurgedAt        *string `dynamodbav:"purgedAt,omitempty"`
	KeyDeletionDate *string `dynamodbav:"keyDeletionDate,omitempty"`

	// DeletedAt is absent until the objects are deleted out of the cache bucket, which
	// most copies never reach. Stored as an absence rather than a zero time, since a
	// zero would read back as a deletion performed in the year one.
	DeletedAt *string `dynamodbav:"deletedAt,omitempty"`
}

// marshalCopy converts a Copy into a ledger item.
//
// A copy with no identifier is refused here rather than at the table, where it would
// surface as a complaint about a missing attribute named for the other mechanism. A state
// the lifecycle does not know is refused for the plainer reason that nothing could read it
// back afterwards.
func marshalCopy(c *Copy) (map[string]types.AttributeValue, error) {
	if strings.TrimSpace(c.CopyID) == "" {
		return nil, errors.New("copy has no identifier")
	}
	if !c.State.IsStored() {
		return nil, fmt.Errorf("copy %q is in state %q, which is never stored", c.CopyID, c.State)
	}

	item := dynamoCopy{
		CopyID:              c.CopyID,
		Mechanism:           string(model.MechanismS3),
		Kind:                string(kindCopy),
		DatasetURI:          c.DatasetURI,
		HotCacheBucket:      c.HotCacheBucket,
		KeyArn:              c.KeyArn,
		State:               string(c.State),
		CreatedAt:           formatLedgerTime(c.CreatedAt),
		CopyExecutionArn:    c.CopyExecutionArn,
		ObjectCount:         c.ObjectCount,
		TotalBytes:          c.TotalBytes,
		CopySummaryLocation: c.CopySummaryLocation,
		PurgedAt:            formatLedgerTimePtr(c.PurgedAt),
		KeyDeletionDate:     formatLedgerTimePtr(c.KeyDeletionDate),
		DeletedAt:           formatLedgerTimePtr(c.DeletedAt),
	}

	return attributevalue.MarshalMap(item)
}

// unmarshalCopy converts a ledger item into a Copy. An item of the htsget mechanism yields
// errOtherMechanism, and one of this mechanism that is not a copy yields errOtherKind.
func unmarshalCopy(item map[string]types.AttributeValue) (*Copy, error) {
	var stored dynamoCopy
	if err := attributevalue.UnmarshalMap(item, &stored); err != nil {
		return nil, fmt.Errorf("unmarshaling copy: %w", err)
	}

	if mechanism := model.StoredMechanism(stored.Mechanism); mechanism != model.MechanismS3 {
		return nil, fmt.Errorf("%w: %q belongs to the %s mechanism", errOtherMechanism, stored.CopyID, mechanism)
	}
	if err := checkKind(stored.CopyID, stored.Kind, kindCopy); err != nil {
		return nil, err
	}

	state, err := ParseCopyState(stored.State)
	if err != nil {
		return nil, fmt.Errorf("copy %q: %w", stored.CopyID, err)
	}

	createdAt, err := parseLedgerTime(createdAtAttribute, stored.CreatedAt)
	if err != nil {
		return nil, err
	}

	c := &Copy{
		CopyID:              stored.CopyID,
		DatasetURI:          stored.DatasetURI,
		HotCacheBucket:      stored.HotCacheBucket,
		KeyArn:              stored.KeyArn,
		State:               state,
		CreatedAt:           createdAt,
		CopyExecutionArn:    stored.CopyExecutionArn,
		ObjectCount:         stored.ObjectCount,
		TotalBytes:          stored.TotalBytes,
		CopySummaryLocation: stored.CopySummaryLocation,
	}

	for _, optional := range []struct {
		attribute string
		stored    *string
		field     **time.Time
	}{
		{purgedAtAttribute, stored.PurgedAt, &c.PurgedAt},
		{keyDeletionDateAttribute, stored.KeyDeletionDate, &c.KeyDeletionDate},
		{deletedAtAttribute, stored.DeletedAt, &c.DeletedAt},
	} {
		parsed, err := parseOptionalLedgerTime(optional.attribute, optional.stored)
		if err != nil {
			return nil, err
		}
		*optional.field = parsed
	}

	return c, nil
}

// itemKey builds the primary key of a copy or a grant. Both kinds live under the same
// partition key, so an identifier locates a record without saying which kind it is; what
// comes back is checked for its kind before it is read as anything.
func itemKey(recordID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		recordIDAttribute: &types.AttributeValueMemberS{Value: recordID},
	}
}

// formatLedgerTime renders a timestamp the way the shared table stores them: RFC3339 in
// UTC. Normalising the zone matters for the creation time, which is the sort key of the
// per-researcher index and so has to order lexicographically as well as chronologically
// across every kind of record in the table.
func formatLedgerTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func formatLedgerTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := formatLedgerTime(*t)
	return &formatted
}

// parseLedgerTime reads a stored timestamp, naming the attribute it came from so a
// malformed item says which field is wrong.
func parseLedgerTime(attribute, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %s: %w", attribute, err)
	}
	return parsed, nil
}

func parseOptionalLedgerTime(attribute string, value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := parseLedgerTime(attribute, *value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
