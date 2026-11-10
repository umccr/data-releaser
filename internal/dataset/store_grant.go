package dataset

// This file is the read and write path for grant records in the shared ledger table.
//
// A grant has a much shorter life than the copy it reads from. It is written in one go
// from a copy that has already staged, its expiry may be moved for as long as it stands,
// and it ends once -- withdrawn, and stays withdrawn. There is deliberately no way back:
// restoring access is a new grant with its own record, because the withdrawal of this one
// is part of the account of what happened and rewriting it would erase that.
//
// Several grants may be live on one copy at once, so nothing here is keyed on the copy
// being granted only once. The grants of a copy are found through the dataset index and
// narrowed on the copy identifier, which keeps the read within one dataset's records
// rather than adding an index for a question asked about a handful of items.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/model"
)

// ErrGrantNotFound reports that no grant with the given identifier exists. An identifier
// naming a copy, or a record of the other mechanism, reports as absent too: it exists, but
// not as anything this store can act on.
var ErrGrantNotFound = errors.New("grant not found")

// ErrUnlimitedGrant reports an attempt to move the expiry of a grant that has none. An
// unlimited grant carries no time condition in its deployed policies, so there is nothing
// to move; withdrawing its access is a revocation, not an expiry.
var ErrUnlimitedGrant = errors.New("grant is unlimited, so no expiry applies")

// NotGrantableError reports an attempt to grant access to a copy that did not deliver the
// dataset, naming the state the copy is in. Nothing was written.
//
// The state that matters is failed: a bucket holding part of a dataset looks exactly like
// one holding all of it, so the only thing standing between a researcher and a truncated
// dataset is that the copy said what happened to it.
type NotGrantableError struct {
	CopyID string
	// Current is the state the copy is in.
	Current CopyState
}

func (e *NotGrantableError) Error() string {
	return fmt.Sprintf("copy %q is in state %q, so there is nothing to grant access to; "+
		"only a copy in state %q has delivered its dataset",
		e.CopyID, e.Current, CopyStateStaged)
}

// AlreadyRevokedError reports an attempt to withdraw a grant that was withdrawn already.
// Nothing was written, and in particular the recorded time of the withdrawal is not moved:
// a second attempt is a repeat of an outcome the ledger holds, which a caller can read as
// nothing to do.
type AlreadyRevokedError struct {
	GrantID string
}

func (e *AlreadyRevokedError) Error() string {
	return fmt.Sprintf("grant %q is in state %q already", e.GrantID, GrantStateRevoked)
}

// GrantNotExtendableError reports that the expiry of a grant may no longer be moved, naming
// the state that closed it off. Nothing was written.
type GrantNotExtendableError struct {
	GrantID string
	// Current is the state the grant is in.
	Current GrantState
}

func (e *GrantNotExtendableError) Error() string {
	return fmt.Sprintf("grant %q is in state %q, so its expiry can no longer be moved",
		e.GrantID, e.Current)
}

// GrantStore is the ledger for grants: it writes them against a copy, moves their expiry,
// withdraws them, and answers who holds access -- to one copy, to one dataset, or across
// the whole table.
type GrantStore struct {
	client    internalaws.DynamoDBClient
	tableName string

	// now and newID are indirected so a test can pin the timestamps and identifiers a
	// write produces. Nothing outside this package sets them.
	now   func() time.Time
	newID func() string
}

// NewGrantStore returns a store backed by the given table.
func NewGrantStore(client internalaws.DynamoDBClient, tableName string) *GrantStore {
	return &GrantStore{
		client:    client,
		tableName: tableName,
		now:       func() time.Time { return time.Now().UTC() },
		newID:     newLedgerID,
	}
}

// GrantRequest is the access being asked for: who may read, from where, and until when.
// What is read, and where it lives, come from the copy the grant is made against.
type GrantRequest struct {
	// ResearcherAccountID is the external account permitted to read.
	ResearcherAccountID string
	// TargetVpcID is the bare VPC identifier reads must originate from.
	TargetVpcID string
	// ExpiresAt is when access ceases. Nil grants unlimited access, and the deployed
	// policies then carry no time condition at all rather than a far-future one.
	ExpiresAt *time.Time
}

