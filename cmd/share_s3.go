package cmd

// This file grants a researcher account read access to the copy of a dataset already
// held in this account.
//
// Nothing of ours sits in the read path. The access is entirely the two documents the
// applier writes -- the cache bucket's policy and the policy of the key encrypting it --
// so once they are written it is real whether or not the rest of this command succeeds.
// That fact, and one other, decide the order of everything below.
//
// The other is that the statements one grant contributes are named after the grant
// itself, which is what lets several of them sit in one pair of documents without
// colliding. A grant therefore has to exist as a record before it can be rendered at all:
// until the ledger has minted its identifier there is nothing to name its statements
// after.
//
//   - Every input is checked, and the copy to grant against is chosen, before anything is
//     written. A malformed VPC identifier, a dataset with no staged copy, or a grant that
//     duplicates one already in place costs nothing and changes nothing. The state of the
//     key is read here too, for the reason given below.
//   - The grant is recorded next, and the documents are written from the ledger
//     afterwards.
//   - A failure between the two leaves a grant recorded and not deployed. The researcher
//     cannot read, because the documents a request is checked against say nothing about
//     them, and the disagreement resolves in the direction of the record: reconciling the
//     copy renders both documents from the ledger and writes the grant out. The other
//     order fails the dangerous way instead -- live access that no record mentions, which
//     nothing here would notice and reconciliation would resolve by taking it away.
//
// What is written is the whole copy rather than the one new grant. Every grant standing
// against a copy contributes its own statements to the same two documents, and those
// documents are rendered whole and replace whatever is deployed, so rendering from the
// new grant alone would take every other researcher's access away as a side effect of
// giving one.
//
// Reading the key before the record is written does not contradict that ordering. The
// ordering is about which of the two writes comes first, and the only write it moves ahead
// of is still the record; a read commits to nothing. What it catches is the one way the
// milder failure is not mild: a key disabled or scheduled for deletion outside this tool
// leaves the ledger behind, so the copy still reads as staged while the data it protects is
// already unreadable. Recording first there would leave a grant that reconciliation cannot
// make real either, since the key that would have to decrypt is the thing that is gone.
//
// The command takes the researcher's VPC identifier and nothing else about their
// account. Their endpoint for S3, and which of their own principals may read, are
// theirs to arrange: the grant names their account and bounds it to one VPC, and their
// IAM decides the rest.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
)

// minimumExpiryDuration is the shortest release either mechanism accepts. It matches the
// htsget leaf, so that the same duration means the same thing whichever mechanism is
// named, and it rejects the transposed unit that would otherwise grant minutes of access
// where days were meant.
const minimumExpiryDuration = 24 * time.Hour

var shareS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Grant a researcher account direct read access to a copied dataset",
	Long: `Grant a researcher account read access to the copy of a dataset held in this
account, from one nominated VPC.

Access is enforced by the cache bucket's policy and by the policy of the key
encrypting that bucket, evaluated by AWS on every request. The grant therefore
takes effect as the policies are written and needs nothing of ours running to
sustain it.

The dataset must already have been copied into this account. One whose copy is
still running, failed, or whose key is being destroyed is refused with the state
that copy is in.

Several researchers may read one copy at the same time, so a dataset already
granted to someone else is granted again without anything being copied. What is
refused is the same account reading the same copy from the same VPC twice: that
is a duplicate of a grant already in place rather than a second researcher, and
its expiry is moved rather than a second grant being made.

The researcher's VPC, their endpoint for S3, and the permissions their own
principals hold are theirs to provide. Only the VPC identifier is taken here, and
it is compared literally against the VPC a read arrives from.

An expiry is optional. Without one the grant is unlimited, and the policies carry
no time condition at all rather than a distant date.`,
	RunE: runShareS3,
}

