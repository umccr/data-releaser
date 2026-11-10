package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/micahhausler/aws-iam-policy/policy"

	"github.com/umccr/data-releaser/internal/awstest"
)

// diffExpiry is the expiry the deployed documents in these tests are written with.
var diffExpiry = time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)

const diffStrangerAccount = "999999999999"

// absentBucketPolicyError stands in for what S3 answers with when a bucket carries no
// policy at all: an error carrying that code rather than a type of its own.
type absentBucketPolicyError struct{}

func (absentBucketPolicyError) Error() string {
	return "api error NoSuchBucketPolicy: The bucket policy does not exist"
}

func (absentBucketPolicyError) ErrorCode() string { return noSuchBucketPolicyCode }

// deployedDiffer returns a differ whose clients serve the given documents, each of
// which may be empty to stand for a document that is not deployed.
func deployedDiffer(bucketDocument, keyDocument string) *PolicyDiffer {
	objects := &awstest.S3{
		GetBucketPolicyFn: func(ctx context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error) {
			if bucketDocument == "" {
				return nil, absentBucketPolicyError{}
			}
			return &s3.GetBucketPolicyOutput{Policy: aws.String(bucketDocument)}, nil
		},
	}
	keys := &awstest.KMS{
		GetKeyPolicyFn: func(ctx context.Context, params *kms.GetKeyPolicyInput) (*kms.GetKeyPolicyOutput, error) {
			if keyDocument == "" {
				return &kms.GetKeyPolicyOutput{}, nil
			}
			return &kms.GetKeyPolicyOutput{Policy: aws.String(keyDocument)}, nil
		},
	}
	return NewPolicyDiffer(testSettings(), objects, keys)
}

// renderedDocuments returns the pair a target's grants describe, as the applier would
// have written them.
func renderedDocuments(t *testing.T, target PolicyTarget) (bucketDocument, keyDocument string) {
	t.Helper()

	policies, err := RenderPolicies(target, testSettings())
	if err != nil {
		t.Fatalf("rendering policies: %v", err)
	}
	bucketDocument, err = policies.BucketPolicyJSON()
	if err != nil {
		t.Fatalf("rendering the bucket policy: %v", err)
	}
	keyDocument, err = policies.KeyPolicyJSON()
	if err != nil {
		t.Fatalf("rendering the key policy: %v", err)
	}
	return bucketDocument, keyDocument
}

// editedDocument returns document with edit applied to every statement carrying sid,
// and to the document itself, standing in for a policy someone changed outside this
// tool.
func editedDocument(t *testing.T, document, sid string, edit func(doc *policy.Policy, st *policy.Statement)) string {
	t.Helper()

	var parsed policy.Policy
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("reading the document to edit: %v", err)
	}
	// The statement slice is the document's own, so editing an element in place edits the
	// document.
	statements := parsed.Statements.Values()
	for i := range statements {
		if statements[i].Sid != sid {
			continue
		}
		edit(&parsed, &statements[i])
	}

	edited, err := json.Marshal(&parsed)
	if err != nil {
		t.Fatalf("writing the edited document: %v", err)
	}
	return string(edited)
}

// onlyDifference returns the single difference the diff found, failing when it found
// any other number of them.
func onlyDifference(t *testing.T, diff *PolicyDiff) PolicyDifference {
	t.Helper()

	if len(diff.Differences) != 1 {
		t.Fatalf("expected exactly one difference, got %d:\n%s", len(diff.Differences), diff.Report())
	}
	return diff.Differences[0]
}

func TestDiffFindsNothingWhenTheDeployedPoliciesAreTheRenderedOnes(t *testing.T) {
	target := targetFixture(grantedFixture(&diffExpiry))
	bucketDocument, keyDocument := renderedDocuments(t, target)

	diff, err := deployedDiffer(bucketDocument, keyDocument).Diff(context.Background(), target)
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}
	if !diff.Matches() {
		t.Fatalf("expected the deployed policies to match the ledger, got:\n%s", diff.Report())
	}
}

// Two grants on one copy is the case that decides whether the comparison can be trusted at
// all: their statements share both documents, and a comparison reading the pair as one
// grant would report the other's account, VPC and expiry as drift on every run.
func TestDiffFindsNothingForACopyCarryingSeveralGrants(t *testing.T) {
	target := targetFixture(
		grantedFixture(&diffExpiry),
		grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil),
	)
	bucketDocument, keyDocument := renderedDocuments(t, target)

	diff, err := deployedDiffer(bucketDocument, keyDocument).Diff(context.Background(), target)
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}
	if !diff.Matches() {
		t.Fatalf("expected two deployed grants to match the two the ledger describes, got:\n%s", diff.Report())
	}
}

