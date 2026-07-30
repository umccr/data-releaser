package validate

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// policyMockS3Client implements internalaws.S3Client for policy-grant checks.
type policyMockS3Client struct {
	getBucketPolicyFn func(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error)
}

func (m *policyMockS3Client) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{}, nil
}
func (m *policyMockS3Client) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}
func (m *policyMockS3Client) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return &s3.HeadObjectOutput{}, nil
}
func (m *policyMockS3Client) HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, nil
}
func (m *policyMockS3Client) GetBucketPolicy(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
	if m.getBucketPolicyFn != nil {
		return m.getBucketPolicyFn(ctx, params, optFns...)
	}
	return &s3.GetBucketPolicyOutput{}, nil
}

const (
	testBucket    = "data-control-manifest-bucket"
	testAccountID = "534840902377"
	testPrefix    = "manifests/534840902377/*"
)

func policyClient(policy string, err error) *policyMockS3Client {
	return &policyMockS3Client{
		getBucketPolicyFn: func(ctx context.Context, params *s3.GetBucketPolicyInput, optFns ...func(*s3.Options)) (*s3.GetBucketPolicyOutput, error) {
			if err != nil {
				return nil, err
			}
			if policy == "" {
				return &s3.GetBucketPolicyOutput{}, nil
			}
			return &s3.GetBucketPolicyOutput{Policy: aws.String(policy)}, nil
		},
	}
}

func TestCheckManifestBucketPolicy_GrantPresent(t *testing.T) {
	policy := `{
      "Version": "2012-10-17",
      "Statement": [
        {
          "Effect": "Allow",
          "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
          "Action": ["s3:GetObject"],
          "Resource": ["arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"]
        }
      ]
    }`

	check := checkManifestBucketPolicy(context.Background(), policyClient(policy, nil), testBucket, testPrefix, testAccountID)
	if !check.Passed {
		t.Fatalf("expected grant present, got failure: %s", check.Error)
	}
}

func TestCheckManifestBucketPolicy_GrantMissing(t *testing.T) {
	// Policy grants a different account only.
	policy := `{
      "Statement": [
        {
          "Effect": "Allow",
          "Principal": {"AWS": "arn:aws:iam::999999999999:root"},
          "Action": "s3:GetObject",
          "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/999999999999/*"
        }
      ]
    }`

	check := checkManifestBucketPolicy(context.Background(), policyClient(policy, nil), testBucket, testPrefix, testAccountID)
	if check.Passed {
		t.Fatal("expected grant missing to fail the check")
	}
	if check.Error == "" {
		t.Error("expected a descriptive error message about the missing onboarding grant")
	}
}

func TestCheckManifestBucketPolicy_NoPolicy(t *testing.T) {
	check := checkManifestBucketPolicy(context.Background(), policyClient("", nil), testBucket, testPrefix, testAccountID)
	if check.Passed {
		t.Fatal("expected empty policy to fail the check")
	}
}

func TestCheckManifestBucketPolicy_GetError(t *testing.T) {
	check := checkManifestBucketPolicy(context.Background(), policyClient("", errors.New("no such bucket policy")), testBucket, testPrefix, testAccountID)
	if check.Passed {
		t.Fatal("expected GetBucketPolicy error to fail the check")
	}
}

func TestCheckManifestBucketPolicy_BucketNotConfigured(t *testing.T) {
	check := checkManifestBucketPolicy(context.Background(), policyClient("", nil), "", testPrefix, testAccountID)
	if check.Passed {
		t.Fatal("expected unconfigured bucket to fail the check")
	}
}

func TestCheckManifestBucketPolicy_WildcardAction(t *testing.T) {
	policy := `{
      "Statement": [
        {
          "Effect": "Allow",
          "Principal": "arn:aws:iam::534840902377:root",
          "Action": "s3:*",
          "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
        }
      ]
    }`

	check := checkManifestBucketPolicy(context.Background(), policyClient(policy, nil), testBucket, testPrefix, testAccountID)
	if !check.Passed {
		t.Fatalf("expected wildcard action to satisfy grant, got: %s", check.Error)
	}
}
