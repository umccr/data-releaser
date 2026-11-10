package dataset

// This file maps a grant onto an item in the ledger table, alongside the copy it reads
// from.
//
// A grant carries the same three markers a copy does -- the shared partition key, the
// mechanism, and the kind -- and differs in the indexes it belongs to. It writes the
// researcher account, which puts it in the per-researcher index and makes "what can
// account X read?" one query across both mechanisms. It writes the dataset URI too, so
// that the grants of a dataset are found the same way its copies are, and so that the
// grants of one copy can be found without an index of their own.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/model"
)

// Attribute names only a grant writes: the copy it reads from, the VPC its reads must
// come from, and the two timestamps bounding how long it stands.
const (
	// copyIDAttribute holds the copy a grant reads from. It keys no index: the grants of
	// a copy are found through the dataset index and narrowed on this, which is bounded
	// by one dataset's records rather than by the whole table.
	copyIDAttribute = "copyId"

	targetVpcAttribute = "targetVpcId"
	expiresAtAttribute = "expiresAt"
	revokedAtAttribute = "revokedAt"
)

// dynamoGrant is the ledger representation of a Grant. As with a copy, timestamps are
// RFC3339 strings and the state is its stored spelling.
//
// Nothing identifying is omitted. A grant always names an account and a VPC -- those are
// what it exists to record -- so unlike a copy, whose account attribute is absent because
// it has none, the researcher account is written unconditionally and the item always
// belongs in the per-researcher index. Only the two timestamps that may genuinely be absent are omitted: an unlimited
// grant has no expiry at all rather than a far-future one, and a grant still in force has
// not been revoked.
type dynamoGrant struct {
	GrantID string `dynamodbav:"shareId"`

	// Mechanism and Kind mark the item as this mechanism's grant record. Absence of
	// either means something else, so neither is ever omitted.
	Mechanism string `dynamodbav:"mechanism"`
	Kind      string `dynamodbav:"kind"`

	CopyID string `dynamodbav:"copyId"`

	DatasetURI     string `dynamodbav:"datasetUri"`
	HotCacheBucket string `dynamodbav:"hotCacheBucket"`
	KeyArn         string `dynamodbav:"keyArn"`

	ResearcherAccountID string `dynamodbav:"researcherAccountId"`
	TargetVpcID         string `dynamodbav:"targetVpcId"`

	// State is a reserved word in DynamoDB, so any expression naming it needs an
	// attribute-name alias.
	State string `dynamodbav:"state"`

	CreatedAt string  `dynamodbav:"createdAt"`
	ExpiresAt *string `dynamodbav:"expiresAt,omitempty"`
	RevokedAt *string `dynamodbav:"revokedAt,omitempty"`
}

// marshalGrant converts a Grant into a ledger item.
//
// The derived state is refused here, where the complaint can say why: writing it would
// discard the recorded expiry, and that expiry is the whole of what makes an expired grant
// extendable rather than dead.
func marshalGrant(g *Grant) (map[string]types.AttributeValue, error) {
	if strings.TrimSpace(g.GrantID) == "" {
		return nil, errors.New("grant has no identifier")
	}
	if !g.State.IsStored() {
		return nil, fmt.Errorf("grant %q is in state %q, which is never stored", g.GrantID, g.State)
	}

	item := dynamoGrant{
		GrantID:             g.GrantID,
		Mechanism:           string(model.MechanismS3),
		Kind:                string(kindGrant),
		CopyID:              g.CopyID,
		DatasetURI:          g.DatasetURI,
		HotCacheBucket:      g.HotCacheBucket,
		KeyArn:              g.KeyArn,
		ResearcherAccountID: g.ResearcherAccountID,
		TargetVpcID:         g.TargetVpcID,
		State:               string(g.State),
		CreatedAt:           formatLedgerTime(g.CreatedAt),
		ExpiresAt:           formatLedgerTimePtr(g.ExpiresAt),
		RevokedAt:           formatLedgerTimePtr(g.RevokedAt),
	}

	return attributevalue.MarshalMap(item)
}

// unmarshalGrant converts a ledger item into a Grant. An item of the htsget mechanism
// yields errOtherMechanism, and one of this mechanism that is not a grant yields
// errOtherKind.
func unmarshalGrant(item map[string]types.AttributeValue) (*Grant, error) {
	var stored dynamoGrant
	if err := attributevalue.UnmarshalMap(item, &stored); err != nil {
		return nil, fmt.Errorf("unmarshaling grant: %w", err)
	}

	if mechanism := model.StoredMechanism(stored.Mechanism); mechanism != model.MechanismS3 {
		return nil, fmt.Errorf("%w: %q belongs to the %s mechanism", errOtherMechanism, stored.GrantID, mechanism)
	}
	if err := checkKind(stored.GrantID, stored.Kind, kindGrant); err != nil {
		return nil, err
	}

	state, err := ParseGrantState(stored.State)
	if err != nil {
		return nil, fmt.Errorf("grant %q: %w", stored.GrantID, err)
	}

	createdAt, err := parseLedgerTime(createdAtAttribute, stored.CreatedAt)
	if err != nil {
		return nil, err
	}

	g := &Grant{
		GrantID:             stored.GrantID,
		CopyID:              stored.CopyID,
		DatasetURI:          stored.DatasetURI,
		HotCacheBucket:      stored.HotCacheBucket,
		KeyArn:              stored.KeyArn,
		ResearcherAccountID: stored.ResearcherAccountID,
		TargetVpcID:         stored.TargetVpcID,
		State:               state,
		CreatedAt:           createdAt,
	}

	for _, optional := range []struct {
		attribute string
		stored    *string
		field     **time.Time
	}{
		{expiresAtAttribute, stored.ExpiresAt, &g.ExpiresAt},
		{revokedAtAttribute, stored.RevokedAt, &g.RevokedAt},
	} {
		parsed, err := parseOptionalLedgerTime(optional.attribute, optional.stored)
		if err != nil {
			return nil, err
		}
		*optional.field = parsed
	}

	return g, nil
}