// Create records access to one copy for one researcher, and returns the grant.
//
// It takes the copy rather than its identifier because a grant carries the copy's dataset,
// bucket and key alongside its own fields, so that it can render its own policies and print
// its own row without a join. Those cannot change for a given copy, so copying them here is
// not a value that can go stale.
//
// A copy that has not staged is refused: the transfer either delivered the dataset or it
// did not, and granting access to a partial one is the mistake this whole distinction
// exists to prevent.
//
// It is the ledger side of applying the policies, and runs after them: the policies are
// what a request is checked against, and this is the record of what was intended, which
// reconciliation compares them to.
func (s *GrantStore) Create(ctx context.Context, c *Copy, request GrantRequest) (*Grant, error) {
	if c == nil {
		return nil, errors.New("a copy is required to grant access to one")
	}
	if !c.IsGrantable() {
		return nil, &NotGrantableError{CopyID: c.CopyID, Current: c.State}
	}

	grant := &Grant{
		GrantID:             s.newID(),
		CopyID:              c.CopyID,
		DatasetURI:          c.DatasetURI,
		HotCacheBucket:      c.HotCacheBucket,
		KeyArn:              c.KeyArn,
		ResearcherAccountID: request.ResearcherAccountID,
		TargetVpcID:         request.TargetVpcID,
		State:               GrantStateGranted,
		CreatedAt:           s.now(),
		ExpiresAt:           request.ExpiresAt,
	}

	// A grant naming no account or no VPC would be recorded happily and then render a
	// policy granting nothing, or worse, one whose condition is absent. The shape of an
	// account or a VPC identifier is the renderer's business; that they are present at
	// all is this record's. The three fields taken from the copy are checked for the same
	// reason they are on a copy: a grant missing any of them is found by nothing and
	// renders nothing.
	for _, required := range []struct {
		field string
		value string
	}{
		{"copy to read from", grant.CopyID},
		{"dataset URI", grant.DatasetURI},
		{"hot cache bucket", grant.HotCacheBucket},
		{"encryption key ARN", grant.KeyArn},
		{"researcher account to grant to", grant.ResearcherAccountID},
		{"VPC reads are permitted from", grant.TargetVpcID},
	} {
		if strings.TrimSpace(required.value) == "" {
			return nil, fmt.Errorf("a grant needs a %s", required.field)
		}
	}

	item, err := marshalGrant(grant)
	if err != nil {
		return nil, fmt.Errorf("marshaling grant: %w", err)
	}

	// Conditional on the identifier being unused, so a grant can never land on top of a
	// copy, a grant, or a record of the other mechanism.
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                aws.String(s.tableName),
		Item:                     item,
		ConditionExpression:      aws.String("attribute_not_exists(#grantId)"),
		ExpressionAttributeNames: map[string]string{"#grantId": recordIDAttribute},
	})
	if err != nil {
		var taken *types.ConditionalCheckFailedException
		if errors.As(err, &taken) {
			return nil, fmt.Errorf("identifier %q is already in use", grant.GrantID)
		}
		return nil, fmt.Errorf("recording grant: %w", err)
	}

	return grant, nil
}

// Get returns one grant by its identifier.
func (s *GrantStore) Get(ctx context.Context, grantID string) (*Grant, error) {
	output, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key:       itemKey(grantID),
	})
	if err != nil {
		return nil, fmt.Errorf("reading grant %q: %w", grantID, err)
	}
	if output.Item == nil {
		return nil, fmt.Errorf("%w: %q", ErrGrantNotFound, grantID)
	}

	grant, err := unmarshalGrant(output.Item)
	switch {
	case errors.Is(err, errOtherMechanism):
		// The identifier exists but names a record of the other mechanism. Saying which
		// mechanism it belongs to is the difference between an operator retyping the
		// identifier and reaching for the other leaf of the command.
		return nil, fmt.Errorf("%w: %q is a record of the %s mechanism",
			ErrGrantNotFound, grantID, model.MechanismHtsget)
	case errors.Is(err, errOtherKind):
		// A copy identifier used where a grant was wanted, most likely -- the operand of
		// extend and revoke is the grant, not the copy it reads from. The wrapped message
		// says which it is.
		return nil, fmt.Errorf("%w: %s", ErrGrantNotFound, err)
	}
	return grant, err
}

