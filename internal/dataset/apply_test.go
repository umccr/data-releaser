package dataset

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/umccr/data-releaser/internal/awstest"
)

// Fixtures for the applier, kept separate from the renderer's so that a change to either
// set cannot silently alter what the other is asserting.
const (
	applyOwnerAccount      = "999988887777"
	applyResearcherAccount = "210987654321"
	applyDatasetURI        = "s3://ceph-source/datasets/cohort-b/"
	applyGrantID           = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	applyBucket            = "hot-cache-cohort-b"
	applyKeyArn            = "arn:aws:kms:ap-southeast-2:999988887777:key/8f7e6d5c-4b3a-2019-8877-665544332211"
	applyVpcID             = "vpc-00112233445566778"
)

func applierSettings() PolicySettings {
	return PolicySettings{
		AccountID:          applyOwnerAccount,
		Region:             "ap-southeast-2",
		CopyWriterRoleName: "copy-writer",
		CustodianRoleARN:   "arn:aws:iam::999988887777:role/dataset-custodian",
	}
}

// applierTarget is one dataset's copy carrying a single grant in the given state. The
// grant fields stay populated whichever state it is in, since a withdrawn grant keeps them
// and what stops it being written is the state alone.
func applierTarget(state GrantState) PolicyTarget {
	expiry := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	return PolicyTarget{
		DatasetURI:     applyDatasetURI,
		HotCacheBucket: applyBucket,
		KeyArn:         applyKeyArn,
		Grants: []*Grant{{
			GrantID:             applyGrantID,
			CopyID:              "01JCCCCCCCCCCCCCCCCCCCCCCC",
			DatasetURI:          applyDatasetURI,
			HotCacheBucket:      applyBucket,
			KeyArn:              applyKeyArn,
			ResearcherAccountID: applyResearcherAccount,
			TargetVpcID:         applyVpcID,
			State:               state,
			CreatedAt:           time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC),
			ExpiresAt:           &expiry,
		}},
	}
}

// recordingDoubles returns S3 and KMS doubles that append to a shared write log, so the
// order of the two puts is observable across the two services.
func recordingDoubles(keyState kmstypes.KeyState, order *[]ApplyTarget) (*awstest.S3, *awstest.KMS) {
	objects := &awstest.S3{
		PutBucketPolicyFn: func(_ context.Context, _ *s3.PutBucketPolicyInput) (*s3.PutBucketPolicyOutput, error) {
			*order = append(*order, ApplyTargetBucket)
			return &s3.PutBucketPolicyOutput{}, nil
		},
	}
	keys := &awstest.KMS{
		DescribeKeyFn: func(_ context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
			return &kms.DescribeKeyOutput{KeyMetadata: &kmstypes.KeyMetadata{
				KeyId:    params.KeyId,
				Enabled:  keyState == kmstypes.KeyStateEnabled,
				KeyState: keyState,
			}}, nil
		},
		PutKeyPolicyFn: func(_ context.Context, _ *kms.PutKeyPolicyInput) (*kms.PutKeyPolicyOutput, error) {
			*order = append(*order, ApplyTargetKey)
			return &kms.PutKeyPolicyOutput{}, nil
		},
	}
	return objects, keys
}

// CheckGrantable is the same question Apply asks of the key, asked before a caller has
// committed to anything. It has to answer for a copy carrying no grant yet, since that is
// exactly the moment it is useful: the caller is about to record the first one.
func TestCheckGrantableAnswersForTheKeyAndWritesNothing(t *testing.T) {
	// The copy has no grant against it, so nothing in the target could make the answer
	// come from anywhere but the key.
	target := PolicyTarget{
		DatasetURI:     applyDatasetURI,
		HotCacheBucket: applyBucket,
		KeyArn:         applyKeyArn,
	}

	for _, tc := range []struct {
		state   kmstypes.KeyState
		refused bool
	}{
		{state: kmstypes.KeyStateEnabled},
		{state: kmstypes.KeyStateDisabled, refused: true},
		{state: kmstypes.KeyStatePendingDeletion, refused: true},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			var order []ApplyTarget
			objects, keys := recordingDoubles(tc.state, &order)

			err := NewPolicyApplier(applierSettings(), objects, keys).
				CheckGrantable(context.Background(), target)

			var notGrantable *KeyNotGrantableError
			switch {
			case tc.refused && !errors.As(err, &notGrantable):
				t.Fatalf("expected a refusal naming the key state, got %v", err)
			case tc.refused && notGrantable.KeyState != tc.state:
				t.Errorf("refusal names key state %q, want %q", notGrantable.KeyState, tc.state)
			case tc.refused && len(notGrantable.GrantIDs) != 0:
				t.Errorf("the refusal names grants %v; nothing has been recorded to name",
					notGrantable.GrantIDs)
			case !tc.refused && err != nil:
				t.Fatalf("CheckGrantable: %v", err)
			}

			if len(order) != 0 {
				t.Errorf("a read-only check wrote %v", order)
			}
		})
	}
}

