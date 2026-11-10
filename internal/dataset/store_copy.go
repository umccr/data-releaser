package dataset

// This file is the read and write path for copy records in the shared ledger table, and
// at its foot the plumbing that path shares with the grant one.
//
// A copy is created before a transfer that runs for hours and is billed for it, reports
// once, and is never rewritten afterwards except to record that the key encrypting it is
// being destroyed. The moves its lifecycle forbids are dangerous rather than merely wrong,
// and the dangerous one above all is reporting a transfer that did not finish as one that
// delivered: a bucket holding some of a dataset looks exactly like a bucket holding all of
// it. So a state change is checked against the lifecycle before it is attempted, and then
// written conditionally on the state the check was made against, which leaves a record
// that changed underneath us alone rather than overwriting it.
//
// The copies of one dataset are found through the dataset index. Grants populate that
// index too, so unlike the query it replaces this one is narrowed by the kind attribute: a
// grant answering "has this dataset been copied?" would report a transfer that some other
// record performed, and staging would then decline to copy a dataset nothing had copied.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/oklog/ulid/v2"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/model"
)

// Index names on the shared table.
const (
	// ResearcherIndexName is keyed on the researcher account and the creation time.
	// Both mechanisms write those attributes, so it answers "what does this account
	// have?" across both and a query over it has to be filtered down to one.
	ResearcherIndexName = "GSI2"

	// DatasetIndexName is keyed on the dataset URI and the creation time. Only this
	// mechanism's records carry a dataset URI, so the other mechanism cannot reach a
	// read through it -- but both of this mechanism's kinds do, so a read of it still
	// has to say which kind it wants.
	DatasetIndexName = "GSI3"
)

// ErrCopyNotFound reports that no copy with the given identifier exists. An identifier
// naming a grant, or a record of the other mechanism, reports as absent too: it exists,
// but not as anything this store can act on.
var ErrCopyNotFound = errors.New("copy not found")

// ForbiddenCopyTransitionError reports a state change the lifecycle does not allow, naming
// the state the copy is actually in. Nothing was written, so the copy still holds whatever
// the report says it does.
type ForbiddenCopyTransitionError struct {
	CopyID string
	// Current is the state the copy is in.
	Current CopyState
	// Requested is the state the caller asked for.
	Requested CopyState
	// PermittedFrom lists the states the requested change may be made from, so the
	// report says what would have had to be true rather than only that it was not.
	PermittedFrom []CopyState
}

func (e *ForbiddenCopyTransitionError) Error() string {
	if e.Current == e.Requested {
		// A repeated report of an outcome already recorded, rather than a move to
		// somewhere else. Saying so is what lets a caller treat it as nothing to do,
		// which matters here because a copy reports through a completion callback that
		// may well deliver the same result twice.
		return fmt.Sprintf("copy %q is already in state %q", e.CopyID, e.Current)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "copy %q is in state %q and cannot become %q", e.CopyID, e.Current, e.Requested)
	if len(e.PermittedFrom) == 0 {
		return b.String()
	}
	names := make([]string, 0, len(e.PermittedFrom))
	for _, state := range e.PermittedFrom {
		names = append(names, string(state))
	}
	fmt.Fprintf(&b, "; that is only permitted from state %s", joinAlternatives(names))
	return b.String()
}

// CopyStore is the ledger for copies: it creates them, moves them through the lifecycle,
// and answers what has been transferred, for one dataset or for all of them.
type CopyStore struct {
	client    internalaws.DynamoDBClient
	tableName string

	// now and newID are indirected so a test can pin the timestamps and identifiers a
	// write produces. Nothing outside this package sets them.
	now   func() time.Time
	newID func() string
}

// NewCopyStore returns a store backed by the given table.
func NewCopyStore(client internalaws.DynamoDBClient, tableName string) *CopyStore {
	return &CopyStore{
		client:    client,
		tableName: tableName,
		now:       func() time.Time { return time.Now().UTC() },
		newID:     newLedgerID,
	}
}

// newLedgerID mints an identifier for a copy or a grant. A ULID sorts by the time it was
// created, which the other mechanism's records also rely on, so identifiers from every
// kind of record in the table order consistently in a listing.
func newLedgerID() string {
	return ulid.Make().String()
}

