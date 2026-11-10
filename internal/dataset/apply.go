package dataset

// This file writes the two documents a dataset's copy is enforced by. Rendering them is
// policy.go's business; here they are put on the bucket and on the key, in an order
// chosen for what a failure between the two writes leaves behind.
//
// Withdrawing access is not a separate operation. The rendered pair names only the grants
// still deployed against the copy, so applying a target whose last grant has been revoked
// writes exactly the baseline pair -- the same pair a copy nobody was ever granted has.
// There is therefore one write path, and a caller withdraws by applying the target it has
// already taken the grant out of.
//
// # Which document is written first, and why
//
// The bucket policy is the gate. It decides whether a request can address the objects at
// all, and it is evaluated before S3 asks the key to decrypt anything. The key policy
// only decides whether a request that has already got past the gate can read what it
// found. So:
//
//   - Granting writes the key policy first and the bucket policy last. A decrypt
//     permission conditioned on S3 in one region, for a bucket the researcher cannot yet
//     address, is unusable: the grant becomes real at the last write and not before.
//   - Withdrawing writes the bucket policy first and the key policy last. Closing the
//     gate is what stops access, so it happens at the first write rather than the last.
//
// A failure between the two writes therefore leaves the safe half in place in both
// directions, and the report says which:
//
//   - Granting, bucket write failed: the researchers can decrypt this dataset's objects
//     but cannot address them, and cannot list the bucket either. Nothing is readable and
//     nothing has been disclosed.
//   - Withdrawing, key write failed: the researchers can no longer address the bucket, so
//     reads and listings have both stopped. Decrypt permissions they cannot use are left
//     on the key.
//
// The ordering matters most in the withdrawing direction, and for the listing rather than
// the objects. Removing the decrypt permissions first would stop reads but leave
// s3:ListBucket working, and the keys of a genomic dataset routinely carry the sample
// identifiers the release exists to control.
//
// A target carrying several grants is written the same way and in one pair of puts, since
// the documents are shared: adding or withdrawing one grant re-renders both documents
// whole, with the other grants' statements in them exactly as they were.
//
// The bucket policy is replaced and never deleted. The rendered document always carries
// the deny on requests that did not arrive over TLS, so an absent policy would be weaker
// than the baseline rather than a neutral starting point.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// keyPolicyName is the only name a KMS key's policy has. It is sent explicitly so the
// call says which document it is replacing.
const keyPolicyName = "default"

// ApplyTarget names one of the two documents access is enforced by.
type ApplyTarget string

const (
	// ApplyTargetBucket is the cache bucket's policy.
	ApplyTargetBucket ApplyTarget = "bucket policy"
	// ApplyTargetKey is the policy of the key encrypting that bucket.
	ApplyTargetKey ApplyTarget = "key policy"
)

func (t ApplyTarget) String() string {
	return string(t)
}

// PolicyApplier writes rendered policy documents to a dataset's cache bucket and to the
// key encrypting it.
type PolicyApplier struct {
	objects  internalaws.S3Client
	keys     internalaws.KMSClient
	settings PolicySettings
}

// NewPolicyApplier returns an applier that renders with the given settings.
func NewPolicyApplier(settings PolicySettings, objects internalaws.S3Client, keys internalaws.KMSClient) *PolicyApplier {
	return &PolicyApplier{
		objects:  objects,
		keys:     keys,
		settings: settings,
	}
}

// PolicyApplication is what one application wrote.
type PolicyApplication struct {
	// DatasetURI, HotCacheBucket, and KeyArn identify what was written and where.
	DatasetURI     string
	HotCacheBucket string
	KeyArn         string

	// GrantIDs lists the grants whose statements the written documents carry, in the
	// order they were written. Empty for a withdrawal, which is how one is distinguished
	// from a grant after the fact.
	GrantIDs []string

	// Granting reports whether the documents written admit anybody. False when no grant
	// against the copy is still deployed.
	Granting bool

	// KeyState is the state the key was observed in before anything was written.
	KeyState kmstypes.KeyState

	// BucketDocument and KeyDocument are the documents that were rendered. KeyDocument
	// is rendered even when it is not written, so a report can show what would have
	// been put.
	BucketDocument string
	KeyDocument    string

	// Written lists the documents that were put, in the order they were put in.
	Written []ApplyTarget

	// KeyPolicyLeftAlone explains why the key policy was not written, and is empty when
	// it was. It is only ever set for a withdrawal from a key whose deletion is already
	// scheduled.
	KeyPolicyLeftAlone string
}

