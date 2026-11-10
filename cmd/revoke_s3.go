package cmd

// This file stops one researcher account reading the copy of a dataset held in this
// account. It destroys nothing: the objects stay where they are, the key encrypting them
// stays usable, and the dataset can be granted again -- to the same account or to another
// one -- without being copied a second time. Destroying the copy is a separate operation
// with a separate confirmation, because it is the one that cannot be undone.
//
// Withdrawing is not a separate operation on the two documents access is enforced by. They
// are rendered from every grant standing against the copy, and a grant that has been
// withdrawn contributes nothing, so writing them again with this grant taken out is the
// whole of it. Every other grant on the copy is rendered exactly as the ledger holds it,
// which is what leaves the other researchers reading: one withdrawal, one researcher.
//
// What the applier does with the order of the two writes is its own business and
// documented there; the part that matters here is that the bucket policy goes first, so if
// only one write lands it is the one that closes the gate, and reads and listings have
// already stopped.
//
// The order of the two halves of the command -- policies, then the record -- is the
// opposite of the one a grant is made in, and for a reason the grant does not have.
//
// Granting records first because a grant's statements are named after the grant, so
// nothing can be rendered until the record exists, and because a failure between the two
// then leaves access recorded and not given. Withdrawing has a record already, so it is
// free to choose, and what it has to choose between is which half of a half-finished
// withdrawal to leave behind. The direction to avoid is a record saying access has stopped
// while it has not: that reports the custodian's intent as achieved when nothing was
// withdrawn, and nothing would look again, because the listing shows no access so nobody
// investigates while the researcher keeps reading. Applying first fails the other way,
// which is the direction that was asked for -- the reads have stopped, and only the
// bookkeeping is behind.
//
// That failure is still reported loudly rather than as a warning, because a grant that
// reads as deployed is what reconciliation would write back out of the ledger and into the
// policies. Re-running the same withdrawal is the fix: the record is still granted, so the
// second attempt renders the same documents and records the withdrawal the first could not.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/dataset"
)

var revokeS3Cmd = &cobra.Command{
	Use:   "s3 <grant-id>",
	Short: "Stop a researcher account reading a copied dataset",
	Long: `Stop one researcher account reading the copy of a dataset held in this account.

The cache bucket's policy and the policy of the key encrypting it are written
again without the statements naming that researcher, which is the whole of the
withdrawal: access is evaluated by AWS per request, so reads stop as the bucket
policy is written and nothing of ours has to keep running.

The operand is a grant, not the copy it reads from. Several researchers may hold
grants on one copy, and withdrawing one leaves the others deployed exactly as
they were.

Nothing is destroyed. The objects remain in the bucket, the key remains able to
decrypt them, and the dataset can be granted again without being copied a second
time. Destroying the copy is a separate operation.

Only a grant that is still deployed can be withdrawn, including one whose expiry
has already passed -- an expired grant still carries the granting statements, so
removing them is a real change. A grant already withdrawn is refused with the
state it is in.`,
	Args: cobra.ExactArgs(1),
	RunE: runRevokeS3,
}

func init() {
	revokeCmd.AddCommand(revokeS3Cmd)
}

func runRevokeS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	grantID := strings.TrimSpace(args[0])
	if grantID == "" {
		return errors.New("the grant to withdraw is required")
	}

	releaser, err := newReleaserFromConfig(ctx)
	if err != nil {
		return err
	}

	return releaser.revoke(ctx, cmd, grantID)
}