// CreateCopyInput describes the transfer to record. Everything here is known before it
// starts; what it delivered is recorded when it reports, and who may read it is not this
// record's business at all.
type CreateCopyInput struct {
	// DatasetURI identifies the objects in the source store that are copied as a unit.
	DatasetURI string
	// HotCacheBucket is the bucket the copy writes into.
	HotCacheBucket string
	// KeyArn is the key encrypting that bucket, and only that bucket.
	KeyArn string
	// CopyExecutionArn references the execution performing the transfer. Expected, since
	// a transfer running with no record of it is untraceable, but a copy is worth
	// recording even when the reference could not be obtained.
	CopyExecutionArn string
}

// Create records a new copy in the copying state, under an identifier of its own minting,
// and returns it.
//
// The caller is responsible for having checked that the dataset does not already have a
// copy worth reusing; this writes what it is given.
func (s *CopyStore) Create(ctx context.Context, input CreateCopyInput) (*Copy, error) {
	c := &Copy{
		CopyID:           s.newID(),
		DatasetURI:       input.DatasetURI,
		HotCacheBucket:   input.HotCacheBucket,
		KeyArn:           input.KeyArn,
		State:            CopyStateCopying,
		CreatedAt:        s.now(),
		CopyExecutionArn: input.CopyExecutionArn,
	}
	if err := s.Record(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Record writes a copy whose identifier and timestamp the caller has already fixed.
//
// Submitting a transfer needs that. The identifier a transfer is submitted under is also
// the folder its artefacts are written to and the name of the execution performing them,
// all three decided before there is anything to record, so the record has to carry the
// identifier the transfer already runs under rather than one minted here — otherwise a
// ledger record would not locate the copy it is waiting on.
//
// The write is conditional on the identifier being unused, so resubmitting a transfer that
// has already been recorded is refused instead of overwriting it, and neither this nor
// Create can land on top of a grant or a record of the other mechanism.
func (s *CopyStore) Record(ctx context.Context, c *Copy) error {
	if c == nil {
		return errors.New("a copy is required to record one")
	}

	// The dataset URI is the key the copies of one dataset are found through, and the
	// bucket and key are what a grant's policies are rendered against. A copy missing any
	// of them is refused here, where the complaint can name the field, rather than
	// surfacing later as a rejected write or an unrenderable policy.
	for _, required := range []struct {
		field string
		value string
	}{
		{"dataset URI", c.DatasetURI},
		{"hot cache bucket", c.HotCacheBucket},
		{"encryption key ARN", c.KeyArn},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("a copy needs a %s", required.field)
		}
	}

	item, err := marshalCopy(c)
	if err != nil {
		return fmt.Errorf("marshaling copy: %w", err)
	}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                aws.String(s.tableName),
		Item:                     item,
		ConditionExpression:      aws.String("attribute_not_exists(#copyId)"),
		ExpressionAttributeNames: map[string]string{"#copyId": recordIDAttribute},
	})
	if err != nil {
		var taken *types.ConditionalCheckFailedException
		if errors.As(err, &taken) {
			return fmt.Errorf("identifier %q is already in use", c.CopyID)
		}
		return fmt.Errorf("recording copy: %w", err)
	}

	return nil
}

// Get returns one copy by its identifier.
func (s *CopyStore) Get(ctx context.Context, copyID string) (*Copy, error) {
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key:       itemKey(copyID),
	})
	if err != nil {
		return nil, fmt.Errorf("reading copy %q: %w", copyID, err)
	}
	if output.Item == nil {
		return nil, fmt.Errorf("%w: %q", ErrCopyNotFound, copyID)
	}

	c, err := unmarshalCopy(output.Item)
	switch {
	case errors.Is(err, errOtherMechanism):
		// The identifier exists but names a record of the other mechanism. Saying which
		// mechanism it belongs to is the difference between an operator retyping the
		// identifier and reaching for the other leaf of the command.
		return nil, fmt.Errorf("%w: %q is a record of the %s mechanism",
			ErrCopyNotFound, copyID, model.MechanismHtsget)
	case errors.Is(err, errOtherKind):
		// A grant identifier used where a copy was wanted, most likely. The wrapped
		// message says which it is, so the operator knows which command to reach for.
		return nil, fmt.Errorf("%w: %s", ErrCopyNotFound, err)
	}
	return c, err
}