func TestApplyGrantsWithTheBucketPolicyWrittenLast(t *testing.T) {
	var order []ApplyTarget
	objects, keys := recordingDoubles(kmstypes.KeyStateEnabled, &order)

	application, err := NewPolicyApplier(applierSettings(), objects, keys).
		Apply(context.Background(), applierTarget(GrantStateGranted))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The bucket policy is what admits a request, so it is written once the key can
	// already serve the reads it will admit.
	want := []ApplyTarget{ApplyTargetKey, ApplyTargetBucket}
	if !equalTargets(order, want) {
		t.Errorf("documents written in order %v, want %v", order, want)
	}
	if !equalTargets(application.Written, want) {
		t.Errorf("application reports %v written, want %v", application.Written, want)
	}
	if !application.Granting {
		t.Error("application of a deployed grant does not report itself as granting")
	}
	if got := application.GrantIDs; len(got) != 1 || got[0] != applyGrantID {
		t.Errorf("application reports grants %v written, want [%s]", got, applyGrantID)
	}
	if application.KeyPolicyLeftAlone != "" {
		t.Errorf("key policy reported as left alone: %s", application.KeyPolicyLeftAlone)
	}

	// The documents that reached AWS are the rendered ones, not something composed on
	// the way out.
	if got := len(objects.PutBucketPolicyCalls); got != 1 {
		t.Fatalf("bucket policy written %d times, want 1", got)
	}
	if got := aws.ToString(objects.PutBucketPolicyCalls[0].Policy); got != application.BucketDocument {
		t.Errorf("bucket policy put differs from the document rendered:\n%s", got)
	}
	if got := aws.ToString(objects.PutBucketPolicyCalls[0].Bucket); got != applyBucket {
		t.Errorf("bucket policy put on %q, want %q", got, applyBucket)
	}
	if got := len(keys.PutKeyPolicyCalls); got != 1 {
		t.Fatalf("key policy written %d times, want 1", got)
	}
	if got := aws.ToString(keys.PutKeyPolicyCalls[0].Policy); got != application.KeyDocument {
		t.Errorf("key policy put differs from the document rendered:\n%s", got)
	}
	if got := aws.ToString(keys.PutKeyPolicyCalls[0].KeyId); got != applyKeyArn {
		t.Errorf("key policy put on %q, want %q", got, applyKeyArn)
	}

	// The grant is in the documents, which is what makes the withdrawal assertions below
	// mean something.
	if !strings.Contains(application.BucketDocument, applyResearcherAccount) {
		t.Error("bucket policy granting access names no researcher account")
	}
	if !strings.Contains(application.KeyDocument, applyResearcherAccount) {
		t.Error("key policy granting access names no researcher account")
	}
}

func TestApplyWithdrawsWithTheBucketPolicyWrittenFirst(t *testing.T) {
	var order []ApplyTarget
	objects, keys := recordingDoubles(kmstypes.KeyStateEnabled, &order)

	application, err := NewPolicyApplier(applierSettings(), objects, keys).
		Apply(context.Background(), applierTarget(GrantStateRevoked))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Closing the gate is what stops access, so it is the first thing written rather
	// than the last: a failure after it has already withdrawn reads and listings alike.
	want := []ApplyTarget{ApplyTargetBucket, ApplyTargetKey}
	if !equalTargets(order, want) {
		t.Errorf("documents written in order %v, want %v", order, want)
	}
	if application.Granting {
		t.Error("application of a withdrawn grant reports itself as granting")
	}
	if len(application.GrantIDs) != 0 {
		t.Errorf("application of a withdrawn grant reports %v as written", application.GrantIDs)
	}

	// Withdrawing is the same render with the grant left out, so neither document may
	// name the researcher or their VPC any more.
	for _, document := range []struct {
		target ApplyTarget
		body   string
	}{
		{ApplyTargetBucket, application.BucketDocument},
		{ApplyTargetKey, application.KeyDocument},
	} {
		if strings.Contains(document.body, applyResearcherAccount) {
			t.Errorf("withdrawn %s still names the researcher account:\n%s", document.target, document.body)
		}
		if strings.Contains(document.body, applyVpcID) {
			t.Errorf("withdrawn %s still names the target VPC:\n%s", document.target, document.body)
		}
	}
}