// Extend moves the expiry of a grant to the given duration from now, which is what makes a
// grant whose expiry has already passed extendable: its statements and its recorded expiry
// are still in place, so moving the expiry forward restores access without anything being
// transferred again.
//
// A revoked grant is refused with its current state, and an unlimited one with
// ErrUnlimitedGrant, in both cases leaving the record untouched.
func (s *GrantStore) Extend(ctx context.Context, grantID string, duration time.Duration) (*Grant, error) {
	if duration <= 0 {
		return nil, fmt.Errorf("an extension needs a positive duration, got %s", duration)
	}

	grant, err := s.Get(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if !grant.IsExtendable() {
		return nil, &GrantNotExtendableError{GrantID: grantID, Current: grant.State}
	}
	if grant.IsUnlimited() {
		return nil, fmt.Errorf("%w: %q", ErrUnlimitedGrant, grantID)
	}

	// The expiry is computed from the store's own clock rather than from the recorded one,
	// so an extension of a long-lapsed grant lands in the future rather than a little
	// further into the past.
	expiresAt := s.now().Add(duration)

	// The expiry moves but the state does not, so the state is only a condition here: a
	// grant revoked between the read and the write keeps that outcome rather than quietly
	// regaining an expiry.
	u := newItemUpdate()
	u.setTime(expiresAtAttribute, expiresAt)
	u.expect(stateAttribute, string(grant.State))

	return s.write(ctx, grantID, u, func(current GrantState) error {
		return &GrantNotExtendableError{GrantID: grantID, Current: current}
	})
}

// MarkRevoked records that this grant's statements have been taken out of the deployed
// policies. The objects, the key, and every other grant on the same copy are untouched,
// which is what separates a withdrawal from a purge.
//
// The timestamp is written once and never cleared, because there is no move out of revoked
// for it to be cleared by: granting access again is a new record.
func (s *GrantStore) MarkRevoked(ctx context.Context, grantID string) (*Grant, error) {
	grant, err := s.Get(ctx, grantID)
	if err != nil {
		return nil, err
	}

	refuse := func(GrantState) error {
		return &AlreadyRevokedError{GrantID: grantID}
	}
	if !grant.IsDeployed() {
		return nil, refuse(grant.State)
	}

	u := newItemUpdate()
	u.setString(stateAttribute, string(GrantStateRevoked))
	u.setTime(revokedAtAttribute, s.now())
	u.expect(stateAttribute, string(GrantStateGranted))

	return s.write(ctx, grantID, u, refuse)
}

// write applies an update and returns the grant as it now stands.
//
// The record is read back out of the write itself, so what is returned cannot drift from
// what was stored. A refused condition means the grant changed between the read the update
// was built from and the write, so it is re-read and reported in the state it is actually
// in.
func (s *GrantStore) write(
	ctx context.Context,
	grantID string,
	u *itemUpdate,
	refuse func(current GrantState) error,
) (*Grant, error) {
	output, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.tableName),
		Key:                       itemKey(grantID),
		UpdateExpression:          aws.String(u.expression()),
		ConditionExpression:       u.condition(),
		ExpressionAttributeNames:  u.names,
		ExpressionAttributeValues: u.values,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		var changed *types.ConditionalCheckFailedException
		if errors.As(err, &changed) {
			current, getErr := s.Get(ctx, grantID)
			if getErr != nil {
				return nil, getErr
			}
			return nil, refuse(current.State)
		}
		return nil, fmt.Errorf("updating grant %q: %w", grantID, err)
	}

	if len(output.Attributes) == 0 {
		return s.Get(ctx, grantID)
	}
	return unmarshalGrant(output.Attributes)
}

// GrantsOfCopy returns every grant made against one copy, newest first, withdrawn ones
// included.
//
// It takes the copy because the copy identifier keys no index: the read goes through the
// dataset index and is narrowed on the copy, which is bounded by one dataset's records
// instead of the whole table. Reconciliation and purge both ask this, and both need the
// withdrawn ones -- one to see statements that should no longer be deployed, the other to
// name everything the destruction of the key will take with it.
func (s *GrantStore) GrantsOfCopy(ctx context.Context, c *Copy) ([]*Grant, error) {
	if c == nil {
		return nil, errors.New("a copy is needed to find the grants against it")
	}
	if strings.TrimSpace(c.CopyID) == "" || strings.TrimSpace(c.DatasetURI) == "" {
		return nil, fmt.Errorf("a copy needs an identifier and a dataset URI to find its grants, got %q of %q",
			c.CopyID, c.DatasetURI)
	}

	grants, err := s.queryGrants(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(DatasetIndexName),
		KeyConditionExpression: aws.String("#datasetUri = :datasetUri"),
		FilterExpression:       aws.String(recordsOfKind + " AND #copyId = :copyId"),
		ExpressionAttributeNames: withKindName(map[string]string{
			"#datasetUri": datasetURIAttribute,
			"#copyId":     copyIDAttribute,
		}),
		ExpressionAttributeValues: withKind(map[string]types.AttributeValue{
			":datasetUri": stringValue(c.DatasetURI),
			":copyId":     stringValue(c.CopyID),
		}, kindGrant),
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("querying grants against copy %q: %w", c.CopyID, err)
	}
	return grants, nil
}

