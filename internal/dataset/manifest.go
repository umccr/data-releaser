package dataset

// This file builds and publishes the document that tells a researcher account what it
// can currently read.
//
// The document is descriptive and nothing else. A read is authorised by the cache
// bucket's policy and by the policy of the key encrypting it, both rendered from the
// ledger; nothing in that path opens this document, and editing it grants nothing. That
// is a constraint on the schema as much as on the code: it carries where the data is,
// which key decrypts it, which VPC to read from and when access ends -- what a reader
// needs in order to find the data -- and deliberately carries no permissions, no policy
// fragments, and no lifecycle detail that would invite anyone to treat it as the
// authority on what they may do.
//
// It is built from grants and not from copies. A copy is a transfer of a dataset into
// this account and says nothing about who may read what it delivered; a grant is exactly
// what the document has to describe -- one account, one dataset, one VPC, one end date --
// and it carries the bucket and the key alongside, so an entry is written from one record
// without a join.
//
// A document lists only the grants giving access at the moment it was generated. A grant
// that has expired or been withdrawn is simply absent, and an account with none left has
// no document at all: publishing removes it rather than leaving a description of access
// that has ended. The absence of an entry is therefore not evidence of anything except
// that the document was generated after the access ended.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/umccr/data-releaser/internal/manifest"
)

// manifestNote travels inside the document, because the document outlives the
// conversation in which it was handed over and will be read by people who were not part
// of it.
const manifestNote = "Descriptive only: this document records the dataset access held by this account " +
	"at the time it was generated. Access is enforced by the cache bucket's policy and by the policy of " +
	"the key encrypting it. Nothing here is consulted when a read is authorised, and changing this " +
	"document neither grants nor withdraws anything."

// datasetObjectPrefix is where a dataset's objects sit in its cache bucket.
//
// A cache bucket holds exactly one dataset, so they sit at its root and the copy
// reproduces the dataset's own folder structure from there. It is named rather than left
// implicit because the document reports it to a reader, and the value has to stay the
// one the copy actually writes to.
const datasetObjectPrefix = ""

// Manifest is the document published to one researcher account.
type Manifest struct {
	// Note says what the document is for and what it is not. It is part of the payload
	// rather than a comment in this file because the caution is only useful where the
	// document is read.
	Note string `json:"note"`

	// AccountID is the account the document was written for, so a reader can tell at a
	// glance that they are looking at their own.
	AccountID string `json:"accountId"`

	// GeneratedAt is when the document was built. Everything below describes the access
	// held at that moment and nothing about what it will be later.
	GeneratedAt time.Time `json:"generatedAt"`

	// Datasets is one entry per grant currently giving this account access.
	Datasets []ManifestDataset `json:"datasets"`
}

// ManifestDataset describes one dataset a researcher account can read, and where to read
// it from.
//
// One entry per grant rather than one per dataset: two grants of the same dataset to the
// same account can differ in the VPC they are read from and in when they end, and
// collapsing them into one entry would have to misreport one or the other. They may even
// read from different copies of it.
type ManifestDataset struct {
	// GrantID identifies the access this entry describes. It is the identifier to quote
	// when asking about that access, which is the only thing it is good for -- an
	// operator needs it, a reader's tooling does not.
	GrantID string `json:"grantId"`

	// DatasetURI names the dataset in the source store it was copied from. It is the
	// dataset's identity, not somewhere the reader can reach.
	DatasetURI string `json:"datasetUri"`

	// Bucket holds the readable copy, and Region is where it is served from. A reader
	// needs both to address it.
	Bucket string `json:"bucket"`
	Region string `json:"region"`

	// Prefix is where the dataset's objects sit in that bucket. Empty means the bucket
	// root, which is where they sit today: the bucket holds one dataset.
	Prefix string `json:"prefix"`

	// ExcludedPrefix is a prefix inside the bucket that is not part of the dataset and
	// is not readable by this account. It holds the record of how the copy ran, and it
	// appears in a listing of the bucket, so naming it here saves a reader working out
	// why fetching from it is refused.
	ExcludedPrefix string `json:"excludedPrefix"`

	// KeyArn is the key the objects are encrypted with. Reads are decrypted per
	// request, so a reader whose client cannot use this key sees a refusal rather than
	// an object.
	KeyArn string `json:"keyArn"`

	// TargetVpcID is the VPC reads must originate from. Requests reaching the bucket
	// from anywhere else are refused, and that refusal looks like every other one, so it
	// is worth stating.
	TargetVpcID string `json:"targetVpcId"`

	// ExpiresAt is when access ends. Null means it does not end on a date, which is not
	// the same as permanent: access can still be withdrawn.
	ExpiresAt *time.Time `json:"expiresAt"`
}

// ManifestBuilder builds the document for a researcher account.
type ManifestBuilder struct {
	// Region is where the cache buckets are served from. It is here rather than on a
	// grant because every cache bucket in one deployment is in the same region, and a
	// reader still needs it to address the bucket.
	Region string
}

// NewManifestBuilder returns a builder for cache buckets served from the given region.
func NewManifestBuilder(region string) *ManifestBuilder {
	return &ManifestBuilder{Region: strings.TrimSpace(region)}
}