func TestApplyRefusesToGrantThroughAnUnusableKey(t *testing.T) {
	tests := []struct {
		name            string
		keyState        kmstypes.KeyState
		pendingDeletion bool
	}{
		{"disabled", kmstypes.KeyStateDisabled, false},
		{"pending deletion", kmstypes.KeyStatePendingDeletion, true},
		{"pending replica deletion", kmstypes.KeyStatePendingReplicaDeletion, true},
		{"unavailable", kmstypes.KeyStateUnavailable, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var order []ApplyTarget
			objects, keys := recordingDoubles(tc.keyState, &order)

			_, err := NewPolicyApplier(applierSettings(), objects, keys).
				Apply(context.Background(), applierTarget(GrantStateGranted))

			var refused *KeyNotGrantableError
			if !errors.As(err, &refused) {
				t.Fatalf("Apply returned %v, want a refusal naming the key state", err)
			}
			if refused.KeyState != tc.keyState {
				t.Errorf("refusal reports key state %q, want %q", refused.KeyState, tc.keyState)
			}
			if refused.PendingDeletion() != tc.pendingDeletion {
				t.Errorf("refusal reports pending deletion %v, want %v",
					refused.PendingDeletion(), tc.pendingDeletion)
			}

			// The check is what makes the refusal safe: an unusable key must be found
			// before either document is touched, or a refused release still moves access.
			if len(order) != 0 {
				t.Errorf("documents written despite the refusal: %v", order)
			}
			if len(objects.PutBucketPolicyCalls) != 0 || len(keys.PutKeyPolicyCalls) != 0 {
				t.Error("a policy was written for a target that was refused")
			}
		})
	}
}