// Wrote reports whether the named document was put.
func (a *PolicyApplication) Wrote(target ApplyTarget) bool {
	return a != nil && slices.Contains(a.Written, target)
}

// CheckGrantable reports whether the copy's key is in a state a new grant could be
// deployed under. It reads and writes nothing.
//
// It is the same read Apply makes and the same refusal, offered on its own so a caller can
// ask before it commits to anything. The case it exists for is a key disabled or scheduled
// for deletion outside this tool: that leaves the ledger behind, so the copy still reads as
// staged while the data it protects is already unreadable, and a caller that records a
// grant before deploying it would otherwise write a record it then cannot make real.
//
// The target's own grants are not considered. The question is whether a grant that does not
// exist yet could be deployed, so the key has to be enabled whatever is or is not standing
// against the copy already.
func (a *PolicyApplier) CheckGrantable(ctx context.Context, target PolicyTarget) error {
	keyState, err := a.observeKeyState(ctx, target)
	if err != nil {
		return err
	}
	if keyState == kmstypes.KeyStateEnabled {
		return nil
	}
	// No grant is named: nothing has been recorded, so there is none to report as
	// recorded and not deployed.
	return &KeyNotGrantableError{
		DatasetURI:     target.DatasetURI,
		HotCacheBucket: target.HotCacheBucket,
		KeyArn:         target.KeyArn,
		KeyState:       keyState,
	}
}

// Apply renders the policies for the target as it currently stands and writes both
// documents.
//
// It is the whole of granting access and the whole of withdrawing it. A target carrying a
// deployed grant renders a pair naming its researcher; one whose grants have all been
// revoked renders the baseline pair, which is what removes them. Nothing else composes
// either document, so applying always replaces whatever is deployed with what the ledger
// describes -- which is also what makes it the repair for a document edited outside this
// tool.
//
// Nothing is written until the key has been inspected, so an application refused for the
// state of its key leaves both documents exactly as they were.
func (a *PolicyApplier) Apply(ctx context.Context, target PolicyTarget) (*PolicyApplication, error) {
	policies, err := RenderPolicies(target, a.settings)
	if err != nil {
		return nil, err
	}
	bucketDocument, err := policies.BucketPolicyJSON()
	if err != nil {
		return nil, err
	}
	keyDocument, err := policies.KeyPolicyJSON()
	if err != nil {
		return nil, err
	}

	granting := target.granting()

	keyState, err := a.observeKeyState(ctx, target)
	if err != nil {
		return nil, err
	}
	// A grant needs a working key. Access granted through a key that cannot decrypt, or
	// through one already being destroyed, presents to the researcher as an access
	// denial with no cause they can see and none we could distinguish from a wrong VPC
	// or a passed expiry. A withdrawal needs no such check: refusing to reduce access
	// because the key is unwell would leave a grant standing.
	if granting && keyState != kmstypes.KeyStateEnabled {
		return nil, &KeyNotGrantableError{
			DatasetURI:     target.DatasetURI,
			HotCacheBucket: target.HotCacheBucket,
			KeyArn:         target.KeyArn,
			GrantIDs:       target.deployedGrantIDs(),
			KeyState:       keyState,
		}
	}

	application := &PolicyApplication{
		DatasetURI:     target.DatasetURI,
		HotCacheBucket: target.HotCacheBucket,
		KeyArn:         target.KeyArn,
		GrantIDs:       target.deployedGrantIDs(),
		Granting:       granting,
		KeyState:       keyState,
		BucketDocument: bucketDocument,
		KeyDocument:    keyDocument,
	}

	writes := []policyWrite{
		{target: ApplyTargetKey, put: func(ctx context.Context) error {
			return a.putKeyPolicy(ctx, target, keyDocument)
		}},
		{target: ApplyTargetBucket, put: func(ctx context.Context) error {
			return a.putBucketPolicy(ctx, target, bucketDocument)
		}},
	}
	if !granting {
		// The gate closes first when access is being withdrawn.
		slices.Reverse(writes)

		// A key whose deletion is scheduled accepts no new policy, and needs none: the
		// grants it carries die with the key. The bucket policy is still worth writing,
		// since it is what a request is turned away by in the meantime.
		if keyIsPendingDeletion(keyState) {
			writes = slices.DeleteFunc(writes, func(w policyWrite) bool {
				return w.target == ApplyTargetKey
			})
			application.KeyPolicyLeftAlone = fmt.Sprintf(
				"the key is %s and accepts no further policy; the grants it carries are destroyed with it",
				keyState)
		}
	}

	for i, write := range writes {
		if err := write.put(ctx); err != nil {
			if i == 0 {
				// Nothing has been written, so access is whatever it already was.
				return nil, fmt.Errorf("the deployed policies of dataset %q were left exactly as they were: %w",
					target.DatasetURI, err)
			}
			return nil, &PartialPolicyApplicationError{
				DatasetURI:     target.DatasetURI,
				HotCacheBucket: target.HotCacheBucket,
				Granting:       granting,
				Applied:        writes[i-1].target,
				Failed:         write.target,
				Cause:          err,
			}
		}
		application.Written = append(application.Written, write.target)
	}

	return application, nil
}