// GrantsOfDataset returns every grant of one dataset, newest first, across all of its
// copies. It is what answers "who has this dataset?" without the asker having to know how
// many times it was transferred.
func (s *GrantStore) GrantsOfDataset(ctx context.Context, datasetURI string) ([]*Grant, error) {
	if strings.TrimSpace(datasetURI) == "" {
		return nil, errors.New("a dataset URI is needed to find its grants")
	}

	grants, err := s.queryGrants(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(DatasetIndexName),
		KeyConditionExpression: aws.String("#datasetUri = :datasetUri"),
		// The copies of this dataset sit in the same index under the same key, so the
		// query has to say which kind it wants.
		FilterExpression: aws.String(recordsOfKind),
		ExpressionAttributeNames: withKindName(map[string]string{
			"#datasetUri": datasetURIAttribute,
		}),
		ExpressionAttributeValues: withKind(map[string]types.AttributeValue{
			":datasetUri": stringValue(datasetURI),
		}, kindGrant),
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("querying grants of dataset %q: %w", datasetURI, err)
	}
	return grants, nil
}

// ListByAccount returns every grant held by one researcher account, newest first.
func (s *GrantStore) ListByAccount(ctx context.Context, researcherAccountID string) ([]*Grant, error) {
	if strings.TrimSpace(researcherAccountID) == "" {
		return nil, errors.New("a researcher account is needed to list its grants")
	}

	grants, err := s.queryGrants(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		IndexName:              aws.String(ResearcherIndexName),
		KeyConditionExpression: aws.String("#researcherAccountId = :researcherAccountId"),
		// Both mechanisms populate this index, and a copy would be in it too if it named
		// a researcher, so the query is narrowed to this mechanism's grants.
		FilterExpression: aws.String(recordsOfKind),
		ExpressionAttributeNames: withKindName(map[string]string{
			"#researcherAccountId": researcherAccountAttribute,
		}),
		ExpressionAttributeValues: withKind(map[string]types.AttributeValue{
			":researcherAccountId": stringValue(researcherAccountID),
		}, kindGrant),
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		return nil, fmt.Errorf("querying grants of account %q: %w", researcherAccountID, err)
	}
	return grants, nil
}

// ListAll returns every grant, newest first, including the withdrawn and the expired: a
// listing that hid them would answer "who has access?" and not "what has been granted?".
func (s *GrantStore) ListAll(ctx context.Context) ([]*Grant, error) {
	grants, err := scanLedger(ctx, s.client, &dynamodb.ScanInput{
		TableName:                 aws.String(s.tableName),
		FilterExpression:          aws.String(recordsOfKind),
		ExpressionAttributeNames:  withKindName(nil),
		ExpressionAttributeValues: withKind(nil, kindGrant),
	}, unmarshalGrant)
	if err != nil {
		return nil, fmt.Errorf("scanning grants: %w", err)
	}

	// A scan returns items in no useful order, so the ordering the indexes give for free
	// has to be imposed here to match.
	slices.SortFunc(grants, grantsNewestFirst)
	return grants, nil
}

func (s *GrantStore) queryGrants(ctx context.Context, input *dynamodb.QueryInput) ([]*Grant, error) {
	return queryLedger(ctx, s.client, input, unmarshalGrant)
}

// grantsNewestFirst orders grants by creation time descending, falling back to the
// identifier so that grants made in the same second still order deterministically. Two
// grants on one copy made together is an ordinary case, not a rare one.
func grantsNewestFirst(a, b *Grant) int {
	if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
		return order
	}
	return strings.Compare(b.GrantID, a.GrantID)
}