func init() {
	shareS3Cmd.Flags().String("dataset-uri", "", "URI of the dataset to grant access to")
	shareS3Cmd.Flags().String("account", "", "Researcher AWS account ID to grant read access to")
	shareS3Cmd.Flags().String("target-vpc-id", "",
		"Bare identifier of the researcher VPC reads must arrive from (e.g. vpc-0123456789abcdef0). "+
			"This is the bare identifier the bucket policy compares against, not the full VPC ARN "+
			"the htsget leaf takes.")
	shareS3Cmd.Flags().String("expiry", "", "Optional expiry duration (e.g. 30d, 24h); omitted, the grant is unlimited")

	_ = shareS3Cmd.MarkFlagRequired("dataset-uri")
	_ = shareS3Cmd.MarkFlagRequired("account")
	_ = shareS3Cmd.MarkFlagRequired("target-vpc-id")

	shareCmd.AddCommand(shareS3Cmd)
}

func runShareS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	request, err := readShareS3Request(cmd)
	if err != nil {
		return err
	}

	releaser, err := newReleaserFromConfig(ctx)
	if err != nil {
		return err
	}

	return releaser.share(ctx, cmd, request)
}

// share grants one researcher account read access to a dataset already copied here.
func (r *releaser) share(ctx context.Context, cmd *cobra.Command, request shareS3Request) error {
	copies, err := r.copies.CopiesOfDataset(ctx, request.datasetURI)
	if err != nil {
		return err
	}
	c, err := copyToGrant(request.datasetURI, copies)
	if err != nil {
		return err
	}

	standing, err := r.grants.GrantsOfCopy(ctx, c)
	if err != nil {
		return err
	}
	if duplicate := duplicateGrant(standing, request); duplicate != nil {
		return &duplicateGrantError{DatasetURI: request.datasetURI, Existing: duplicate}
	}

	// Asked of the key before anything is written, since a key that cannot decrypt makes
	// the grant undeployable rather than merely undeployed. It changes nothing either way.
	if err := r.applier.CheckGrantable(ctx, policyTargetFor(c, standing)); err != nil {
		return fmt.Errorf(
			"dataset %q was not granted to account %s, and nothing was recorded: %w",
			request.datasetURI, request.account, shareS3ApplyFailure(err))
	}

	// Recorded before anything is deployed. The identifier this mints is what the grant's
	// statements are named after, so there is nothing to render until it exists.
	grant, err := r.grants.Create(ctx, c, dataset.GrantRequest{
		ResearcherAccountID: request.account,
		TargetVpcID:         request.targetVpcID,
		ExpiresAt:           request.expiresAt,
	})
	if err != nil {
		return fmt.Errorf(
			"dataset %q was not granted to account %s, and no policy was written: %w",
			request.datasetURI, request.account, err)
	}

	undeployed := func(cause error) error {
		return &undeployedGrantError{
			GrantID:             grant.GrantID,
			CopyID:              c.CopyID,
			DatasetURI:          grant.DatasetURI,
			ResearcherAccountID: grant.ResearcherAccountID,
			TargetVpcID:         grant.TargetVpcID,
			Cause:               cause,
		}
	}

	// Read again rather than added to what was read above, so the documents are rendered
	// from the whole of what the ledger now holds against this copy: the new grant, and
	// anything else that arrived alongside it.
	deployed, err := r.grants.GrantsOfCopy(ctx, c)
	if err != nil {
		return undeployed(err)
	}

	application, err := r.applier.Apply(ctx, policyTargetFor(c, deployed))
	if err != nil {
		return undeployed(shareS3ApplyFailure(err))
	}

	reportGrant(cmd.OutOrStdout(), grant, application)

	// Published from the ledger after it has been written, so the document describes the
	// access that was recorded rather than the one that was asked for.
	manifestErr := r.publishManifest(ctx, cmd, grant.ResearcherAccountID,
		fmt.Sprintf("dataset %q is granted to account %s and grant %s records it",
			grant.DatasetURI, grant.ResearcherAccountID, grant.GrantID))

	// Whether the researcher can actually read is not observable from here, and the
	// three ways it can fail are indistinguishable to them: a VPC identifier that does
	// not match theirs, an expiry that has passed, and a key that cannot decrypt all
	// present as the same denial. Saying so is the difference between an operator
	// checking and an operator assuming.
	fmt.Fprintln(cmd.ErrOrStderr(),
		"Note: a grant can only be confirmed by reading an object from inside the target VPC; "+
			"a wrong VPC identifier, a passed expiry, and a key that cannot decrypt all present "+
			"as the same access denial.")

	return manifestErr
}