// policyWrite is one document being put, named so the order can be rearranged and
// reported on rather than being spelled out twice.
type policyWrite struct {
	target ApplyTarget
	put    func(ctx context.Context) error
}

// observeKeyState reports the state of the key encrypting the dataset's cache bucket.
func (a *PolicyApplier) observeKeyState(ctx context.Context, target PolicyTarget) (kmstypes.KeyState, error) {
	prerequisite := DatasetKeyPrerequisite(target.KeyArn, a.settings.AccountID)

	described, err := a.keys.DescribeKey(ctx, &kms.DescribeKeyInput{
		KeyId: aws.String(target.KeyArn),
	})
	if err != nil {
		return "", prerequisite.Missing(fmt.Errorf(
			"reading the state of the key encrypting %q before changing its access: %w",
			target.HotCacheBucket, err))
	}
	if described.KeyMetadata == nil || strings.TrimSpace(string(described.KeyMetadata.KeyState)) == "" {
		return "", prerequisite.Missingf(
			"the key was found but reported no state, so whether it can decrypt %q cannot be established",
			target.HotCacheBucket)
	}
	return described.KeyMetadata.KeyState, nil
}

// keyIsPendingDeletion reports whether the key is on its way to being destroyed. Both
// forms count: a replica pending deletion is as unwritable as a primary, and either way
// the objects encrypted under it are being taken away.
func keyIsPendingDeletion(state kmstypes.KeyState) bool {
	return state == kmstypes.KeyStatePendingDeletion || state == kmstypes.KeyStatePendingReplicaDeletion
}

// putBucketPolicy replaces the cache bucket's policy.
//
// The lockout guard S3 offers is deliberately left at its default, so a document that
// would take the owning account's own access away is rejected by S3 rather than written.
// The renderer will not produce such a document, and this is the check on that.
func (a *PolicyApplier) putBucketPolicy(ctx context.Context, target PolicyTarget, document string) error {
	_, err := a.objects.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(target.HotCacheBucket),
		Policy: aws.String(document),
	})
	if err != nil {
		return CacheBucketPrerequisite(target.HotCacheBucket, a.settings.AccountID).
			Missing(fmt.Errorf("writing the policy of the bucket holding dataset %q: %w",
				target.DatasetURI, err))
	}
	return nil
}