func TestApplyStillWithdrawsThroughADisabledKey(t *testing.T) {
	var order []ApplyTarget
	objects, keys := recordingDoubles(kmstypes.KeyStateDisabled, &order)

	// A disabled key refuses a grant but must not refuse a withdrawal: the key being
	// switched off is no reason to leave granting statements deployed.
	if _, err := NewPolicyApplier(applierSettings(), objects, keys).
		Apply(context.Background(), applierTarget(GrantStateRevoked)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := []ApplyTarget{ApplyTargetBucket, ApplyTargetKey}
	if !equalTargets(order, want) {
		t.Errorf("documents written in order %v, want %v", order, want)
	}
}

func TestApplyLeavesAKeyPendingDeletionAloneButStillClosesTheBucket(t *testing.T) {
	var order []ApplyTarget
	objects, keys := recordingDoubles(kmstypes.KeyStatePendingDeletion, &order)

	application, err := NewPolicyApplier(applierSettings(), objects, keys).
		Apply(context.Background(), applierTarget(GrantStateRevoked))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The key accepts no further policy once its deletion is scheduled, and needs none:
	// whatever it grants is destroyed with it. The bucket policy is still what turns a
	// request away until then.
	if !equalTargets(order, []ApplyTarget{ApplyTargetBucket}) {
		t.Errorf("documents written in order %v, want the bucket policy only", order)
	}
	if !application.Wrote(ApplyTargetBucket) {
		t.Error("application does not report the bucket policy as written")
	}
	if application.Wrote(ApplyTargetKey) {
		t.Error("application reports the key policy as written")
	}
	if application.KeyPolicyLeftAlone == "" {
		t.Error("application does not explain why the key policy was left alone")
	}
	if len(keys.PutKeyPolicyCalls) != 0 {
		t.Error("a policy was written to a key whose deletion is scheduled")
	}
}

func TestApplyReportsWhatIsInForceWhenTheSecondWriteFails(t *testing.T) {
	tests := []struct {
		name     string
		state    GrantState
		failing  ApplyTarget
		applied  ApplyTarget
		granting bool
		// expects is a phrase the report has to contain, since the point of the report
		// is that it says what access is now in force.
		expects string
	}{
		{
			name:     "granting, the bucket write fails",
			state:    GrantStateGranted,
			failing:  ApplyTargetBucket,
			applied:  ApplyTargetKey,
			granting: true,
			expects:  "no access has been granted",
		},
		{
			name:     "withdrawing, the key write fails",
			state:    GrantStateRevoked,
			failing:  ApplyTargetKey,
			applied:  ApplyTargetBucket,
			granting: false,
			expects:  "their access has stopped",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var order []ApplyTarget
			objects, keys := recordingDoubles(kmstypes.KeyStateEnabled, &order)
			failure := errors.New("access denied")
			switch tc.failing {
			case ApplyTargetBucket:
				objects.PutBucketPolicyFn = func(_ context.Context, _ *s3.PutBucketPolicyInput) (*s3.PutBucketPolicyOutput, error) {
					return nil, failure
				}
			case ApplyTargetKey:
				keys.PutKeyPolicyFn = func(_ context.Context, _ *kms.PutKeyPolicyInput) (*kms.PutKeyPolicyOutput, error) {
					return nil, failure
				}
			}

			_, err := NewPolicyApplier(applierSettings(), objects, keys).
				Apply(context.Background(), applierTarget(tc.state))

			var partial *PartialPolicyApplicationError
			if !errors.As(err, &partial) {
				t.Fatalf("Apply returned %v, want a report of a partial application", err)
			}
			if partial.Applied != tc.applied || partial.Failed != tc.failing {
				t.Errorf("report says %q was applied and %q failed, want %q and %q",
					partial.Applied, partial.Failed, tc.applied, tc.failing)
			}
			if partial.Granting != tc.granting {
				t.Errorf("report says granting=%v, want %v", partial.Granting, tc.granting)
			}
			if !strings.Contains(err.Error(), tc.expects) {
				t.Errorf("report does not say what access is in force, wanted %q in:\n%s",
					tc.expects, err.Error())
			}
			if !errors.Is(err, failure) {
				t.Error("report does not carry the underlying failure")
			}
		})
	}
}

func TestApplyChangesNothingWhenTheFirstWriteFails(t *testing.T) {
	var order []ApplyTarget
	objects, keys := recordingDoubles(kmstypes.KeyStateEnabled, &order)
	failure := errors.New("access denied")
	// The key policy is written first when granting, so failing it leaves both documents
	// as they were.
	keys.PutKeyPolicyFn = func(_ context.Context, _ *kms.PutKeyPolicyInput) (*kms.PutKeyPolicyOutput, error) {
		return nil, failure
	}

	_, err := NewPolicyApplier(applierSettings(), objects, keys).
		Apply(context.Background(), applierTarget(GrantStateGranted))
	if err == nil {
		t.Fatal("Apply succeeded despite the key policy write failing")
	}

	var partial *PartialPolicyApplicationError
	if errors.As(err, &partial) {
		t.Errorf("a failed first write reported as a partial application: %v", err)
	}
	if !errors.Is(err, failure) {
		t.Error("report does not carry the underlying failure")
	}
	if len(objects.PutBucketPolicyCalls) != 0 {
		t.Error("the bucket policy was written after the key policy write failed")
	}
}

func TestApplyRefusesAKeyThatReportsNoState(t *testing.T) {
	objects := &awstest.S3{}
	keys := &awstest.KMS{
		DescribeKeyFn: func(_ context.Context, _ *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
			return &kms.DescribeKeyOutput{}, nil
		},
	}

	_, err := NewPolicyApplier(applierSettings(), objects, keys).
		Apply(context.Background(), applierTarget(GrantStateGranted))

	var missing *MissingPrerequisiteError
	if !errors.As(err, &missing) {
		t.Fatalf("Apply returned %v, want the key reported as an unmet prerequisite", err)
	}
	if len(objects.PutBucketPolicyCalls) != 0 || len(keys.PutKeyPolicyCalls) != 0 {
		t.Error("a policy was written without the key's state being established")
	}
}

func equalTargets(got, want []ApplyTarget) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
