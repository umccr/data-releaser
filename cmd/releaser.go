package cmd

// The three commands that move a grant -- give it, move its expiry, take it back -- run
// against the same things, and this file is where they get them.
//
// The ledger is two stores because a copy and a grant are two records. The copy ledger
// answers what has been transferred into this account; the grant ledger answers who may
// read what a transfer delivered. Each of these commands needs both, and not only to look
// one up from the other: the two documents access is enforced by are shared by every grant
// standing against a copy, so they are rendered from the copy together with all of them.
// Rendering from the one grant being moved would write a document that takes everyone
// else's access away, which is the mistake this seam exists to make hard to commit.
//
// The applier writes those two documents, since a read is checked against the cache
// bucket's policy and the policy of the key encrypting it and against nothing else. The
// publisher writes the researcher's own copy of the story, which is descriptive and
// nothing more: it is not read when a request is authorised, and it exists so a reader can
// find the data without being told out of band where it staged.
//
// Gathering them here is also what makes each command's own decisions testable without
// reaching AWS, the way staging and purging already are: the clients go in at the edge,
// and everything below works against them.
//
// The publisher is built up front, before a command touches anything, because publishing
// happens last. A manifest target that cannot be resolved is a configuration mistake, and
// discovering it after the policies are in force turns a mistake the operator could have
// fixed before starting into one they have to chase.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
	"github.com/umccr/data-releaser/internal/manifest"
)

// releaseClients are the services a grant, an extension, or a withdrawal touches.
type releaseClients struct {
	ledger  internalaws.DynamoDBClient
	objects internalaws.S3Client
	keys    internalaws.KMSClient
}

// releaser holds what the three commands that move a grant have in common.
type releaser struct {
	copies    *dataset.CopyStore
	grants    *dataset.GrantStore
	applier   *dataset.PolicyApplier
	manifests *dataset.ManifestPublisher

	// now is indirected so that a report of where an expiry stands can be pinned.
	now func() time.Time
}

func newReleaser(cfg *config.Config, clients releaseClients) (*releaser, error) {
	grants := dataset.NewGrantStore(clients.ledger, cfg.Ledger.Table)

	// Built from the grant ledger alone: a researcher's document describes the access
	// they hold, and a copy nobody was granted is not access.
	manifests, err := dataset.NewManifestPublisher(
		grants,
		manifest.NewWriter(clients.objects),
		dataset.NewManifestBuilder(cfg.Region),
		datasetManifestTarget(cfg),
	)
	if err != nil {
		return nil, err
	}

	return &releaser{
		copies:    dataset.NewCopyStore(clients.ledger, cfg.Ledger.Table),
		grants:    grants,
		applier:   dataset.NewPolicyApplier(releasePolicySettings(cfg), clients.objects, clients.keys),
		manifests: manifests,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

// policyTargetFor gathers the whole of what a copy's two documents have to say: the bucket
// and key the copy staged in, and every grant recorded against it.
//
// It is deliberately the only way a target is built in this package. The documents are
// rendered whole and replace whatever is deployed, so a target assembled from the one
// grant a command happens to be moving would write out everybody else's statements. Every
// grant is carried, revoked ones included; which of them contribute statements is the
// renderer's to decide, and handing it the whole of the ledger's answer means no caller
// has to filter correctly for it.
func policyTargetFor(c *dataset.Copy, grants []*dataset.Grant) dataset.PolicyTarget {
	return dataset.PolicyTarget{
		DatasetURI:     c.DatasetURI,
		HotCacheBucket: c.HotCacheBucket,
		KeyArn:         c.KeyArn,
		Grants:         grants,
	}
}

// copyAndGrants reads one grant's copy and every grant standing against it, which is
// everything a render needs.
func (r *releaser) copyAndGrants(ctx context.Context, grant *dataset.Grant) (*dataset.Copy, []*dataset.Grant, error) {
	c, err := r.copies.Get(ctx, grant.CopyID)
	if err != nil {
		return nil, nil, err
	}
	grants, err := r.grants.GrantsOfCopy(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	return c, grants, nil
}

// datasetManifestTarget resolves where a researcher's dataset document is delivered. It
// has its own prefix rather than sharing the htsget document's, because one researcher may
// hold both kinds of access and neither description should overwrite the other.
func datasetManifestTarget(cfg *config.Config) dataset.ManifestTarget {
	target := cfg.S3.Manifest
	return dataset.ManifestTarget{
		Bucket:    strings.TrimSpace(target.Bucket),
		RenderKey: target.RenderManifestKey,
	}
}

// publishManifest rebuilds and delivers the researcher's document after their access has
// changed.
//
// It runs last, after the documents access is enforced by have been written and after the
// ledger has recorded what they say, and it is the only step whose failure leaves nothing
// dangerous behind: the grant is exactly what was asked for either way, and what is stale
// is a description of it. So the change stands and the failure is reported, rather than
// anything being unwound.
//
// It is still reported as a failure rather than a warning. The document is what a
// researcher navigates by, and a stale one either advertises access that has ended or
// omits access that has begun -- neither of which anything else in this tool will notice,
// since nothing reads the document back.
func (r *releaser) publishManifest(ctx context.Context, cmd *cobra.Command, accountID, applied string) error {
	if strings.TrimSpace(accountID) == "" {
		// Nothing here names a researcher, so there is nobody to describe it to.
		return nil
	}

	published, err := r.manifests.Publish(ctx, accountID)
	if err != nil {
		return &manifestNotPublishedError{AccountID: accountID, Applied: applied, Cause: err}
	}

	out := cmd.OutOrStdout()
	if published == nil {
		fmt.Fprintf(out, "  Manifest:    removed; account %s holds no dataset access to describe\n", accountID)
		return nil
	}
	fmt.Fprintf(out, "  Manifest:    %d dataset(s) described to account %s\n", len(published.Datasets), accountID)
	return nil
}

// manifestNotPublishedError reports access that changed as asked, and a researcher who
// has not been told.
//
// The access itself needs no attention: the deployed documents and the ledger agree, and
// re-running the command is safe precisely because the document is rebuilt from the ledger
// every time rather than edited.
type manifestNotPublishedError struct {
	AccountID string
	// Applied says what did take effect, so the report does not read as a failure of the
	// whole command.
	Applied string
	Cause   error
}

func (e *manifestNotPublishedError) Error() string {
	return fmt.Sprintf(
		"%s, but the manifest describing account %s's access could not be published: %v. The access "+
			"itself is exactly as intended -- the deployed policies and the ledger agree, and this "+
			"document is descriptive only and never consulted when a read is authorised. Run the "+
			"same command again to republish it; the document is rebuilt from the ledger each time, "+
			"so nothing further is changed by doing so",
		e.Applied, e.AccountID, e.Cause)
}

func (e *manifestNotPublishedError) Unwrap() error {
	return e.Cause
}

// newReleaserFromConfig builds a releaser against live AWS clients. Each of the three
// commands needs the same sections: the account and region written into both documents,
// the ledger, the principals those documents must keep working besides the researcher,
// and where the researcher's own description of the access is delivered.
func newReleaserFromConfig(ctx context.Context) (*releaser, error) {
	cfg, err := requireConfig(
		config.SectionAccount,
		config.SectionLedger,
		config.SectionReleasePolicies,
		config.SectionDatasetManifest,
	)
	if err != nil {
		return nil, err
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, cfg.Region)
	if err != nil {
		return nil, err
	}

	return newReleaser(cfg, releaseClients{
		ledger:  factory.DynamoDBClient(),
		objects: factory.S3Client(),
		keys:    factory.KMSClient(),
	})
}
