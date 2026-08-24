package validate

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/umccr/data-releaser/internal/awstest"
)

const (
	testBucket    = "data-control-manifest-bucket"
	testAccountID = "534840902377"
	testPrefix    = "manifests/534840902377/*"
)

// policyClient returns an S3 stub whose GetBucketPolicy serves the given policy
// document, or fails with err. An empty policy stands for a bucket with no policy at
// all, which GetBucketPolicy reports as an absent Policy field.
func policyClient(policy string, err error) *awstest.S3 {
	return &awstest.S3{
		GetBucketPolicyFn: func(ctx context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error) {
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

// TestCheckManifestBucketPolicy_GrantShapes covers the statement forms a real
// Manifest_Bucket policy can take, and the near-misses that substring matching used to
// accept: a principal whose ARN merely contains the account digits in a role path, a
// grant on a sibling account's prefix, and a grant on a different bucket.
func TestCheckManifestBucketPolicy_GrantShapes(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		wantPass  bool
	}{
		{
			name: "principal as AWS list",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": ["arn:aws:iam::999999999999:root", "arn:aws:iam::534840902377:root"]},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
            }`,
			wantPass: true,
		},
		{
			name: "principal as bare account ID",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "534840902377"},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
            }`,
			wantPass: true,
		},
		{
			name: "principal is everyone",
			statement: `{
              "Effect": "Allow",
              "Principal": "*",
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
            }`,
			wantPass: true,
		},
		{
			name: "resource wildcard above the prefix",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/*"
            }`,
			wantPass: true,
		},
		{
			name: "bare wildcard resource",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
              "Action": "*",
              "Resource": "*"
            }`,
			wantPass: true,
		},
		{
			name: "account digits appear only in an unrelated role path",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "arn:aws:iam::999999999999:role/534840902377/audit"},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
            }`,
			wantPass: false,
		},
		{
			name: "grant scoped to a sibling account's prefix",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/999999999999/*"
            }`,
			wantPass: false,
		},
		{
			name: "grant on a different bucket",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::some-other-bucket/manifests/534840902377/*"
            }`,
			wantPass: false,
		},
		{
			name: "action does not cover GetObject",
			statement: `{
              "Effect": "Allow",
              "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
              "Action": ["s3:PutObject", "s3:ListBucket"],
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
            }`,
			wantPass: false,
		},
		{
			name: "matching statement is a Deny",
			statement: `{
              "Effect": "Deny",
              "Principal": {"AWS": "arn:aws:iam::534840902377:root"},
              "Action": "s3:GetObject",
              "Resource": "arn:aws:s3:::data-control-manifest-bucket/manifests/534840902377/*"
            }`,
			wantPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := `{"Version": "2012-10-17", "Statement": [` + tt.statement + `]}`
			check := checkManifestBucketPolicy(context.Background(), policyClient(policy, nil), testBucket, testPrefix, testAccountID)
			if check.Passed != tt.wantPass {
				t.Fatalf("check passed = %v, want %v (error: %s)", check.Passed, tt.wantPass, check.Error)
			}
		})
	}
}

func TestCheckManifestBucketPolicy_MalformedJSON(t *testing.T) {
	check := checkManifestBucketPolicy(context.Background(), policyClient(`{"Statement": [`, nil), testBucket, testPrefix, testAccountID)
	if check.Passed {
		t.Fatal("expected malformed policy JSON to fail the check")
	}
}

func TestCheckManifestBucketPolicy_NoStatements(t *testing.T) {
	check := checkManifestBucketPolicy(context.Background(), policyClient(`{"Version": "2012-10-17"}`, nil), testBucket, testPrefix, testAccountID)
	if check.Passed {
		t.Fatal("expected a policy with no statements to fail the check")
	}
}