// shareS3Request is the invocation as it was read and checked, before the ledger is
// consulted.
type shareS3Request struct {
	datasetURI  string
	account     string
	targetVpcID string
	// expiresAt is when access ceases, or nil for an unlimited grant.
	expiresAt *time.Time
}

// readShareS3Request reads and checks the flags.
//
// Everything checkable is checked here, because none of these values fails visibly later.
// An account that is not an account id names no principal, a VPC in the wrong form
// matches no request, and both produce a policy that is written successfully and admits
// nobody.
func readShareS3Request(cmd *cobra.Command) (shareS3Request, error) {
	// Padding is trimmed rather than carried through, since every one of these values
	// goes into a policy document verbatim and a stray space there matches nothing.
	read := func(name string) (string, error) {
		value, err := cmd.Flags().GetString(name)
		if err != nil {
			return "", fmt.Errorf("reading --%s flag: %w", name, err)
		}
		return strings.TrimSpace(value), nil
	}

	datasetURI, err := read("dataset-uri")
	if err != nil {
		return shareS3Request{}, err
	}
	account, err := read("account")
	if err != nil {
		return shareS3Request{}, err
	}
	targetVpcID, err := read("target-vpc-id")
	if err != nil {
		return shareS3Request{}, err
	}
	expiry, err := read("expiry")
	if err != nil {
		return shareS3Request{}, err
	}

	if datasetURI == "" {
		return shareS3Request{}, errors.New("the dataset to grant access to is required (--dataset-uri must not be empty)")
	}
	// Reduced to the form the ledger holds, so a URI written with or without its scheme or
	// its trailing slash finds the dataset that was staged rather than reporting one that
	// was never copied here.
	datasetURI, err = dataset.CanonicalDatasetURI(datasetURI)
	if err != nil {
		return shareS3Request{}, err
	}
	if err := checkResearcherAccount(account); err != nil {
		return shareS3Request{}, err
	}
	if err := checkTargetVpcID(targetVpcID); err != nil {
		return shareS3Request{}, err
	}

	expiresAt, err := parseExpiry(expiry)
	if err != nil {
		return shareS3Request{}, err
	}

	return shareS3Request{
		datasetURI:  datasetURI,
		account:     account,
		targetVpcID: targetVpcID,
		expiresAt:   expiresAt,
	}, nil
}

// checkResearcherAccount rejects anything that is not an AWS account id. The value is
// written into both documents as the principal being granted, where an alias, a role
// ARN, or a truncated number names nobody at all.
func checkResearcherAccount(account string) error {
	if account == "" {
		return errors.New("the researcher account is required (--account must not be empty)")
	}
	if len(account) != 12 || strings.TrimLeft(account, "0123456789") != "" {
		return fmt.Errorf(
			"--account %q is not a 12-digit AWS account id; it is written into the bucket and key "+
				"policies as the principal being granted, so an alias or a role ARN names nobody",
			account)
	}
	return nil
}

// checkTargetVpcID rejects a VPC written in any form but the bare identifier.
//
// The two mechanisms in this tool take a researcher's VPC in two different forms -- the
// full EC2 ARN for the htsget lookup, the bare identifier for the policy condition -- so
// passing the wrong one is a plausible mistake. The consequence is invisible: the
// condition compares this value literally against the VPC a read arrived from, so an ARN
// here is written happily and denies every read.
func checkTargetVpcID(vpcID string) error {
	if vpcID == "" {
		return errors.New("the researcher VPC is required (--target-vpc-id must not be empty)")
	}
	if strings.HasPrefix(vpcID, "arn:") || strings.ContainsAny(vpcID, ":/") {
		return fmt.Errorf(
			"--target-vpc-id %q is an ARN; the bucket policy compares this value literally against "+
				"the VPC a read arrived from, so an ARN here denies every read. Pass the bare "+
				"identifier (the vpc-… part). The full ARN is what the htsget leaf takes",
			vpcID)
	}
	if !strings.HasPrefix(vpcID, "vpc-") || vpcID == "vpc-" {
		return fmt.Errorf(
			"--target-vpc-id %q is not a VPC identifier; it has to be the researcher's own "+
				"vpc-… value, which the bucket policy compares against the VPC a read arrived from",
			vpcID)
	}
	return nil
}