// CopyOutcome is what a finished transfer reports about itself. One that failed part way
// through reports what it managed, so these are not only populated on success.
type CopyOutcome struct {
	ObjectCount int64
	TotalBytes  int64
	// SummaryLocation is where the transfer wrote its own account of which objects
	// staged. It is recorded in the ledger because the copy service's execution history
	// does not persist indefinitely, and afterwards this is the only durable account of
	// it.
	SummaryLocation string
}

// applyCopyOutcome records what a transfer delivered. The fields are written whatever
// their values, so a re-reported outcome does not leave earlier numbers standing.
func applyCopyOutcome(u *itemUpdate, outcome CopyOutcome) {
	u.setNumber(objectCountAttribute, outcome.ObjectCount)
	u.setNumber(totalBytesAttribute, outcome.TotalBytes)
	u.setString(copySummaryLocationAttribute, outcome.SummaryLocation)
}

// MarkStaged records a transfer that delivered the dataset: the objects are in the cache
// bucket, and access to them can now be granted.
func (s *CopyStore) MarkStaged(ctx context.Context, copyID string, outcome CopyOutcome) (*Copy, error) {
	return s.transition(ctx, copyID, copyStateChange{
		to:    CopyStateStaged,
		from:  []CopyState{CopyStateCopying},
		apply: func(u *itemUpdate) { applyCopyOutcome(u, outcome) },
	})
}

// MarkFailed records a transfer that did not finish. The state is distinct from staged
// precisely so that a partial copy can never be granted, and whatever the transfer managed
// before failing is recorded so the failure can be investigated.
func (s *CopyStore) MarkFailed(ctx context.Context, copyID string, outcome CopyOutcome) (*Copy, error) {
	return s.transition(ctx, copyID, copyStateChange{
		to:    CopyStateFailed,
		from:  []CopyState{CopyStateCopying},
		apply: func(u *itemUpdate) { applyCopyOutcome(u, outcome) },
	})
}

// MarkPurged records that deletion of the key encrypting this copy has been scheduled,
// together with the date on which that becomes irreversible.
//
// It applies from any state the copy has reached, because the key is destroyed per dataset
// rather than per copy: a transfer still running, or one that failed, is as purged as one
// that delivered.
func (s *CopyStore) MarkPurged(ctx context.Context, copyID string, keyDeletionDate time.Time) (*Copy, error) {
	purgedAt := s.now()
	return s.transition(ctx, copyID, copyStateChange{
		to: CopyStatePurged,
		apply: func(u *itemUpdate) {
			u.setTime(purgedAtAttribute, purgedAt)
			u.setTime(keyDeletionDateAttribute, keyDeletionDate)
		},
	})
}

// MarkRestored records that a purged copy is readable again: the deletion of its key was
// cancelled inside the pending window, so the objects it staged can be decrypted once
// more and a grant can be made against them.
//
// It applies from purged and nowhere else. The other states have nothing to take back --
// a copy that never delivered was never granted, and one whose objects have been deleted
// has no way back at all -- and narrowing it here is what stops this becoming a way to
// talk a failed transfer into reporting that it delivered.
//
// The purge timestamps are cleared rather than left standing. A copy that is staged again
// is not purged, and a record carrying both the state that says it can be granted and the
// date its destruction becomes irreversible would say two things at once; a listing would
// have to pick one of them to believe.
//
// Nothing is said here about who may read it. The grants the purge withdrew stay
// withdrawn, which is the point of a grant being its own record: restoring the copy hands
// nobody their access back, and a fresh grant is an explicit act.
func (s *CopyStore) MarkRestored(ctx context.Context, copyID string) (*Copy, error) {
	return s.transition(ctx, copyID, copyStateChange{
		to:   CopyStateStaged,
		from: []CopyState{CopyStatePurged},
		apply: func(u *itemUpdate) {
			u.remove(purgedAtAttribute)
			u.remove(keyDeletionDateAttribute)
		},
	})
}

// MarkDeleted records that the objects this copy staged have been deleted out of the
// cache bucket.
//
// It applies from the two states the lifecycle permits it from -- purged and failed -- and
// from no others, because nobody is reading what either of those left behind. Which of
// them the copy is in is the store's to check rather than the caller's, so a copy that
// moved between the caller's reading of it and this write is refused instead of having a
// deletion recorded against a dataset somebody had meanwhile been given.
//
// No companion date is written the way a purge writes one, because there is no window to
// report: the objects are gone by the time this records it.
func (s *CopyStore) MarkDeleted(ctx context.Context, copyID string) (*Copy, error) {
	deletedAt := s.now()
	return s.transition(ctx, copyID, copyStateChange{
		to: CopyStateDeleted,
		apply: func(u *itemUpdate) {
			u.setTime(deletedAtAttribute, deletedAt)
		},
	})
}