// revoke takes one grant's statements back out of both deployed documents and records the
// withdrawal.
func (r *releaser) revoke(ctx context.Context, cmd *cobra.Command, grantID string) error {
	grant, err := r.grants.Get(ctx, grantID)
	if err != nil {
		return err
	}

	// Checked here rather than left to the ledger, so a grant with nothing to withdraw is
	// refused before either document is touched. The record's stored state is what decides
	// it, not the state it reports: a grant whose expiry has passed still has its granting
	// statements deployed, and taking them out is a real withdrawal rather than a
	// formality.
	if !grant.IsDeployed() {
		return &grantNotRevocableError{Grant: grant}
	}

	c, deployed, err := r.copyAndGrants(ctx, grant)
	if err != nil {
		return err
	}

	// The documents are rendered from the ledger, and the ledger has not been written yet
	// -- it is written second on purpose. So the withdrawal is described here as the
	// record will describe it once written, which is the whole of what taking the
	// statements out amounts to.
	target := policyTargetFor(c, withGrantRevoked(deployed, grant.GrantID))

	application, err := r.applier.Apply(ctx, target)
	if err != nil {
		return revokeS3ApplyFailure(err)
	}

	recorded, err := r.grants.MarkRevoked(ctx, grantID)
	if err != nil {
		return &unrecordedRevocationError{
			GrantID:             grant.GrantID,
			DatasetURI:          grant.DatasetURI,
			ResearcherAccountID: grant.ResearcherAccountID,
			HotCacheBucket:      grant.HotCacheBucket,
			Cause:               err,
		}
	}

	reportRevocation(cmd.OutOrStdout(), recorded, application)

	// Republished so the researcher's own description stops advertising access that has
	// ended. An account whose last grant this was has its document removed rather than
	// left behind. Reported inside the field block, before the closing note below.
	manifestErr := r.publishManifest(ctx, cmd, recorded.ResearcherAccountID,
		fmt.Sprintf("grant %s of dataset %q gives account %s nothing, in the ledger and in both deployed policies",
			recorded.GrantID, recorded.DatasetURI, recorded.ResearcherAccountID))

	reportWhatSurvivesRevocation(cmd.OutOrStdout(), recorded, application)

	// A key whose deletion is already scheduled takes no further policy, so the applier
	// writes the bucket policy alone and says why. Reads have still stopped -- the gate
	// is the bucket policy -- but the key carries a decrypt permission nobody can use
	// until the key itself goes, and an operator reading the applied documents would
	// otherwise have to work out why one of them is missing.
	if application.KeyPolicyLeftAlone != "" {
		warnf(cmd, "the key policy was left as it was: %s", application.KeyPolicyLeftAlone)
	}

	return manifestErr
}

// withGrantRevoked returns a copy's grants with one of them described as it will be once
// the withdrawal is recorded.
//
// Every other grant is passed through exactly as the ledger holds it, and that is what
// leaves the other researchers reading: the documents are rendered whole from all of them,
// so anything dropped here would be access silently taken away from someone who was not
// mentioned in the command.
func withGrantRevoked(grants []*dataset.Grant, grantID string) []*dataset.Grant {
	described := make([]*dataset.Grant, 0, len(grants))
	for _, grant := range grants {
		if grant.GrantID != grantID {
			described = append(described, grant)
			continue
		}
		// Copied rather than altered in place, so that what is reported afterwards is the
		// record the ledger returned and not a description invented for the render.
		withdrawn := *grant
		withdrawn.State = dataset.GrantStateRevoked
		described = append(described, &withdrawn)
	}
	return described
}

// revokeS3ApplyFailure reports a failure to write the two documents.
//
// The applier says what is in force in both the case where nothing was written and the
// case where only the bucket policy was, so its account is passed through rather than
// restated. What it cannot know is that the ledger has not been touched yet, which is
// what makes the half-finished withdrawal recoverable by repeating it.
func revokeS3ApplyFailure(err error) error {
	var partial *dataset.PartialPolicyApplicationError
	if errors.As(err, &partial) {
		return fmt.Errorf("%w. The grant still records the access, so running the same "+
			"withdrawal again writes the document that was missed", err)
	}
	return err
}

// grantNotRevocableError reports that a grant has no access left to withdraw.
//
// Nothing was written. Both documents are exactly as they were, so whatever access they
// already describe still stands -- and it does not include this grant, whose statements
// came out when it was first withdrawn.
type grantNotRevocableError struct {
	Grant *dataset.Grant
}

func (e *grantNotRevocableError) Error() string {
	return fmt.Sprintf(
		"grant %s of dataset %q has no access to withdraw: it is %s, so its statements came out of "+
			"the deployed policies on %s. The objects in %s and the key encrypting them are still "+
			"there, so the dataset can be granted again -- as a new grant, since this one's "+
			"withdrawal is part of the record of what happened. No policy was written",
		e.Grant.GrantID, e.Grant.DatasetURI, e.Grant.State, formatMoment(e.Grant.RevokedAt),
		e.Grant.HotCacheBucket)
}

