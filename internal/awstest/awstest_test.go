package awstest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
)

// The doubles below carry a little behaviour of their own — a defaulted pending window,
// a readable body — and a wrong default silently weakens every test that relies on it.

func TestS3_GetObjectDefaultBodyIsReadable(t *testing.T) {
	client := &S3{}

	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("key"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if out.Body == nil {
		t.Fatal("GetObject returned a nil body; callers read and close it unconditionally")
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected an empty body by default, got %q", body)
	}
}

func TestS3_RecordsPolicyWrites(t *testing.T) {
	client := &S3{}
	ctx := context.Background()

	if _, err := client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("bucket"),
		Policy: aws.String(`{"Version":"2012-10-17"}`),
	}); err != nil {
		t.Fatalf("PutBucketPolicy: %v", err)
	}
	if _, err := client.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{
		Bucket: aws.String("bucket"),
	}); err != nil {
		t.Fatalf("DeleteBucketPolicy: %v", err)
	}

	if len(client.PutBucketPolicyCalls) != 1 {
		t.Fatalf("expected 1 recorded policy write, got %d", len(client.PutBucketPolicyCalls))
	}
	if got := aws.ToString(client.PutBucketPolicyCalls[0].Policy); got != `{"Version":"2012-10-17"}` {
		t.Errorf("recorded policy = %q", got)
	}
	if len(client.DeleteBucketPolicyCalls) != 1 {
		t.Fatalf("expected 1 recorded policy delete, got %d", len(client.DeleteBucketPolicyCalls))
	}
}

func TestKMS_DescribeKeyDefaultsToEnabled(t *testing.T) {
	client := &KMS{}

	out, err := client.DescribeKey(context.Background(), &kms.DescribeKeyInput{
		KeyId: aws.String("alias/dataset"),
	})
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if out.KeyMetadata == nil {
		t.Fatal("DescribeKey returned no key metadata")
	}
	if !out.KeyMetadata.Enabled {
		t.Error("expected an enabled key by default")
	}
	if out.KeyMetadata.KeyState != kmstypes.KeyStateEnabled {
		t.Errorf("key state = %q, want %q", out.KeyMetadata.KeyState, kmstypes.KeyStateEnabled)
	}
	if got := aws.ToString(out.KeyMetadata.KeyId); got != "alias/dataset" {
		t.Errorf("key id = %q, want the requested id echoed back", got)
	}
}

func TestKMS_ScheduleKeyDeletionWindow(t *testing.T) {
	tests := []struct {
		name       string
		window     *int32
		wantWindow int32
	}{
		{name: "defaulted when absent", window: nil, wantWindow: defaultPendingWindowDays},
		{name: "honoured when given", window: aws.Int32(7), wantWindow: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &KMS{}
			before := time.Now().UTC()

			out, err := client.ScheduleKeyDeletion(context.Background(), &kms.ScheduleKeyDeletionInput{
				KeyId:               aws.String("key-id"),
				PendingWindowInDays: tt.window,
			})
			if err != nil {
				t.Fatalf("ScheduleKeyDeletion: %v", err)
			}

			if got := aws.ToInt32(out.PendingWindowInDays); got != tt.wantWindow {
				t.Errorf("pending window = %d, want %d", got, tt.wantWindow)
			}
			if out.DeletionDate == nil {
				t.Fatal("no deletion date reported")
			}
			if !out.DeletionDate.After(before) {
				t.Errorf("deletion date %v is not in the future", out.DeletionDate)
			}
			if out.KeyState != kmstypes.KeyStatePendingDeletion {
				t.Errorf("key state = %q, want %q", out.KeyState, kmstypes.KeyStatePendingDeletion)
			}
			if len(client.ScheduleDeletionCalls) != 1 {
				t.Errorf("expected 1 recorded call, got %d", len(client.ScheduleDeletionCalls))
			}
		})
	}
}

func TestStepFunctions_StartExecutionRecordsAndNamesExecution(t *testing.T) {
	client := &StepFunctions{}

	out, err := client.StartExecution(context.Background(), &sfn.StartExecutionInput{
		StateMachineArn: aws.String("arn:aws:states:ap-southeast-2:000000000000:stateMachine:copy"),
		Name:            aws.String("run-1"),
		Input:           aws.String(`{}`),
	})
	if err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	if got := aws.ToString(out.ExecutionArn); got == "" {
		t.Fatal("no execution ARN reported; callers persist it as the copy reference")
	} else if want := "run-1"; got[len(got)-len(want):] != want {
		t.Errorf("execution ARN %q does not end in the requested name %q", got, want)
	}
	if len(client.StartCalls) != 1 {
		t.Fatalf("expected 1 recorded start, got %d", len(client.StartCalls))
	}
}