// Withdrawing one of two grants leaves the other deployed, so what is reported has to be
// the withdrawn researcher alone. Reporting the remaining one as well would have a
// reconciliation "repair" a grant that is doing exactly what the ledger asked.
func TestDiffReportsOnlyTheWithdrawnGrantOfACopyThatKeepsAnother(t *testing.T) {
	kept := grantedFixture(&diffExpiry)
	withdrawn := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)
	bucketDocument, keyDocument := renderedDocuments(t, targetFixture(kept, withdrawn))

	revoked := *withdrawn
	revoked.State = GrantStateRevoked

	diff, err := deployedDiffer(bucketDocument, keyDocument).
		Diff(context.Background(), targetFixture(kept, &revoked))
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}

	// Both documents carry the withdrawn researcher's statements, and both have to say so.
	for _, document := range []ApplyTarget{ApplyTargetBucket, ApplyTargetKey} {
		var deployed []string
		for _, difference := range diff.Differences {
			if difference.Document != document || difference.Aspect != AspectGrantedAccount {
				continue
			}
			if difference.Expected != "" {
				t.Errorf("the %s reports the remaining grant as missing: %s", document, difference)
			}
			deployed = append(deployed, difference.Deployed)
		}
		if !slices.Contains(deployed, testSecondResearcher) {
			t.Errorf("expected the %s to report account %s as still granted:\n%s",
				document, testSecondResearcher, diff.Report())
		}
		if slices.Contains(deployed, testResearcherAccount) {
			t.Errorf("the %s reports the grant the ledger still describes as drift:\n%s",
				document, diff.Report())
		}
	}
}

// TestDiffLooksPastHowADocumentIsWritten covers the reason the comparison is not
// textual: a document read back from AWS, or edited in a console, says the same thing
// differently. A principal written as a bare string rather than under an AWS key, a
// single-element list collapsed to a string, statements in another order, and an expiry
// carrying an explicit zero offset all describe exactly the access the ledger does.
func TestDiffLooksPastHowADocumentIsWritten(t *testing.T) {
	target := targetFixture(grantedFixture(&diffExpiry))

	bucketDocument := `{
      "Version": "2012-10-17",
      "Statement": [
        {
          "Sid": "DenyInsecureTransport",
          "Effect": "Deny",
          "Principal": "*",
          "Action": "s3:*",
          "Resource": ["arn:aws:s3:::hot-cache-cohort-a", "arn:aws:s3:::hot-cache-cohort-a/*"],
          "Condition": {"Bool": {"aws:SecureTransport": "false"}}
        },
        {
          "Sid": "SomethingSomeoneRenamed",
          "Effect": "Allow",
          "Principal": "arn:aws:iam::123456789012:root",
          "Action": ["s3:ListBucket", "s3:GetObject"],
          "Resource": ["arn:aws:s3:::hot-cache-cohort-a/*", "arn:aws:s3:::hot-cache-cohort-a"],
          "Condition": {
            "DateLessThan": {"aws:CurrentTime": "2026-10-01T06:30:00+00:00"},
            "StringEquals": {"aws:SourceVpc": ["vpc-0a1b2c3d4e5f60718"]}
          }
        },
        {
          "Sid": "DenyResearcherControlArtefacts",
          "Effect": "Deny",
          "Principal": {"AWS": "123456789012"},
          "Action": "s3:*",
          "Resource": "arn:aws:s3:::hot-cache-cohort-a/_control/*"
        }
      ]
    }`

	keyDocument := `{
      "Version": "2012-10-17",
      "Statement": [
        {
          "Sid": "AllowResearcherDecrypt",
          "Effect": "Allow",
          "Principal": {"AWS": ["arn:aws:iam::123456789012:root"]},
          "Action": ["kms:DescribeKey", "kms:Decrypt"],
          "Resource": "*",
          "Condition": {
            "StringEquals": {"kms:ViaService": "s3.ap-southeast-2.amazonaws.com"},
            "DateLessThan": {"aws:CurrentTime": "2026-10-01T06:30:00Z"}
          }
        },
        {
          "Sid": "AllowCopyWriterToEncrypt",
          "Effect": "Allow",
          "Principal": {"AWS": "arn:aws:iam::916218008751:role/copy-writer"},
          "Action": ["kms:GenerateDataKey", "kms:Decrypt"],
          "Resource": "*"
        },
        {
          "Sid": "AllowCustodianToWithdraw",
          "Effect": "Allow",
          "Principal": {"AWS": "arn:aws:iam::916218008751:role/dataset-custodian"},
          "Action": ["kms:DisableKey", "kms:ScheduleKeyDeletion"],
          "Resource": "*"
        },
        {
          "Sid": "AllowAccountKeyAdministration",
          "Effect": "Allow",
          "Principal": "916218008751",
          "Action": "kms:*",
          "Resource": "*"
        }
      ]
    }`

	diff, err := deployedDiffer(bucketDocument, keyDocument).Diff(context.Background(), target)
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}
	if !diff.Matches() {
		t.Fatalf("expected a respelled document to describe the same access, got:\n%s", diff.Report())
	}
}