// Build returns the document for accountID, or nil when the account holds no grant
// giving access as at now.
//
// Nil is not a failure. It is the case publishing turns into a deletion, so an account
// whose last grant has ended has its document removed rather than left describing access
// it no longer has.
//
// Grants held by another account are dropped rather than reported. A document is readable
// by the account it is written for, so one built from a mixed list would disclose another
// account's access; dropping is the only safe reading of a caller that passed more than
// it meant to.
func (b *ManifestBuilder) Build(accountID string, grants []*Grant, now time.Time) *Manifest {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}

	entries := make([]ManifestDataset, 0, len(grants))
	for _, grant := range grants {
		if grant == nil || grant.ResearcherAccountID != accountID {
			continue
		}
		// Active is the whole filter: it holds only for a grant that stands and whose
		// expiry has not passed.
		if !grant.IsActive(now) {
			continue
		}
		entries = append(entries, b.entry(grant))
	}
	if len(entries) == 0 {
		return nil
	}

	// Ordered by dataset, then by grant, so the same access produces the same document
	// however the ledger happened to return it.
	slices.SortFunc(entries, func(a, b ManifestDataset) int {
		if order := cmp.Compare(a.DatasetURI, b.DatasetURI); order != 0 {
			return order
		}
		return cmp.Compare(a.GrantID, b.GrantID)
	})

	return &Manifest{
		Note:        manifestNote,
		AccountID:   accountID,
		GeneratedAt: now.UTC(),
		Datasets:    entries,
	}
}

// entry describes one grant. Everything a reader needs is on the grant itself: it carries
// the dataset, the bucket and the key of the copy it reads from, so nothing here has to
// go and find the copy.
func (b *ManifestBuilder) entry(grant *Grant) ManifestDataset {
	var expiresAt *time.Time
	if grant.ExpiresAt != nil {
		// Copied rather than aliased, so a document already built cannot change under a
		// caller that goes on to use the grant.
		expiry := grant.ExpiresAt.UTC()
		expiresAt = &expiry
	}

	return ManifestDataset{
		GrantID:        grant.GrantID,
		DatasetURI:     grant.DatasetURI,
		Bucket:         grant.HotCacheBucket,
		Region:         b.Region,
		Prefix:         datasetObjectPrefix,
		ExcludedPrefix: ControlPrefix + "/",
		KeyArn:         grant.KeyArn,
		TargetVpcID:    grant.TargetVpcID,
		ExpiresAt:      expiresAt,
	}
}

// ManifestTarget is where a researcher's document is delivered.
//
// Both release mechanisms deliver into the same bucket and differ only in the key prefix,
// so that a researcher holding both kinds of access does not have one document overwrite
// the other.
type ManifestTarget struct {
	// Bucket holds every researcher's document, one key each. It belongs to the account
	// this tool runs in; a researcher's read of it is granted by that bucket's own
	// policy, which is not written here.
	Bucket string

	// RenderKey returns the key for one account. The account alone does not determine
	// the key -- the prefix is configured -- so the caller supplies the rendering rather
	// than this package assuming a layout.
	RenderKey func(accountID string) (string, error)
}

// accountGrantLister is the ledger read publishing needs: every grant recorded for one
// account, whatever state each is in.
//
// It stays a one-method interface rather than the store itself because publishing asks
// the ledger exactly one question, and a document that has to be rebuilt from a different
// source -- a reconciliation, a test -- should not have to be a ledger to do it.
type accountGrantLister interface {
	ListByAccount(ctx context.Context, researcherAccountID string) ([]*Grant, error)
}

// ManifestPublisher rebuilds a researcher account's document from the ledger and
// delivers it.
//
// It is deliberately the only way the document is written, and it is always rebuilt from
// every grant the account holds rather than from whichever grant just changed: one
// account has one document covering all of its access, so a document derived from a
// single change would drop the rest of it.
type ManifestPublisher struct {
	grants  accountGrantLister
	writer  *manifest.Writer
	builder *ManifestBuilder
	target  ManifestTarget
	now     func() time.Time
}

// NewManifestPublisher returns a publisher delivering to the given target.
//
// The target is checked here rather than at the first publish because publishing happens
// after the policies have been applied, where a misconfiguration is no longer something
// the operator can simply retry past.
func NewManifestPublisher(
	grants accountGrantLister,
	writer *manifest.Writer,
	builder *ManifestBuilder,
	target ManifestTarget,
) (*ManifestPublisher, error) {
	if grants == nil {
		return nil, errors.New("a grant ledger is needed to build a researcher's manifest from")
	}
	if writer == nil {
		return nil, errors.New("a manifest writer is needed to deliver a researcher's manifest")
	}
	if builder == nil {
		return nil, errors.New("a manifest builder is needed to describe a researcher's access")
	}
	if strings.TrimSpace(target.Bucket) == "" {
		return nil, ManifestBucketPrerequisite("", "").
			Missingf("no bucket is configured to publish researcher manifests to")
	}
	if target.RenderKey == nil {
		return nil, errors.New("no key rendering is configured for researcher manifests")
	}

	return &ManifestPublisher{
		grants:  grants,
		writer:  writer,
		builder: builder,
		target:  target,
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

// Publish rebuilds the document for one researcher account and writes it, or removes the
// document when the account holds no grant giving access. It returns what was published,
// and nil when the document was removed.
func (p *ManifestPublisher) Publish(ctx context.Context, accountID string) (*Manifest, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("a researcher account is needed to publish a manifest for")
	}

	key, err := p.target.RenderKey(accountID)
	if err != nil {
		return nil, fmt.Errorf("resolving the manifest location for account %s: %w", accountID, err)
	}

	grants, err := p.grants.ListByAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("reading the access held by account %s: %w", accountID, err)
	}

	document := p.builder.Build(accountID, grants, p.now())

	// One call for both outcomes: a document to publish is written, and no document is
	// a deletion, so an account that has just lost its last grant does not keep a stale
	// one.
	if err := manifest.SyncDocument(ctx, p.writer, p.target.Bucket, key, document); err != nil {
		return nil, fmt.Errorf("publishing the manifest for account %s to s3://%s/%s: %w",
			accountID, p.target.Bucket, key, err)
	}

	return document, nil
}