// copyStateChange describes one move through the lifecycle.
type copyStateChange struct {
	// to is the state being moved to.
	to CopyState
	// from narrows the states the move may be made from, beyond what the lifecycle
	// allows. Empty means every state the lifecycle permits -- which is what a purge
	// wants, and what a transition recording a specific event does not.
	from []CopyState
	// apply adds the attributes this move records.
	apply func(u *itemUpdate)
}

// permittedFrom lists the states this move may be made from, in lifecycle order.
func (c copyStateChange) permittedFrom() []CopyState {
	permitted := make([]CopyState, 0, len(copyStates))
	for _, from := range copyStates {
		if !slices.Contains(allowedCopyTransitions[from], c.to) {
			continue
		}
		if len(c.from) > 0 && !slices.Contains(c.from, from) {
			continue
		}
		permitted = append(permitted, from)
	}
	return permitted
}

// transition moves a copy to a new state, recording whatever that move carries with it.
//
// The state is checked before the write and then made a condition of it, so a move the
// lifecycle forbids is refused without touching the record, and a move that was legal when
// it was checked but not by the time it was written is refused the same way rather than
// overwriting whatever happened in between.
func (s *CopyStore) transition(ctx context.Context, copyID string, change copyStateChange) (*Copy, error) {
	c, err := s.Get(ctx, copyID)
	if err != nil {
		return nil, err
	}

	permitted := change.permittedFrom()
	refuse := func(current CopyState) error {
		return &ForbiddenCopyTransitionError{
			CopyID:        copyID,
			Current:       current,
			Requested:     change.to,
			PermittedFrom: permitted,
		}
	}
	if !slices.Contains(permitted, c.State) {
		return nil, refuse(c.State)
	}

	u := newItemUpdate()
	u.setString(stateAttribute, string(change.to))
	if change.apply != nil {
		change.apply(u)
	}
	u.expect(stateAttribute, string(c.State))

	return s.write(ctx, copyID, u, refuse)
}

// write applies an update and returns the copy as it now stands.
//
// The record is read back out of the write itself, so what is returned cannot drift from
// what was stored. A refused condition means the copy changed between the read the update
// was built from and the write, so it is re-read and reported in the state it is actually
// in.
func (s *CopyStore) write(
	ctx context.Context,
	copyID string,
	u *itemUpdate,
	refuse func(current CopyState) error,
) (*Copy, error) {
	output, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.tableName),
		Key:                       itemKey(copyID),
		UpdateExpression:          aws.String(u.expression()),
		ConditionExpression:       u.condition(),
		ExpressionAttributeNames:  u.names,
		ExpressionAttributeValues: u.values,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		var changed *types.ConditionalCheckFailedException
		if errors.As(err, &changed) {
			current, getErr := s.Get(ctx, copyID)
			if getErr != nil {
				return nil, getErr
			}
			return nil, refuse(current.State)
		}
		return nil, fmt.Errorf("updating copy %q: %w", copyID, err)
	}

	if len(output.Attributes) == 0 {
		return s.Get(ctx, copyID)
	}
	return unmarshalCopy(output.Attributes)
}

// CopiesOfDataset returns every copy of one dataset, newest first, whatever state each is
// in.
//
// Both callers need all of them: staging has to see a copy that already covers the dataset
// rather than starting a second billed transfer alongside it, and purge has to name every
// copy the destruction of the key will take with it.
func (s *CopyStore) CopiesOfDataset(ctx context.Context, datasetURI string) ([]*Copy, error) {
	if strings.TrimSpace(datasetURI) == "" {
		return nil, errors.New("a dataset URI is needed to find its copies")
	}

	copies, err := queryLedger(ctx, s.client, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(DatasetIndexName),
		KeyConditionExpression: aws.String("#datasetUri = :datasetUri"),
		// Grants of this dataset sit in the same index under the same key, so the query
		// has to say which kind it wants.
		FilterExpression: aws.String(recordsOfKind),
		ExpressionAttributeNames: withKindName(map[string]string{
			"#datasetUri": datasetURIAttribute,
		}),
		ExpressionAttributeValues: withKind(map[string]types.AttributeValue{
			":datasetUri": stringValue(datasetURI),
		}, kindCopy),
		ScanIndexForward: aws.Bool(false),
	}, unmarshalCopy)
	if err != nil {
		return nil, fmt.Errorf("querying copies of dataset %q: %w", datasetURI, err)
	}
	return copies, nil
}