// TestDiffReportsDriftInEveryAspect edits one fact of the deployed bucket policy at a
// time, which is what a policy changed in the console looks like.
func TestDiffReportsDriftInEveryAspect(t *testing.T) {
	movedExpiry := diffExpiry.AddDate(1, 0, 0)

	tests := []struct {
		name         string
		edit         func(doc *policy.Policy, st *policy.Statement)
		wantAspect   DiffAspect
		wantExpected string
		wantDeployed string
	}{
		{
			name: "granted to another account",
			edit: func(_ *policy.Policy, st *policy.Statement) {
				st.Principal = policy.NewAWSPrincipal("arn:aws:iam::" + diffStrangerAccount + ":root")
			},
			wantAspect:   AspectGrantedAccount,
			wantExpected: testResearcherAccount,
			wantDeployed: diffStrangerAccount,
		},
		{
			name: "permitted from another VPC",
			edit: func(_ *policy.Policy, st *policy.Statement) {
				st.Condition["StringEquals"]["aws:SourceVpc"] =
					policy.NewConditionValueString(true, "vpc-0999888777666555")
			},
			wantAspect:   AspectPermittedVpc,
			wantExpected: testVpcID,
			wantDeployed: "vpc-0999888777666555",
		},
		{
			name: "VPC condition removed altogether",
			edit: func(_ *policy.Policy, st *policy.Statement) {
				delete(st.Condition, "StringEquals")
			},
			wantAspect:   AspectPermittedVpc,
			wantExpected: testVpcID,
			wantDeployed: anyVpc,
		},
		{
			name: "expiry moved",
			edit: func(_ *policy.Policy, st *policy.Statement) {
				st.Condition["DateLessThan"]["aws:CurrentTime"] =
					policy.NewConditionValueString(true, movedExpiry.Format(time.RFC3339))
			},
			wantAspect:   AspectExpiry,
			wantExpected: diffExpiry.Format(time.RFC3339),
			wantDeployed: movedExpiry.Format(time.RFC3339),
		},
		{
			name: "expiry removed, so access no longer lapses",
			edit: func(_ *policy.Policy, st *policy.Statement) {
				delete(st.Condition, "DateLessThan")
			},
			wantAspect:   AspectExpiry,
			wantExpected: diffExpiry.Format(time.RFC3339),
			wantDeployed: noExpiry,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grant := grantedFixture(&diffExpiry)
			target := targetFixture(grant)
			bucketDocument, keyDocument := renderedDocuments(t, target)
			drifted := editedDocument(t, bucketDocument, statementIDsFor(grant).read, tt.edit)

			diff, err := deployedDiffer(drifted, keyDocument).Diff(context.Background(), target)
			if err != nil {
				t.Fatalf("diffing: %v", err)
			}

			difference := onlyDifference(t, diff)
			if difference.Document != ApplyTargetBucket {
				t.Errorf("difference reported against %s, want %s", difference.Document, ApplyTargetBucket)
			}
			if difference.Aspect != tt.wantAspect {
				t.Errorf("difference aspect = %s, want %s", difference.Aspect, tt.wantAspect)
			}
			if difference.Expected != tt.wantExpected {
				t.Errorf("difference expected %q, want %q", difference.Expected, tt.wantExpected)
			}
			if difference.Deployed != tt.wantDeployed {
				t.Errorf("difference deployed %q, want %q", difference.Deployed, tt.wantDeployed)
			}
		})
	}
}