// putKeyPolicy replaces the policy of the key encrypting the cache bucket.
//
// The lockout safety check is left in force. The rendered document always names the
// owning account as a key administrator, so the check passes; bypassing it would only
// serve to permit a document that had lost that statement, and a key nobody can write a
// policy for can never be withdrawn.
func (a *PolicyApplier) putKeyPolicy(ctx context.Context, target PolicyTarget, document string) error {
	_, err := a.keys.PutKeyPolicy(ctx, &kms.PutKeyPolicyInput{
		KeyId:      aws.String(target.KeyArn),
		PolicyName: aws.String(keyPolicyName),
		Policy:     aws.String(document),
	})
	if err != nil {
		return DatasetKeyPrerequisite(target.KeyArn, a.settings.AccountID).
			Missing(fmt.Errorf("writing the policy of the key encrypting dataset %q: %w",
				target.DatasetURI, err))
	}
	return nil
}

// KeyNotGrantableError reports that access was not granted because the key encrypting the
// dataset cannot serve it. Nothing was written, so the deployed documents are unchanged.
type KeyNotGrantableError struct {
	DatasetURI     string
	HotCacheBucket string
	KeyArn         string
	// GrantIDs are the grants that would have been written.
	GrantIDs []string
	// KeyState is the state the key was found in.
	KeyState kmstypes.KeyState
}

// PendingDeletion reports whether the key is on its way to being destroyed, which is the
// difference between a dataset that has been withdrawn for good and one whose key was
// merely switched off.
func (e *KeyNotGrantableError) PendingDeletion() bool {
	return keyIsPendingDeletion(e.KeyState)
}

func (e *KeyNotGrantableError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "access to dataset %q cannot be granted: the key encrypting %s is %s",
		e.DatasetURI, e.HotCacheBucket, e.KeyState)

	switch {
	case e.PendingDeletion():
		b.WriteString(", so the copy it encrypts is already being destroyed and access to it " +
			"would be access to nothing readable; the dataset has to be staged again before it " +
			"can be released")
	case e.KeyState == kmstypes.KeyStateDisabled:
		b.WriteString(", so every read of the bucket would fail at decryption; enable the key " +
			"before granting access, since the researcher would otherwise see an access denial " +
			"indistinguishable from a wrong VPC or a passed expiry")
	default:
		b.WriteString(" rather than enabled, so it cannot decrypt the objects the grant would " +
			"cover; the grant would present as an access denial with no visible cause")
	}
	if len(e.GrantIDs) > 0 {
		fmt.Fprintf(&b, ". No policy was written, so grant %s is recorded and not deployed",
			strings.Join(e.GrantIDs, ", "))
		return b.String()
	}
	b.WriteString(". No policy was written")
	return b.String()
}

// PartialPolicyApplicationError reports that one of the two documents was written and the
// other was not.
//
// It exists to say what is in force, because the two documents only mean anything
// together and the halfway state is not something an operator can infer. In both
// directions the half that was written is the one that fails safe, which is what the
// write order is for.
type PartialPolicyApplicationError struct {
	DatasetURI     string
	HotCacheBucket string
	// Granting reports which direction the application was going in.
	Granting bool
	// Applied is the document that was written, and Failed the one that was not.
	Applied ApplyTarget
	Failed  ApplyTarget
	Cause   error
}

func (e *PartialPolicyApplicationError) Error() string {
	var b strings.Builder
	action := "withdrawing access to"
	if e.Granting {
		action = "granting access to"
	}
	fmt.Fprintf(&b, "%s dataset %q wrote the %s but not the %s: ", action, e.DatasetURI, e.Applied, e.Failed)

	if e.Granting {
		fmt.Fprintf(&b,
			"the researcher can decrypt this dataset's objects but cannot address or list %s, "+
				"so nothing is readable and no access has been granted. Apply the same records again "+
				"to finish, or withdraw them to take the decrypt permission back off the key",
			e.HotCacheBucket)
	} else {
		fmt.Fprintf(&b,
			"the researcher can no longer address or list %s, so their access has stopped, but a "+
				"decrypt permission they cannot use is left on the key. Apply the same records again "+
				"to remove it",
			e.HotCacheBucket)
	}
	fmt.Fprintf(&b, ": %v", e.Cause)
	return b.String()
}

func (e *PartialPolicyApplicationError) Unwrap() error {
	return e.Cause
}