// ListAll returns every copy, newest first, including the failed and the purged: a listing
// that hid them would answer "what can be granted?" and not "what has been transferred?".
func (s *CopyStore) ListAll(ctx context.Context) ([]*Copy, error) {
	copies, err := scanLedger(ctx, s.client, &dynamodb.ScanInput{
		TableName:                 aws.String(s.tableName),
		FilterExpression:          aws.String(recordsOfKind),
		ExpressionAttributeNames:  withKindName(nil),
		ExpressionAttributeValues: withKind(nil, kindCopy),
	}, unmarshalCopy)
	if err != nil {
		return nil, fmt.Errorf("scanning copies: %w", err)
	}

	// A scan returns items in no useful order, so the ordering the indexes give for free
	// has to be imposed here to match.
	slices.SortFunc(copies, copiesNewestFirst)
	return copies, nil
}

// copiesNewestFirst orders copies by creation time descending, falling back to the
// identifier so that copies created in the same second still order deterministically.
func copiesNewestFirst(a, b *Copy) int {
	if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
		return order
	}
	return strings.Compare(b.CopyID, a.CopyID)
}

// recordsOfKind narrows a read of the shared table to one kind of this mechanism's
// records.
//
// The mechanism clause keeps the other mechanism's items out; the kind clause keeps copies
// and grants apart, and keeps out the records written before the two were separate, which
// carry no kind at all and are neither.
//
// An absent mechanism attribute does not qualify: every record of this mechanism writes
// it, and the records that predate the shared table are all the other mechanism's.
const recordsOfKind = "#mechanism = :releaseMechanism AND #kind = :kind"

// withMechanismName adds the mechanism alias recordsOfKind names to a read's own
// attribute names.
func withMechanismName(names map[string]string) map[string]string {
	merged := make(map[string]string, len(names)+2)
	for alias, name := range names {
		merged[alias] = name
	}
	merged["#mechanism"] = model.MechanismAttribute
	return merged
}

// withReleaseMechanism adds the mechanism recordsOfKind compares against to a read's own
// expression values.
func withReleaseMechanism(values map[string]types.AttributeValue) map[string]types.AttributeValue {
	merged := make(map[string]types.AttributeValue, len(values)+2)
	for placeholder, value := range values {
		merged[placeholder] = value
	}
	merged[":releaseMechanism"] = stringValue(string(model.MechanismS3))
	return merged
}

// withKindName adds the alias recordsOfKind names to a read's own attribute names.
func withKindName(names map[string]string) map[string]string {
	merged := withMechanismName(names)
	merged["#kind"] = kindAttribute
	return merged
}

// withKind adds the kind recordsOfKind compares against to a read's own expression values.
func withKind(values map[string]types.AttributeValue, kind itemKind) map[string]types.AttributeValue {
	merged := withReleaseMechanism(values)
	merged[":kind"] = stringValue(string(kind))
	return merged
}

// The rest of this file is the plumbing the two kinds of record share: the walk over a
// read's pages, the accumulator an update expression is built in, and the phrasing a
// refusal reports its alternatives in. It lives here rather than beside either kind
// because both stores are written against it and neither owns it.
//
// A query or a scan for one kind differs from the other only in what an item unmarshals
// to, so the walk over the pages is written once and given the conversion to use.

// queryLedger reads every page of a query and returns the records in the order the index
// gave them.
//
// Pagination is driven to exhaustion rather than reading the first page: the callers that
// ask about a dataset act on what comes back, and a partial answer would look exactly like
// a dataset with fewer records than it has.
func queryLedger[T any](
	ctx context.Context,
	client internalaws.DynamoDBClient,
	input *dynamodb.QueryInput,
	unmarshal func(map[string]types.AttributeValue) (*T, error),
) ([]*T, error) {
	paginator := dynamodb.NewQueryPaginator(client, input)

	var records []*T
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		batch, err := ledgerRecords(page.Items, unmarshal)
		if err != nil {
			return nil, err
		}
		records = append(records, batch...)
	}
	return records, nil
}