// TestDiffReportsAGrantNobodyRecorded covers a read grant added by hand under a
// statement identifier of its own, which is the shape an out-of-band grant actually
// takes.
func TestDiffReportsAGrantNobodyRecorded(t *testing.T) {
	grant := grantedFixture(&diffExpiry)
	target := targetFixture(grant)
	bucketDocument, keyDocument := renderedDocuments(t, target)

	drifted := editedDocument(t, bucketDocument, statementIDsFor(grant).read, func(doc *policy.Policy, _ *policy.Statement) {
		doc.Statements.Add(policy.Statement{
			Sid:       "AllowAFavour",
			Effect:    policy.EffectAllow,
			Principal: policy.NewAWSPrincipal("arn:aws:iam::" + diffStrangerAccount + ":root"),
			Action:    policy.NewStringOrSlice(true, "s3:GetObject"),
			Resource:  policy.NewStringOrSlice(true, "arn:aws:s3:::"+testBucket+"/*"),
		})
	})

	diff, err := deployedDiffer(drifted, keyDocument).Diff(context.Background(), target)
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}

	var accounts, vpcs []string
	for _, difference := range diff.Differences {
		switch difference.Aspect {
		case AspectGrantedAccount:
			accounts = append(accounts, difference.Deployed)
		case AspectPermittedVpc:
			vpcs = append(vpcs, difference.Deployed)
		}
	}
	if len(accounts) != 1 || accounts[0] != diffStrangerAccount {
		t.Errorf("unexpected granted accounts reported = %v, want [%s]\n%s",
			accounts, diffStrangerAccount, diff.Report())
	}
	// The added statement is conditioned on nothing, so it also admits requests from
	// anywhere -- which is worth reporting alongside the account.
	if len(vpcs) != 1 || vpcs[0] != anyVpc {
		t.Errorf("unexpected permitted VPCs reported = %v, want [%s]\n%s", vpcs, anyVpc, diff.Report())
	}
	if !strings.Contains(diff.Report(), "AllowAFavour") {
		t.Errorf("expected the report to point at the statement the grant was added to:\n%s", diff.Report())
	}
}

// TestDiffReportsAGrantThatOutlivedItsRevocation is the case reconciliation exists for:
// the ledger says access was withdrawn and the deployed documents still admit the
// researcher.
func TestDiffReportsAGrantThatOutlivedItsRevocation(t *testing.T) {
	granted := grantedFixture(&diffExpiry)
	bucketDocument, keyDocument := renderedDocuments(t, targetFixture(granted))

	revoked := *granted
	revoked.State = GrantStateRevoked

	diff, err := deployedDiffer(bucketDocument, keyDocument).
		Diff(context.Background(), targetFixture(&revoked))
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}
	if diff.Matches() {
		t.Fatal("expected a withdrawn grant still deployed in policy to differ from the ledger")
	}

	for _, document := range []ApplyTarget{ApplyTargetBucket, ApplyTargetKey} {
		found := false
		for _, difference := range diff.Differences {
			if difference.Document != document || difference.Aspect != AspectGrantedAccount {
				continue
			}
			if difference.Deployed == testResearcherAccount && difference.Expected == "" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected the %s to report account %s as granted where the ledger describes no grant:\n%s",
				document, testResearcherAccount, diff.Report())
		}
	}
}

func TestDiffReportsAnAbsentBucketPolicy(t *testing.T) {
	target := targetFixture(grantedFixture(nil))
	_, keyDocument := renderedDocuments(t, target)

	diff, err := deployedDiffer("", keyDocument).Diff(context.Background(), target)
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}
	if len(diff.Absent) != 1 || diff.Absent[0] != ApplyTargetBucket {
		t.Fatalf("absent documents = %v, want [%s]", diff.Absent, ApplyTargetBucket)
	}
	if diff.Matches() {
		t.Fatal("expected an absent bucket policy not to match a copy that grants access")
	}

	// The grant the ledger describes is missing rather than changed, so it is reported
	// with nothing deployed against it.
	found := false
	for _, difference := range diff.Differences {
		if difference.Document == ApplyTargetBucket &&
			difference.Aspect == AspectGrantedAccount &&
			difference.Expected == testResearcherAccount &&
			difference.Deployed == "" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the described grant to be reported as not deployed:\n%s", diff.Report())
	}
}

func TestDiffRefusesToGuessAtADocumentItCannotRead(t *testing.T) {
	target := targetFixture(grantedFixture(&diffExpiry))
	_, keyDocument := renderedDocuments(t, target)

	_, err := deployedDiffer(`{"Statement": [`, keyDocument).Diff(context.Background(), target)
	if err == nil {
		t.Fatal("expected a malformed deployed policy to be an error rather than a difference")
	}
	if !strings.Contains(err.Error(), testBucket) {
		t.Errorf("expected the error to name the bucket, got: %v", err)
	}
}

func TestDiffReportsWhoOwnsAnUnreadablePolicy(t *testing.T) {
	target := targetFixture(grantedFixture(&diffExpiry))
	objects := &awstest.S3{
		GetBucketPolicyFn: func(ctx context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error) {
			return nil, errors.New("AccessDenied")
		},
	}

	_, err := NewPolicyDiffer(testSettings(), objects, &awstest.KMS{}).
		Diff(context.Background(), target)
	if err == nil {
		t.Fatal("expected a policy that could not be read to fail the comparison")
	}

	var missing *MissingPrerequisiteError
	if !errors.As(err, &missing) {
		t.Fatalf("expected the failure to name the resource and its owner, got: %v", err)
	}
	if missing.Prerequisite.Identifier != testBucket {
		t.Errorf("prerequisite identifier = %q, want %q", missing.Prerequisite.Identifier, testBucket)
	}
}