// parseExpiry turns the expiry flag into the moment access ceases, or nil where none was
// asked for.
//
// An absent expiry is unlimited rather than defaulted to something long: the documents
// then carry no time condition at all, which is both what was agreed and what
// distinguishes an unlimited grant from a long one when the deployed policy is read back.
func parseExpiry(expiry string) (*time.Time, error) {
	if expiry == "" {
		return nil, nil
	}

	duration, err := parseDuration(expiry)
	if err != nil {
		return nil, fmt.Errorf("invalid expiry: %w", err)
	}
	if duration < minimumExpiryDuration {
		return nil, fmt.Errorf("expiry duration must be at least 1 day (24h), got %s", duration)
	}

	// To the second, because that is the precision the time condition is written and
	// compared at. Keeping the recorded expiry at the same precision means a re-render
	// matches what is deployed rather than differing by a fraction nobody can see.
	expiresAt := time.Now().UTC().Add(duration).Truncate(time.Second)
	return &expiresAt, nil
}

// releasePolicySettings gathers what the two documents need beyond the ledger itself: the
// account and region they are rendered for, and the principals other than the researcher
// that must keep working once a rendered document replaces the deployed one.
func releasePolicySettings(cfg *config.Config) dataset.PolicySettings {
	return dataset.PolicySettings{
		AccountID:               cfg.Account,
		Region:                  cfg.Region,
		CopyWriterRoleName:      cfg.S3.Copy.WriterRoleName,
		CopyWriterRoleAccountID: cfg.S3.Copy.WriterRoleAccount,
		CustodianRoleARN:        cfg.S3.CustodianRoleArn,
	}
}

// copyToGrant picks the copy a grant is made against: the newest one that staged.
//
// Landing is the whole of the condition. A copy that delivered the dataset can be granted
// to as many researchers as have been agreed, at the same time or one after another, and
// whether anyone already holds it makes no difference to whether it can be granted again:
// a grant is a statement in a policy, and adding one takes nothing away from the others.
//
// Copies arrive newest first, so where a dataset has been transferred more than once the
// most recent delivery is the one granted -- which is the one actually in the bucket,
// since every copy of a dataset lands in the same place and the later transfer wrote over
// the earlier. Where none has staged, the refusal names the copy that most determines what
// can be done next.
func copyToGrant(datasetURI string, copies []*dataset.Copy) (*dataset.Copy, error) {
	var reported *dataset.Copy
	for _, c := range copies {
		if c.IsGrantable() {
			return c, nil
		}
		if reported == nil || copyRefusalRank[c.State] > copyRefusalRank[reported.State] {
			reported = c
		}
	}
	return nil, &datasetNotGrantableError{DatasetURI: datasetURI, Copy: reported}
}

// copyRefusalRank orders the states a refusal may be reported from, so that a dataset with
// several copies is described by the one that matters most.
//
// A copy whose objects have been deleted outranks everything, because the bucket is
// empty and nothing about the dataset is coming back without a fresh transfer. A
// scheduled key deletion comes next, for the same reason one step earlier: it is about to
// take every copy of the dataset with it. A transfer still running follows, since it will
// deliver the dataset and the operator has only to wait for it. One that failed ranks
// last: it delivered nothing and calls for the same staging a purged dataset does.
//
// The staged state is absent: it is granted rather than refused, so it never reaches a
// report of why a grant could not be made.
var copyRefusalRank = map[dataset.CopyState]int{
	dataset.CopyStateDeleted: 4,
	dataset.CopyStatePurged:  3,
	dataset.CopyStateCopying: 2,
	dataset.CopyStateFailed:  1,
}

