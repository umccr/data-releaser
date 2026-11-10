package cmd

// This file moves the expiry of one grant, so a researcher's access continues without the
// data being copied again.
//
// An expiry is a condition inside the two documents access is enforced by, not a job that
// runs somewhere. Moving it is therefore two writes: the recorded expiry in the ledger,
// and the time condition in the cache bucket's policy and in the policy of the key
// encrypting it. Both express one value, and the ledger is where that value is minted --
// the store computes the new expiry from its own clock as it writes the record.
//
// That is what fixes the order:
//
//   - The record is written first, and the documents are then rendered from the ledger as
//     it now stands. Rendering them first would mean rendering a moment the ledger had not
//     yet agreed to, and the two policy writes take long enough that the ledger's own clock
//     lands in a later second. The condition is compared to whole seconds, so every
//     extension would leave a mismatch that reconciliation reports as drift, over a
//     difference nobody asked for and nobody can see.
//   - Granting records first as well, but for a reason of its own: a grant's statements
//     are named after the grant, so there is nothing to render until the ledger has minted
//     it. The two commands arrive at the same order from different directions, and neither
//     inherited it from the other.
//
// Moving the expiry outward is what this command is for, and in that direction writing the
// record first fails closed: between the two writes the record claims access the documents
// have not granted yet, so the researcher never holds more than the ledger declares, and
// reconciliation converges on the record. A duration shorter than the time remaining moves
// the expiry inward, and there the same failure leaves real access outliving the record.
// The failure report says which of the two happened, because one can be retried at leisure
// and the other cannot.
//
// What is written is the whole copy rather than the one grant. The two documents carry the
// statements of every grant standing against the copy and are rendered whole, so moving one
// researcher's expiry writes the others' statements back exactly as they were -- where
// rendering from the moved grant alone would take their access away as a side effect.

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

var extendS3Cmd = &cobra.Command{
	Use:   "s3 <grant-id>",
	Short: "Move the expiry of a granted dataset",
	Long: `Move the expiry of one grant, so a researcher's access continues without the
dataset being copied again.

The expiry is a condition inside the cache bucket's policy and the policy of the
key encrypting it, evaluated by AWS on every request, so a moved expiry takes
effect as those documents are written and needs nothing of ours running.

The duration runs from now rather than from the expiry it replaces. That is what
makes a grant whose expiry has already passed extendable: its granting statements
are still deployed and the objects are still in place, so moving the expiry
forward restores access without another copy.

The operand is a grant, not the copy it reads from. Several researchers may hold
grants on one copy, and moving one researcher's expiry leaves the others exactly
as they were.

A withdrawn grant is refused with the state it is in, and so is one whose copy is
being destroyed. A grant made without an expiry has no time condition to move,
and is reported as such and left exactly as it was.`,
	Args: cobra.ExactArgs(1),
	RunE: runExtendS3,
}

func init() {
	extendS3Cmd.Flags().String("duration", "", "New duration from now (e.g. 30d, 24h)")

	_ = extendS3Cmd.MarkFlagRequired("duration")

	extendCmd.AddCommand(extendS3Cmd)
}

func runExtendS3(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	request, err := readExtendS3Request(cmd, args)
	if err != nil {
		return err
	}

	releaser, err := newReleaserFromConfig(ctx)
	if err != nil {
		return err
	}

	return releaser.extend(ctx, cmd, request)
}

// extend moves the expiry of one grant, in the ledger and in both deployed documents.
func (r *releaser) extend(ctx context.Context, cmd *cobra.Command, request extendS3Request) error {
	// Read for the report, not for a decision. Whether the expiry may be moved at all is
	// the store's to settle, and it settles it against the record as it stands when it
	// writes rather than as it stood here. What this is for is the expiry being replaced:
	// that is the difference between an extension that widened access and one that
	// narrowed it, and neither the store nor the applier is in a position to say which.
	before, err := r.grants.Get(ctx, request.grantID)
	if err != nil {
		return extendS3LedgerFailure(err)
	}

	// The copy is read before anything is written, because the documents are rendered
	// from it and because a copy whose key is being destroyed can carry no expiry worth
	// moving: no date reaches data on its way out, and recording one would report success
	// for access nothing will honour.
	c, err := r.copies.Get(ctx, before.CopyID)
	if err != nil {
		return err
	}
	if c.State == dataset.CopyStatePurged {
		return &copyBeingDestroyedError{Grant: before, Copy: c}
	}

	updated, err := r.grants.Extend(ctx, request.grantID, request.duration)
	if errors.Is(err, dataset.ErrUnlimitedGrant) {
		return reportNoExpiryToMove(cmd.OutOrStdout(), before)
	}
	if err != nil {
		return extendS3LedgerFailure(err)
	}

	unapplied := func(cause error) error {
		return &unappliedExtensionError{
			GrantID:             updated.GrantID,
			DatasetURI:          updated.DatasetURI,
			ResearcherAccountID: updated.ResearcherAccountID,
			Previous:            before.ExpiresAt,
			Recorded:            updated.ExpiresAt,
			Cause:               cause,
		}
	}

	// Rendered from the ledger as it now stands, so the deployed condition and the
	// recorded expiry are the same moment rather than two readings of two clocks, and so
	// that every other grant on this copy is written back as the ledger holds it.
	deployed, err := r.grants.GrantsOfCopy(ctx, c)
	if err != nil {
		return unapplied(err)
	}

	application, err := r.applier.Apply(ctx, policyTargetFor(c, deployed))
	if err != nil {
		return unapplied(err)
	}

	reportExtension(cmd.OutOrStdout(), before, updated, application)

	// The document carries the expiry, so a researcher whose access was about to lapse
	// reads a stale date until this is republished.
	manifestErr := r.publishManifest(ctx, cmd, updated.ResearcherAccountID,
		fmt.Sprintf("grant %s of dataset %q now runs until %s, in the ledger and in both deployed policies",
			updated.GrantID, updated.DatasetURI, formatExpiry(updated.ExpiresAt)))

	// A grant whose expiry had already passed keeps its granting statements, so moving
	// the expiry forward is the whole of restoring access -- and access restored is no
	// more observable from here than access granted was. Saying so is the difference
	// between an operator checking and an operator assuming.
	if before.IsExpired(r.now()) {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Note: access had lapsed and is now restored, which can only be confirmed by reading "+
				"an object from inside the target VPC; a wrong VPC identifier, a passed expiry, and "+
				"a key that cannot decrypt all present as the same access denial.")
	}

	return manifestErr
}