// scanLedger reads every page of a scan, for the listings that are not keyed on anything.
func scanLedger[T any](
	ctx context.Context,
	client internalaws.DynamoDBClient,
	input *dynamodb.ScanInput,
	unmarshal func(map[string]types.AttributeValue) (*T, error),
) ([]*T, error) {
	paginator := dynamodb.NewScanPaginator(client, input)

	var records []*T
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		batch, err := ledgerRecords(page.Items, unmarshal)
		if err != nil {
			return nil, err
		}
		records = append(records, batch...)
	}
	return records, nil
}

// ledgerRecords converts a page of items, dropping any that belongs to the other mechanism
// or to the other kind.
//
// Every read that lands here is filtered on both, so a drop means the filter did not take
// effect -- through a double, or through a future caller that forgot it. Dropping such an
// item is preferable to reporting a grant as a transfer that never happened, or a copy as
// access nobody was given.
func ledgerRecords[T any](
	items []map[string]types.AttributeValue,
	unmarshal func(map[string]types.AttributeValue) (*T, error),
) ([]*T, error) {
	records := make([]*T, 0, len(items))
	for _, item := range items {
		record, err := unmarshal(item)
		if errors.Is(err, errOtherMechanism) || errors.Is(err, errOtherKind) {
			continue
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// joinAlternatives renders a list of names as "a", "a or b", or "a, b, or c", so a
// refusal reads as a sentence rather than as a slice.
func joinAlternatives(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return strconv.Quote(names[0])
	}

	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	if len(quoted) == 2 {
		return quoted[0] + " or " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1]
}

// itemUpdate accumulates one update expression.
//
// Every attribute name is aliased. One of the names these records write is a word
// DynamoDB reserves, and aliasing all of them means never having to know which, nor
// discovering it from a rejected write.
type itemUpdate struct {
	sets       []string
	removes    []string
	conditions []string
	names      map[string]string
	values     map[string]types.AttributeValue
}

func newItemUpdate() *itemUpdate {
	return &itemUpdate{
		names:  make(map[string]string),
		values: make(map[string]types.AttributeValue),
	}
}

// set writes a value to an attribute.
func (u *itemUpdate) set(attribute string, value types.AttributeValue) {
	alias, placeholder := "#"+attribute, ":"+attribute
	u.names[alias] = attribute
	u.values[placeholder] = value
	u.sets = append(u.sets, alias+" = "+placeholder)
}

func (u *itemUpdate) setString(attribute, value string) {
	u.set(attribute, stringValue(value))
}

func (u *itemUpdate) setNumber(attribute string, value int64) {
	u.set(attribute, &types.AttributeValueMemberN{Value: strconv.FormatInt(value, 10)})
}

// setTime writes a timestamp in the form the shared table stores them.
func (u *itemUpdate) setTime(attribute string, value time.Time) {
	u.setString(attribute, formatLedgerTime(value))
}

// setOptionalTime writes a timestamp, or removes the attribute when there is none, so
// that an absent value clears whatever was there rather than leaving it standing.
func (u *itemUpdate) setOptionalTime(attribute string, value *time.Time) {
	if value == nil {
		u.remove(attribute)
		return
	}
	u.setTime(attribute, *value)
}

// remove clears an attribute.
func (u *itemUpdate) remove(attribute string) {
	alias := "#" + attribute
	u.names[alias] = attribute
	u.removes = append(u.removes, alias)
}

// expect requires that the attribute still holds the given value for the update to
// apply.
func (u *itemUpdate) expect(attribute, value string) {
	alias, placeholder := "#"+attribute, ":expected_"+attribute
	u.names[alias] = attribute
	u.values[placeholder] = stringValue(value)
	u.conditions = append(u.conditions, alias+" = "+placeholder)
}

// expression renders the accumulated clauses.
func (u *itemUpdate) expression() string {
	clauses := make([]string, 0, 2)
	if len(u.sets) > 0 {
		clauses = append(clauses, "SET "+strings.Join(u.sets, ", "))
	}
	if len(u.removes) > 0 {
		clauses = append(clauses, "REMOVE "+strings.Join(u.removes, ", "))
	}
	return strings.Join(clauses, " ")
}

// condition renders the accumulated conditions, or nil when there are none.
func (u *itemUpdate) condition() *string {
	if len(u.conditions) == 0 {
		return nil
	}
	return aws.String(strings.Join(u.conditions, " AND "))
}

func stringValue(value string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: value}
}