// duplicateGrant finds a grant already giving this account access to this copy from this
// VPC.
//
// Several researchers reading one copy at once is ordinary and is not refused, and neither
// is one researcher reading it from two VPCs: each of those is a grant of its own, with
// statements of its own, withdrawn on its own. What is refused is the same account and the
// same VPC twice, which would deploy a second set of statements saying exactly what the
// first already says and leave two records where one describes the access.
//
// A withdrawn grant does not count. Its statements are out of the documents and its
// withdrawal is part of the record of what happened, so granting the same access again is
// a new grant rather than a duplicate of one that has ended.
func duplicateGrant(grants []*dataset.Grant, request shareS3Request) *dataset.Grant {
	for _, grant := range grants {
		if grant.IsDeployed() &&
			grant.ResearcherAccountID == request.account &&
			grant.TargetVpcID == request.targetVpcID {
			return grant
		}
	}
	return nil
}

// duplicateGrantError reports a grant that would say exactly what one already in place
// says. Nothing was written, and the grant already standing is untouched.
type duplicateGrantError struct {
	DatasetURI string
	// Existing is the grant already giving this account access from this VPC.
	Existing *dataset.Grant
}

func (e *duplicateGrantError) Error() string {
	return fmt.Sprintf(
		"account %s already reads dataset %q from %s under grant %s, which runs until %s. A second "+
			"grant would deploy statements saying the same thing and leave two records where one "+
			"describes the access, so nothing was written. Move that grant's expiry to change how "+
			"long it runs, or withdraw it if the access should end. Granting the same account "+
			"access from a different VPC, or another account access to the same copy, is a "+
			"separate grant and is not refused",
		e.Existing.ResearcherAccountID, e.DatasetURI, e.Existing.TargetVpcID,
		e.Existing.GrantID, formatExpiry(e.Existing.ExpiresAt))
}

// datasetNotGrantableError reports that a dataset has no staged copy to give away, and
// says what it has instead.
//
// Nothing was written. Both documents are exactly as they were, so whatever access they
// already describe still stands.
type datasetNotGrantableError struct {
	DatasetURI string
	// Copy is the copy the refusal is about, and is nil when the dataset has never been
	// copied into this account.
	Copy *dataset.Copy
}

func (e *datasetNotGrantableError) Error() string {
	if e.Copy == nil {
		return fmt.Sprintf(
			"dataset %q has never been copied into this account, so there is nothing to grant; "+
				"stage it first. No policy was written", e.DatasetURI)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "dataset %q has no staged copy to grant access to: copy %s is %s",
		e.DatasetURI, e.Copy.CopyID, e.Copy.State)

	switch e.Copy.State {
	case dataset.CopyStatePurged:
		fmt.Fprintf(&b, ", so deletion of the key encrypting %s is already scheduled and the copy "+
			"it holds is being destroyed; stage the dataset again to make a fresh copy grantable",
			e.Copy.HotCacheBucket)
	case dataset.CopyStateCopying:
		fmt.Fprintf(&b, ", so its transfer into %s has not reported yet; it becomes grantable once "+
			"the copy lands", e.Copy.HotCacheBucket)
	case dataset.CopyStateFailed:
		fmt.Fprintf(&b, ", so its transfer did not finish and %s may hold only part of the dataset; "+
			"stage the dataset again", e.Copy.HotCacheBucket)
	case dataset.CopyStateDeleted:
		fmt.Fprintf(&b, ", so its key was destroyed and its objects have since been deleted out of "+
			"%s; stage the dataset again to copy it back into that bucket", e.Copy.HotCacheBucket)
	}

	b.WriteString(". No policy was written")
	return b.String()
}