// extendS3Request is the invocation as it was read and checked.
type extendS3Request struct {
	grantID string
	// duration is how long access runs for from now, replacing whatever is recorded.
	duration time.Duration
}

// readExtendS3Request reads and checks the operand and the flag.
func readExtendS3Request(cmd *cobra.Command, args []string) (extendS3Request, error) {
	grantID := strings.TrimSpace(args[0])
	if grantID == "" {
		return extendS3Request{}, errors.New("the grant whose expiry is being moved is required")
	}

	value, err := cmd.Flags().GetString("duration")
	if err != nil {
		return extendS3Request{}, fmt.Errorf("reading --duration flag: %w", err)
	}

	duration, err := parseDuration(strings.TrimSpace(value))
	if err != nil {
		return extendS3Request{}, fmt.Errorf("invalid duration: %w", err)
	}
	// The same floor the grant is made against, so that a duration means the same thing
	// whether it is being set or moved, and so that the transposed unit which would
	// otherwise shorten access to minutes is refused.
	if duration < minimumExpiryDuration {
		return extendS3Request{}, fmt.Errorf("duration must be at least 1 day (24h), got %s", duration)
	}

	return extendS3Request{grantID: grantID, duration: duration}, nil
}

// extendS3LedgerFailure reports a refusal from the ledger.
//
// Every case here leaves the record exactly as it was, and nothing has been written to
// either document, so whatever access they already describe still stands. The store's own
// messages name the grant and the state it is in; what is added is what that state means
// for an expiry, and what to reach for instead.
func extendS3LedgerFailure(err error) error {
	if errors.Is(err, dataset.ErrGrantNotFound) {
		return fmt.Errorf("%w. Nothing was changed; run 'data-releaser list s3' to see the "+
			"grants and the ids they are extended by", err)
	}

	var notExtendable *dataset.GrantNotExtendableError
	if errors.As(err, &notExtendable) {
		if notExtendable.Current == dataset.GrantStateRevoked {
			return fmt.Errorf("%w: the granting statements are already out of the deployed "+
				"policies, so there is no time condition left to move and moving one would not "+
				"give access back. The objects and the key are untouched, and nothing was "+
				"written. Granting the dataset again is what restores access, as a new grant "+
				"with a record of its own", err)
		}
		return fmt.Errorf("%w. Nothing was written", err)
	}

	return err
}

// copyBeingDestroyedError reports an expiry that could not be moved because the data it
// would bound is on its way out.
//
// Nothing was written. It is refused here rather than left to the ledger because the grant
// itself is perfectly extendable -- it is granted, and it carries an expiry -- and the
// store has no view of the copy the grant reads from. Deletion of the key is what makes
// the extension pointless, and it does not become less so with time.
type copyBeingDestroyedError struct {
	Grant *dataset.Grant
	Copy  *dataset.Copy
}

func (e *copyBeingDestroyedError) Error() string {
	return fmt.Sprintf(
		"grant %s of dataset %q reads copy %s, which is %s: deletion of the key encrypting %s is "+
			"scheduled, so the data it protects is on its way out and no expiry could reach it. "+
			"Stage the dataset again to make a fresh copy grantable. Nothing was written",
		e.Grant.GrantID, e.Grant.DatasetURI, e.Copy.CopyID, e.Copy.State, e.Copy.HotCacheBucket)
}