// unrecordedRevocationError reports access that has stopped but was not written down.
//
// It is separated from every other failure because of what it leaves behind, and because
// the two halves disagree in the direction that eventually undoes the withdrawal. The
// reads have stopped, so the custodian got what they asked for. The record still describes
// a deployed grant, so a listing reports access that does not exist, and reconciliation
// reads the record as the truth -- it would write the granting statements back into both
// documents and hand the researcher their access again.
type unrecordedRevocationError struct {
	GrantID             string
	DatasetURI          string
	ResearcherAccountID string
	HotCacheBucket      string
	Cause               error
}

func (e *unrecordedRevocationError) Error() string {
	return fmt.Sprintf(
		"the deployed policies of dataset %q no longer grant account %s anything, so their reads "+
			"of %s have stopped, but grant %s could not be updated to record it: %v. The grant "+
			"still reads as deployed, so it will appear in listings as live access and reconciling "+
			"the copy would put its statements back into the policies. Withdraw it again to record "+
			"the revocation",
		e.DatasetURI, e.ResearcherAccountID, e.HotCacheBucket, e.GrantID, e.Cause)
}

func (e *unrecordedRevocationError) Unwrap() error {
	return e.Cause
}

// reportRevocation describes what was withdrawn, from the record as it now stands rather
// than from the request, so that what is printed is what was stored.
//
// The account and VPC that held the access are printed even though they no longer have it:
// they are what the operator is confirming, and the ledger keeps them for the same reason.
func reportRevocation(w io.Writer, grant *dataset.Grant, application *dataset.PolicyApplication) {
	fmt.Fprintf(w, "Dataset access withdrawn:\n")
	fmt.Fprintf(w, "  Grant ID:    %s\n", grant.GrantID)
	fmt.Fprintf(w, "  Copy ID:     %s\n", grant.CopyID)
	fmt.Fprintf(w, "  Dataset:     %s\n", grant.DatasetURI)
	fmt.Fprintf(w, "  Account:     %s\n", valueOrAbsent(grant.ResearcherAccountID))
	fmt.Fprintf(w, "  Target VPC:  %s\n", valueOrAbsent(grant.TargetVpcID))
	fmt.Fprintf(w, "  Bucket:      %s\n", grant.HotCacheBucket)
	fmt.Fprintf(w, "  Key:         %s\n", grant.KeyArn)
	fmt.Fprintf(w, "  Was until:   %s\n", formatExpiry(grant.ExpiresAt))
	fmt.Fprintf(w, "  Withdrawn:   %s\n", formatMoment(grant.RevokedAt))
	fmt.Fprintf(w, "  Deployed:    %s\n", formatDeployedGrants(application))
	fmt.Fprintf(w, "  Applied:     %s\n", formatAppliedTargets(application))
}

// reportWhatSurvivesRevocation closes the report by saying what a withdrawal did not do.
//
// It is separate from the field block above so that the manifest, which is published
// between the two, is reported as the field it is rather than appearing after the closing
// paragraph as an afterthought.
//
// The grants left standing are named here rather than in the field block because they are
// the thing an operator is most likely to be uneasy about: the documents were rewritten
// whole, and this says what went back into them.
func reportWhatSurvivesRevocation(w io.Writer, grant *dataset.Grant, application *dataset.PolicyApplication) {
	fmt.Fprintf(w, "\nThe objects in %s and the key encrypting them are untouched: nothing was "+
		"deleted, and the copy is still readable by this account. The dataset can be granted "+
		"again, to this researcher or another, without being copied a second time -- as a new "+
		"grant, since this one's withdrawal stays on the record. Destroying the copy is a "+
		"separate operation.\n", grant.HotCacheBucket)

	if remaining := len(application.GrantIDs); remaining > 0 {
		fmt.Fprintf(w, "\n%d other grant(s) on copy %s were written back exactly as they were, so "+
			"the researchers holding them are reading as before.\n", remaining, grant.CopyID)
	}
}

// formatMoment renders a recorded timestamp, naming the absence of one rather than
// printing a zero time that reads as a real date in 1970.
func formatMoment(at *time.Time) string {
	if at == nil {
		return absentValue
	}
	return at.Format(time.RFC3339)
}