// shareS3ApplyFailure reports a refusal from the applier, whether it came from the check
// made before the grant was recorded or from the write that follows it.
//
// The applier's own failures already say what is in force and what is not, in both the
// case where nothing was written and the case where one document was, so they are passed
// through rather than restated. The one thing worth adding is the standing of a dataset
// whose key is being destroyed: it is refused for the state of the key, and it stays
// refused, because the copy the key protects is on its way out.
func shareS3ApplyFailure(err error) error {
	var notGrantable *dataset.KeyNotGrantableError
	if errors.As(err, &notGrantable) && notGrantable.PendingDeletion() {
		return fmt.Errorf("%w. While that deletion stands the dataset counts as purged and cannot be granted to anyone", err)
	}
	return err
}

// undeployedGrantError reports a grant that was written down but never deployed.
//
// It is the failure this command's ordering is chosen to leave behind, and it is the
// milder of the two directions. The record says the researcher may read; the documents a
// request is actually checked against say nothing about them, so they cannot, and nobody
// holds access that no record mentions. The disagreement resolves towards the record:
// reconciling the copy renders both documents from the ledger, which writes this grant's
// statements out and makes the access real.
//
// Repeating the command does not finish the job -- it is refused as a duplicate of this
// very grant -- so the report says to reconcile or to withdraw instead.
type undeployedGrantError struct {
	GrantID             string
	CopyID              string
	DatasetURI          string
	ResearcherAccountID string
	TargetVpcID         string
	Cause               error
}

func (e *undeployedGrantError) Error() string {
	return fmt.Sprintf(
		"grant %s records account %s as reading dataset %q from %s, but the deployed policies were "+
			"not written, so the researcher can read nothing: %v. No access was given that nothing "+
			"describes. Reconcile copy %s to render both documents from the ledger and make the "+
			"grant real, or withdraw the grant to take it back out of the record; granting the same "+
			"account again would be refused as a duplicate of this one",
		e.GrantID, e.ResearcherAccountID, e.DatasetURI, e.TargetVpcID, e.Cause, e.CopyID)
}

func (e *undeployedGrantError) Unwrap() error {
	return e.Cause
}

// reportGrant describes what was granted and where, from the record as it now stands
// rather than from the request, so that what is printed is what was stored.
func reportGrant(w io.Writer, grant *dataset.Grant, application *dataset.PolicyApplication) {
	fmt.Fprintf(w, "Dataset released successfully:\n")
	fmt.Fprintf(w, "  Grant ID:    %s\n", grant.GrantID)
	fmt.Fprintf(w, "  Copy ID:     %s\n", grant.CopyID)
	fmt.Fprintf(w, "  Dataset:     %s\n", grant.DatasetURI)
	fmt.Fprintf(w, "  Account:     %s\n", grant.ResearcherAccountID)
	fmt.Fprintf(w, "  Target VPC:  %s\n", grant.TargetVpcID)
	fmt.Fprintf(w, "  Bucket:      %s\n", grant.HotCacheBucket)
	fmt.Fprintf(w, "  Key:         %s\n", grant.KeyArn)
	fmt.Fprintf(w, "  Expiry:      %s\n", formatExpiry(grant.ExpiresAt))
	fmt.Fprintf(w, "  Deployed:    %s\n", formatDeployedGrants(application))
	fmt.Fprintf(w, "  Applied:     %s\n", formatAppliedTargets(application))
}

// formatDeployedGrants says how many grants the documents that were written carry.
//
// Several researchers may hold one copy at once and the documents are rendered from all of
// them together, so the count is what tells an operator that granting this one left the
// others where they were.
func formatDeployedGrants(application *dataset.PolicyApplication) string {
	if application == nil {
		return absentValue
	}
	return fmt.Sprintf("%d grant(s) on this copy", len(application.GrantIDs))
}

// formatAppliedTargets names the documents that were written, in the order they were
// written in.
func formatAppliedTargets(application *dataset.PolicyApplication) string {
	if application == nil {
		return absentValue
	}
	names := make([]string, 0, len(application.Written))
	for _, target := range application.Written {
		names = append(names, target.String())
	}
	return valueOrAbsent(strings.Join(names, ", "))
}