// reportNoExpiryToMove answers an invocation against a grant that carries no expiry.
//
// A grant made without one carries no time condition in its deployed policies at all, so
// there is nothing to move: access does not lapse, and ending it is a withdrawal rather
// than an expiry. That is reported and the invocation succeeds, because the state the
// operator was reaching for -- access that continues -- already holds.
//
// Nothing is written for it. A grant carrying no time condition and one carrying a distant
// date are different grants, and quietly turning the first into the second would narrow an
// agreement nobody asked to narrow.
func reportNoExpiryToMove(w io.Writer, grant *dataset.Grant) error {
	fmt.Fprintf(w, "No expiry applies; nothing was changed:\n")
	fmt.Fprintf(w, "  Grant ID:    %s\n", grant.GrantID)
	fmt.Fprintf(w, "  Copy ID:     %s\n", grant.CopyID)
	fmt.Fprintf(w, "  Dataset:     %s\n", grant.DatasetURI)
	fmt.Fprintf(w, "  Account:     %s\n", grant.ResearcherAccountID)
	fmt.Fprintf(w, "  Target VPC:  %s\n", grant.TargetVpcID)
	fmt.Fprintf(w, "  Expiry:      %s\n", formatExpiry(grant.ExpiresAt))
	fmt.Fprintf(w, "\nThe grant was made without an expiry, so its deployed policies carry no time "+
		"condition and access does not lapse. There is nothing for a duration to move; stopping "+
		"this access is a withdrawal rather than an expiry.\n")
	return nil
}

// unappliedExtensionError reports an expiry that was recorded but not deployed.
//
// It is separated from every other failure because the two halves have come apart: the
// record says access runs until one moment and the documents a request is checked against
// say another. Which way round that leaves things depends entirely on the direction the
// expiry moved, so the report says so -- an outward move that failed here has exposed
// nothing and can be retried, while an inward one leaves access standing that the ledger
// no longer describes.
type unappliedExtensionError struct {
	GrantID             string
	DatasetURI          string
	ResearcherAccountID string
	// Previous is the expiry the deployed documents still carry, and Recorded the one the
	// ledger now holds.
	Previous *time.Time
	Recorded *time.Time
	Cause    error
}

func (e *unappliedExtensionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "grant %s of dataset %q now records an expiry of %s, but the deployed "+
		"policies were not brought to match: %v",
		e.GrantID, e.DatasetURI, formatExpiry(e.Recorded), e.Cause)

	if expiryWidened(e.Previous, e.Recorded) {
		fmt.Fprintf(&b, ". They still cut access off at %s, so account %s loses access then rather "+
			"than at the recorded expiry, and nothing further has been exposed. Run the same "+
			"extension again, or reconcile the copy, to write the condition out",
			formatExpiry(e.Previous), e.ResearcherAccountID)
		return b.String()
	}

	fmt.Fprintf(&b, ". They still admit reads until %s, which is no earlier than the expiry now "+
		"recorded, so account %s holds access the ledger no longer describes. Run the same "+
		"extension again, or reconcile the copy, before %s",
		formatExpiry(e.Previous), e.ResearcherAccountID, formatExpiry(e.Previous))
	return b.String()
}

func (e *unappliedExtensionError) Unwrap() error {
	return e.Cause
}

// expiryWidened reports whether the move gave away more time than it took back. An absent
// expiry is the widest there is, since a document carrying no time condition admits
// requests indefinitely.
func expiryWidened(previous, recorded *time.Time) bool {
	if recorded == nil {
		return true
	}
	if previous == nil {
		return false
	}
	return recorded.After(*previous)
}

// reportExtension describes the move from the record as it now stands, alongside the
// expiry it replaced, so that what is printed is what was stored and an operator can see
// which way the expiry went.
//
// The count of grants deployed on the copy is printed for the same reason it is printed
// when one is made: the documents were rewritten whole, and it is what shows that the
// other researchers' statements went back in.
func reportExtension(w io.Writer, before, after *dataset.Grant, application *dataset.PolicyApplication) {
	fmt.Fprintf(w, "Grant expiry moved:\n")
	fmt.Fprintf(w, "  Grant ID:    %s\n", after.GrantID)
	fmt.Fprintf(w, "  Copy ID:     %s\n", after.CopyID)
	fmt.Fprintf(w, "  Dataset:     %s\n", after.DatasetURI)
	fmt.Fprintf(w, "  Account:     %s\n", after.ResearcherAccountID)
	fmt.Fprintf(w, "  Target VPC:  %s\n", after.TargetVpcID)
	fmt.Fprintf(w, "  State:       %s\n", after.State)
	fmt.Fprintf(w, "  Was:         %s\n", formatExpiry(before.ExpiresAt))
	fmt.Fprintf(w, "  Expiry:      %s\n", formatExpiry(after.ExpiresAt))
	fmt.Fprintf(w, "  Deployed:    %s\n", formatDeployedGrants(application))
	fmt.Fprintf(w, "  Applied:     %s\n", formatAppliedTargets(application))
}
