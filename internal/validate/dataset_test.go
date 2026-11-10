package validate

// These tests cover the pass that compares the access a dataset's ledger records describe
// against the access actually deployed for it, together with the prerequisites a copy
// depends on.
//
// Three things shape them. The comparison exists to notice a policy someone edited outside
// this tool, so the inputs worth constructing are deployed documents that differ from the
// applied pair by exactly one fact -- who may read, where they may read from, and until
// when. The pass is meant to report everything one run found, so a failing prerequisite
// must not take the later checks with it; that is asserted by holding a broken run's
// results against a healthy run's rather than against a hand-counted number, which would go
// stale the moment a check is added. And a dataset's two documents are shared by every copy
// of it and by every grant made against those copies, so the cases worth the most here are
// the ones carrying more than one of either: a comparison made from any smaller unit
// reports the rest of the ledger as drift, and reconciliation then writes it away.
//
// The ledger behind them is the shared-table double rather than a stub store, and the
// records go in through the stores, so the checks read what a real write would have left.
// The deployed documents are rendered from those same records rather than written by hand,
// so a test cannot pass by comparing a fixture against itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/micahhausler/aws-iam-policy/policy"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/dataset"
)

// The one deployment every test here runs against. The values are the shapes the checks
// actually read rather than arbitrary strings: the key ARN carries the partition the
// rendered documents are built from, the VPCs are the bare identifiers a bucket policy
// compares a request against, and the execution names the state machine configured below,
// which is what makes it evidence that the orchestration answers for it.
const (
	datasetOwnerAccount      = "916218008751"
	datasetResearcherAccount = "123456789012"
	datasetSecondResearcher  = "210987654321"
	datasetStrangerAccount   = "999999999999"
	datasetRegion            = "ap-southeast-2"
	datasetLedgerTable       = "data-releaser-ledger"
	datasetTargetVpc         = "vpc-0a1b2c3d4e5f60718"
	datasetSecondResearchvpc = "vpc-0b2c3d4e5f60718a9"
	datasetStrangerTargetVpc = "vpc-0999888777666555"

	datasetCopyStateMachine = "arn:aws:states:ap-southeast-2:916218008751:stateMachine:dataset-copy"
	datasetCopyExecution    = "arn:aws:states:ap-southeast-2:916218008751:execution:dataset-copy:01J8ZQ4N8Z4T7VQ2K3M5X6Y7A0"
	datasetWorkingBucket    = "copy-service-working"
	datasetWorkingPrefix    = "instructions"

	datasetSourceEndpoint = "https://ceph.example.org"
	datasetSourceSecret   = "arn:aws:secretsmanager:ap-southeast-2:916218008751:secret:ceph-source-Ab12Cd" // pragma: allowlist secret
)

// datasetNow pins the clock the pass judges expiry and scheduled key deletion against, so
// no result depends on when the tests are run. It sits before datasetExpiry, which is what
// makes a granted fixture one that currently gives access -- the state in which a key that
// cannot decrypt is a finding rather than a note.
var (
	datasetNow    = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	datasetExpiry = time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
)

// Names of the results these tests look for. The per-dataset checks name their dataset as
// well, so only the fixed part is spelled out.
const (
	// keyStateCheckPrefix labels the state of the key encrypting one dataset.
	keyStateCheckPrefix = "Dataset key state"

	// repairCheckPrefix labels a dataset whose documents were replaced with the pair the
	// ledger describes. Asserting on it is what keeps a write distinguishable from an
	// observation in the output.
	repairCheckPrefix = "Ledger policies re-applied"
)

// cohortACopy is the transfer nearly every test works with: one dataset staged in a bucket
// of its own, under a key of its own. It carries the execution that performed it, since a
// copy this deployment already started is what the orchestration check asks about in
// preference to a probe.
func cohortACopy() *dataset.Copy {
	return &dataset.Copy{
		CopyID:           "01J8ZQ4N8Z4T7VQ2K3M5X6Y7A0",
		DatasetURI:       "s3://ceph-source/datasets/cohort-a/",
		HotCacheBucket:   "hot-cache-cohort-a",
		KeyArn:           "arn:aws:kms:ap-southeast-2:916218008751:key/1a2b3c4d-5e6f-7081-92a3-b4c5d6e7f809",
		State:            dataset.CopyStateStaged,
		CreatedAt:        time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		ObjectCount:      12,
		TotalBytes:       1 << 30,
		CopyExecutionArn: datasetCopyExecution,
	}
}

// cohortBCopy is a transfer of a different dataset, into a bucket of its own and under a
// key of its own. Reconciliation is meant to touch only what differs, and one dataset in
// the ledger cannot tell that apart from reconciliation touching everything.
func cohortBCopy() *dataset.Copy {
	return &dataset.Copy{
		CopyID:         "01J8ZQ4N8Z4T7VQ2K3M5X6Y7B1",
		DatasetURI:     "s3://ceph-source/datasets/cohort-b/",
		HotCacheBucket: "hot-cache-cohort-b",
		KeyArn:         "arn:aws:kms:ap-southeast-2:916218008751:key/2b3c4d5e-6f70-8192-a3b4-c5d6e7f80912",
		State:          dataset.CopyStateStaged,
		CreatedAt:      time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC),
		ObjectCount:    9,
		TotalBytes:     1 << 29,
	}
}

// failedCohortACopy is an earlier attempt at the same dataset: the same bucket and the same
// key, since both are deployed per dataset, and nothing granted against it because it never
// delivered anything to grant.
func failedCohortACopy() *dataset.Copy {
	c := cohortACopy()
	c.CopyID = "01J8ZQ4N8Z4T7VQ2K3M5X6Y7C1"
	c.State = dataset.CopyStateFailed
	c.CreatedAt = c.CreatedAt.Add(-24 * time.Hour)
	c.ObjectCount, c.TotalBytes = 3, 4096
	c.CopyExecutionArn = "arn:aws:states:ap-southeast-2:916218008751:execution:dataset-copy:01J8ZQ4N8Z4T7VQ2K3M5X6Y7C1"
	return c
}

// grantOf is the request one researcher's access is recorded from, expiring at the pinned
// expiry so that the grant is live as at datasetNow.
func grantOf(researcherAccountID, targetVpcID string) dataset.GrantRequest {
	expiry := datasetExpiry
	return dataset.GrantRequest{
		ResearcherAccountID: researcherAccountID,
		TargetVpcID:         targetVpcID,
		ExpiresAt:           &expiry,
	}
}

// datasetSettings are the settings the deployed documents in these tests were rendered
// with, and the same ones the pass compares with. The expected pair is rendered rather
// than described, so a comparison made with other settings would report a difference in
// everything and every assertion below would hold for the wrong reason.
func datasetSettings() dataset.PolicySettings {
	return dataset.PolicySettings{
		AccountID:          datasetOwnerAccount,
		Region:             datasetRegion,
		CopyWriterRoleName: "copy-writer",
		CustodianRoleARN:   "arn:aws:iam::916218008751:role/dataset-custodian",
	}
}

// absentBucketPolicy stands in for what S3 answers with when a bucket carries no policy at
// all. It is a coded error rather than a type of its own, and that code is the only thing
// separating a bucket that grants nothing from one whose policy could not be read.
type absentBucketPolicy struct{}

func (absentBucketPolicy) Error() string {
	return "api error NoSuchBucketPolicy: The bucket policy does not exist"
}

func (absentBucketPolicy) ErrorCode() string { return "NoSuchBucketPolicy" }

// datasetDeployment is one deployment under test: the options a pass is run with, the
// ledger behind them, and the doubles behind that, so a test can bend one seam and then
// assert on what another was asked to write.
type datasetDeployment struct {
	opts    ValidateDatasetOpts
	objects *awstest.S3
	keys    *awstest.KMS
	sfn     *awstest.StepFunctions

	copies *dataset.CopyStore
	grants *dataset.GrantStore

	// bucketPolicies and keyPolicies are the documents the doubles serve, addressed the
	// way the checks read them: a bucket policy by bucket name, a key policy by key ARN.
	// Replacing an entry stands in for a document edited in a console; an entry that is
	// absent stands in for a document that is not deployed at all.
	bucketPolicies map[string]string
	keyPolicies    map[string]string
}

// newDatasetDeployment returns a deployment with an empty ledger and nothing deployed. A
// test records the copies and grants it is about, then deploys exactly the documents those
// records describe, which is the state a pass should find nothing wrong in.
func newDatasetDeployment(t *testing.T) *datasetDeployment {
	t.Helper()

	table := awstest.NewLedger()
	deployment := &datasetDeployment{
		sfn:            &awstest.StepFunctions{},
		copies:         dataset.NewCopyStore(table, datasetLedgerTable),
		grants:         dataset.NewGrantStore(table, datasetLedgerTable),
		bucketPolicies: make(map[string]string),
		keyPolicies:    make(map[string]string),
	}

	deployment.objects = &awstest.S3{
		GetBucketPolicyFn: func(_ context.Context, params *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error) {
			document, deployed := deployment.bucketPolicies[aws.ToString(params.Bucket)]
			if !deployed {
				return nil, absentBucketPolicy{}
			}
			return &s3.GetBucketPolicyOutput{Policy: aws.String(document)}, nil
		},
	}
	deployment.keys = &awstest.KMS{
		// KMS holds a policy for every key it has, so an unknown key answers with an
		// empty document rather than an error, which the comparison reads as one that
		// grants nothing.
		GetKeyPolicyFn: func(_ context.Context, params *kms.GetKeyPolicyInput) (*kms.GetKeyPolicyOutput, error) {
			document := deployment.keyPolicies[aws.ToString(params.KeyId)]
			return &kms.GetKeyPolicyOutput{Policy: aws.String(document)}, nil
		},
	}

	deployment.opts = ValidateDatasetOpts{
		Copies:              deployment.copies,
		Grants:              deployment.grants,
		LedgerTable:         datasetLedgerTable,
		S3Client:            deployment.objects,
		KMSClient:           deployment.keys,
		StepFunctionsClient: deployment.sfn,
		PolicySettings:      datasetSettings(),
		Copy: DatasetCopyTargets{
			StateMachineARN:     datasetCopyStateMachine,
			WorkingBucket:       datasetWorkingBucket,
			WorkingBucketPrefix: datasetWorkingPrefix,
		},
		Source: DatasetSourceStore{
			EndpointURL:       datasetSourceEndpoint,
			CredentialsSecret: datasetSourceSecret,
		},
		Now: datasetNow,
	}
	return deployment
}

// record writes a copy into the ledger under the identifier and timestamp it carries.
func (d *datasetDeployment) record(t *testing.T, c *dataset.Copy) *dataset.Copy {
	t.Helper()
	if err := d.copies.Record(context.Background(), c); err != nil {
		t.Fatalf("recording copy %s: %v", c.CopyID, err)
	}
	return c
}

// grant records one researcher's access to a copy, and returns the grant with the
// identifier the ledger minted for it -- which is also what names its statements in the
// deployed documents.
func (d *datasetDeployment) grant(t *testing.T, c *dataset.Copy, request dataset.GrantRequest) *dataset.Grant {
	t.Helper()
	granted, err := d.grants.Create(context.Background(), c, request)
	if err != nil {
		t.Fatalf("granting %s access to copy %s: %v", request.ResearcherAccountID, c.CopyID, err)
	}
	return granted
}

// revoke withdraws a grant, which is what takes its statements out of the pair the ledger
// describes.
func (d *datasetDeployment) revoke(t *testing.T, granted *dataset.Grant) *dataset.Grant {
	t.Helper()
	revoked, err := d.grants.MarkRevoked(context.Background(), granted.GrantID)
	if err != nil {
		t.Fatalf("revoking grant %s: %v", granted.GrantID, err)
	}
	return revoked
}

// deploy installs, for every dataset in the ledger, exactly the pair of documents its
// records describe -- one pair per bucket, carrying the statements of every grant made
// against every copy in it, which is how applying puts them there.
func (d *datasetDeployment) deploy(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	copies, err := d.copies.ListAll(ctx)
	if err != nil {
		t.Fatalf("reading the recorded copies to deploy from: %v", err)
	}
	grants, err := d.grants.ListAll(ctx)
	if err != nil {
		t.Fatalf("reading the recorded grants to deploy from: %v", err)
	}

	targets := make(map[string]dataset.PolicyTarget, len(copies))
	for _, c := range copies {
		target := targets[c.HotCacheBucket]
		target.DatasetURI, target.HotCacheBucket, target.KeyArn = c.DatasetURI, c.HotCacheBucket, c.KeyArn
		targets[c.HotCacheBucket] = target
	}
	for _, granted := range grants {
		target, copied := targets[granted.HotCacheBucket]
		if !copied {
			continue
		}
		target.Grants = append(target.Grants, granted)
		targets[granted.HotCacheBucket] = target
	}

	for bucket, target := range targets {
		policies, err := dataset.RenderPolicies(target, datasetSettings())
		if err != nil {
			t.Fatalf("rendering the policies of dataset %s: %v", target.DatasetURI, err)
		}
		bucketDocument, err := policies.BucketPolicyJSON()
		if err != nil {
			t.Fatalf("rendering the bucket policy of dataset %s: %v", target.DatasetURI, err)
		}
		keyDocument, err := policies.KeyPolicyJSON()
		if err != nil {
			t.Fatalf("rendering the key policy of dataset %s: %v", target.DatasetURI, err)
		}
		d.bucketPolicies[bucket] = bucketDocument
		d.keyPolicies[target.KeyArn] = keyDocument
	}
}

// validate runs one pass over the deployment as it currently stands.
func (d *datasetDeployment) validate() DatasetValidation {
	return ValidateDataset(context.Background(), d.opts)
}

// drift edits one grant's statement in its dataset's deployed bucket policy, leaving the
// rest of the document as it was applied. That is the shape a console edit takes, and it is
// what the comparison has to notice.
func (d *datasetDeployment) drift(t *testing.T, granted *dataset.Grant, edit func(st *policy.Statement)) {
	t.Helper()
	d.bucketPolicies[granted.HotCacheBucket] = editedGrant(t, d.bucketPolicies[granted.HotCacheBucket], granted, edit)
}

// grantedDataset is the deployment most tests start from: one staged copy of cohort A,
// granted to one researcher, with exactly the documents those two records describe
// deployed.
func grantedDataset(t *testing.T) (*datasetDeployment, *dataset.Copy, *dataset.Grant) {
	t.Helper()

	deployment := newDatasetDeployment(t)
	copied := deployment.record(t, cohortACopy())
	granted := deployment.grant(t, copied, grantOf(datasetResearcherAccount, datasetTargetVpc))
	deployment.deploy(t)
	return deployment, copied, granted
}

// editedGrant applies edit to one grant's granting statement in a document and returns the
// result.
//
// The statement is found by the grant's identifier because that is what names it: several
// grants sit in one document, so a fixed name per statement would match all of them or
// none. The deny that withholds the copy's own artefacts carries the same identifier and is
// left alone; what is edited is the statement that gives the access.
func editedGrant(t *testing.T, document string, granted *dataset.Grant, edit func(st *policy.Statement)) string {
	t.Helper()

	var parsed policy.Policy
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("reading the deployed document to edit: %v", err)
	}

	// The slice belongs to the document, so editing an element in place edits the
	// document itself.
	statements := parsed.Statements.Values()
	edited := false
	for i := range statements {
		sid := statements[i].Sid
		if !strings.HasSuffix(sid, granted.GrantID) || strings.HasPrefix(sid, "Deny") {
			continue
		}
		edit(&statements[i])
		edited = true
	}
	if !edited {
		t.Fatalf("the deployed document carries no granting statement for grant %s to edit", granted.GrantID)
	}

	rewritten, err := json.Marshal(&parsed)
	if err != nil {
		t.Fatalf("writing the edited document: %v", err)
	}
	return string(rewritten)
}

// deployedAccessOf names the comparison check of one dataset, so a test holding more than
// one dataset asserts on the one it means.
func deployedAccessOf(c *dataset.Copy) string {
	return fmt.Sprintf("%s: %s", deployedAccessCheck, c.DatasetURI)
}

// repairOf names the re-application of one dataset's documents.
func repairOf(c *dataset.Copy) string {
	return fmt.Sprintf("%s: %s", repairCheckPrefix, c.DatasetURI)
}

// checkNames lists results by name, for holding what one pass produced against another.
func checkNames(results []Check) []string {
	names := make([]string, 0, len(results))
	for _, result := range results {
		names = append(names, result.Name)
	}
	return names
}

// TestValidateDatasetReportsDeployedDrift edits one fact of a deployed bucket policy at a
// time -- who may read, where from, and until when -- which is what a policy changed
// outside this tool looks like.
//
// Each edit has to be reported against the dataset it belongs to and has to name what is
// deployed, since the operator's next move is to decide whether the record or the document
// is the wrong one, and they cannot do that from being told only that the two disagree.
func TestValidateDatasetReportsDeployedDrift(t *testing.T) {
	movedExpiry := datasetExpiry.AddDate(1, 0, 0)

	tests := []struct {
		name         string
		edit         func(st *policy.Statement)
		wantAspect   dataset.DiffAspect
		wantDeployed string
	}{
		{
			name: "granted to an account no record mentions",
			edit: func(st *policy.Statement) {
				st.Principal = policy.NewAWSPrincipal("arn:aws:iam::" + datasetStrangerAccount + ":root")
			},
			wantAspect:   dataset.AspectGrantedAccount,
			wantDeployed: datasetStrangerAccount,
		},
		{
			name: "reads permitted from another VPC",
			edit: func(st *policy.Statement) {
				st.Condition["StringEquals"]["aws:SourceVpc"] =
					policy.NewConditionValueString(true, datasetStrangerTargetVpc)
			},
			wantAspect:   dataset.AspectPermittedVpc,
			wantDeployed: datasetStrangerTargetVpc,
		},
		{
			name: "expiry moved a year out",
			edit: func(st *policy.Statement) {
				st.Condition["DateLessThan"]["aws:CurrentTime"] =
					policy.NewConditionValueString(true, movedExpiry.Format(time.RFC3339))
			},
			wantAspect:   dataset.AspectExpiry,
			wantDeployed: movedExpiry.Format(time.RFC3339),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment, copied, granted := grantedDataset(t)
			deployment.drift(t, granted, tt.edit)

			check := findCheck(t, deployment.validate().Results(), deployedAccessOf(copied))
			if check.Passed {
				t.Fatal("expected an edited bucket policy to be reported as differing from the ledger")
			}
			for _, want := range []string{
				string(dataset.ApplyTargetBucket),
				tt.wantAspect.String(),
				tt.wantDeployed,
			} {
				if !strings.Contains(check.Error, want) {
					t.Errorf("expected the report to name %q, got: %s", want, check.Error)
				}
			}
		})
	}
}

// TestValidateDatasetPassesWhenTheDeployedPoliciesAreTheRecordedOnes is the contrast the
// drift cases need. The same pass over a deployment nobody has edited reports nothing at
// all, so a reported difference means a real one rather than a comparison that finds
// something in every document it is handed.
func TestValidateDatasetPassesWhenTheDeployedPoliciesAreTheRecordedOnes(t *testing.T) {
	deployment, copied, _ := grantedDataset(t)
	validation := deployment.validate()

	check := findCheck(t, validation.Results(), deployedAccessOf(copied))
	if !check.Passed || check.Skipped {
		t.Errorf("expected the deployed access to be compared and to match, got %+v", check)
	}
	for _, result := range validation.Results() {
		if !result.Passed {
			t.Errorf("expected check %q to pass on an untouched deployment, got: %s", result.Name, result.Error)
		}
	}
	if len(validation.Repairs) != 0 {
		t.Errorf("expected nothing to be written without reconciliation, got %v", checkNames(validation.Repairs))
	}
}

// TestValidateDatasetComparesOncePerDatasetNotPerCopy covers a dataset copied more than
// once before one of those copies was granted, which is what any dataset staged again after
// a failure looks like.
//
// The bucket and the key are deployed per dataset and carry one pair of documents between
// them, so every copy of that dataset shares that pair and only the grants describe it.
// Comparing a copy at a time reported the live grant as undescribed drift against every copy
// carrying none -- and reconciliation then "repaired" it by writing the baseline over a
// grant that was doing exactly what the ledger asked, taking a researcher's access away to
// fix nothing.
func TestValidateDatasetComparesOncePerDatasetNotPerCopy(t *testing.T) {
	deployment := newDatasetDeployment(t)
	deployment.record(t, failedCohortACopy())
	staged := deployment.record(t, cohortACopy())
	deployment.grant(t, staged, grantOf(datasetResearcherAccount, datasetTargetVpc))
	deployment.deploy(t)

	validation := deployment.validate()

	// One comparison for the dataset, whatever the number of copies of it.
	var compared []string
	for _, result := range validation.Results() {
		if strings.Contains(result.Name, deployedAccessCheck) {
			compared = append(compared, result.Name)
		}
	}
	if len(compared) != 1 {
		t.Fatalf("expected one comparison for the dataset, got %d: %v", len(compared), compared)
	}
	if !strings.Contains(compared[0], staged.DatasetURI) {
		t.Errorf("the comparison should name the dataset the documents belong to, got %q", compared[0])
	}

	for _, result := range validation.Results() {
		if !result.Passed {
			t.Errorf("a correctly deployed grant was reported as a failure: %s\n%s",
				result.Name, result.Error)
		}
	}
}

// TestValidateDatasetReconcilesADatasetWithoutUndoingItsGrant is the consequence of the
// above, and the reason it is worth its own test: a repair that ran per copy would write a
// copy-with-no-grant's baseline over the live grant its sibling holds.
func TestValidateDatasetReconcilesADatasetWithoutUndoingItsGrant(t *testing.T) {
	deployment := newDatasetDeployment(t)
	deployment.record(t, failedCohortACopy())
	staged := deployment.record(t, cohortACopy())
	deployment.grant(t, staged, grantOf(datasetResearcherAccount, datasetTargetVpc))
	deployment.deploy(t)
	deployment.opts.Reconcile = true

	validation := deployment.validate()

	// Nothing differs, so nothing is written. The danger is a repair that fires because a
	// copy carrying no grant disagrees with a grant it was never meant to describe.
	if len(validation.Repairs) != 0 {
		t.Errorf("a correctly deployed dataset was repaired: %v", checkNames(validation.Repairs))
	}
	if len(deployment.objects.PutBucketPolicyCalls) != 0 || len(deployment.keys.PutKeyPolicyCalls) != 0 {
		t.Errorf("reconciliation wrote to a dataset that matched the ledger: %d bucket, %d key",
			len(deployment.objects.PutBucketPolicyCalls), len(deployment.keys.PutKeyPolicyCalls))
	}
}

// TestValidateDatasetComparesEveryGrantOnACopy covers one copy released to two researchers,
// which is the other way a dataset's documents come to hold more than one record's worth of
// statements.
//
// Both grants' statements sit in the same pair, so the comparison has to be made against
// both at once: made against either alone it would report the other researcher's access as
// drift nobody described. And when one of them really has been edited, the report has to be
// about that one -- an operator told that two accounts disagree with the ledger will go and
// look at two.
func TestValidateDatasetComparesEveryGrantOnACopy(t *testing.T) {
	deploymentWithTwoGrants := func(t *testing.T) (*datasetDeployment, *dataset.Copy, *dataset.Grant, *dataset.Grant) {
		t.Helper()

		deployment := newDatasetDeployment(t)
		copied := deployment.record(t, cohortACopy())
		first := deployment.grant(t, copied, grantOf(datasetResearcherAccount, datasetTargetVpc))
		second := deployment.grant(t, copied, grantOf(datasetSecondResearcher, datasetSecondResearchvpc))
		deployment.deploy(t)
		return deployment, copied, first, second
	}

	t.Run("both grants deployed", func(t *testing.T) {
		deployment, copied, _, _ := deploymentWithTwoGrants(t)

		check := findCheck(t, deployment.validate().Results(), deployedAccessOf(copied))
		if !check.Passed || check.Skipped {
			t.Errorf("expected a copy carrying two grants to compare clean against its own rendered "+
				"documents, got %+v", check)
		}
	})

	t.Run("one grant edited leaves the other out of it", func(t *testing.T) {
		deployment, copied, first, second := deploymentWithTwoGrants(t)
		deployment.drift(t, first, func(st *policy.Statement) {
			st.Principal = policy.NewAWSPrincipal("arn:aws:iam::" + datasetStrangerAccount + ":root")
		})

		check := findCheck(t, deployment.validate().Results(), deployedAccessOf(copied))
		if check.Passed {
			t.Fatal("expected the edited grant to be reported as differing from the ledger")
		}
		for _, want := range []string{datasetStrangerAccount, first.ResearcherAccountID} {
			if !strings.Contains(check.Error, want) {
				t.Errorf("expected the report to name %q, got: %s", want, check.Error)
			}
		}
		for _, unwanted := range []string{second.ResearcherAccountID, second.TargetVpcID} {
			if strings.Contains(check.Error, unwanted) {
				t.Errorf("the untouched grant's %q was reported as a difference: %s", unwanted, check.Error)
			}
		}
	})
}

// TestValidateDatasetNarrowedToOneResearcher covers a pass asked about one account.
//
// The narrowing has to be by dataset rather than by grant, for the same reason the
// comparison is: a dataset's two documents carry every grant against it, so a pair rendered
// from one account's grants alone is missing the other account's statements. Comparing
// against that would report every one of them as drift, and reconciliation would then write
// the pair and withdraw access from an account nobody asked about.
func TestValidateDatasetNarrowedToOneResearcher(t *testing.T) {
	deployment := newDatasetDeployment(t)
	shared := deployment.record(t, cohortACopy())
	elsewhere := deployment.record(t, cohortBCopy())
	deployment.grant(t, shared, grantOf(datasetResearcherAccount, datasetTargetVpc))
	deployment.grant(t, shared, grantOf(datasetSecondResearcher, datasetSecondResearchvpc))
	deployment.grant(t, elsewhere, grantOf(datasetSecondResearcher, datasetSecondResearchvpc))
	deployment.deploy(t)

	deployment.opts.ResearcherAccountID = datasetResearcherAccount
	deployment.opts.Reconcile = true

	validation := deployment.validate()

	check := findCheck(t, validation.Results(), deployedAccessOf(shared))
	if !check.Passed {
		t.Errorf("expected the dataset the account holds a grant against to be compared against "+
			"every grant on it, got: %s", check.Error)
	}
	for _, result := range validation.Results() {
		if strings.Contains(result.Name, elsewhere.DatasetURI) {
			t.Errorf("expected a dataset the account holds no grant against to be left out, got %q",
				result.Name)
		}
	}
	if len(validation.Repairs) != 0 || len(deployment.objects.PutBucketPolicyCalls) != 0 ||
		len(deployment.keys.PutKeyPolicyCalls) != 0 {
		t.Errorf("a narrowed pass wrote to a correctly deployed dataset: %v, %d bucket, %d key",
			checkNames(validation.Repairs), len(deployment.objects.PutBucketPolicyCalls),
			len(deployment.keys.PutKeyPolicyCalls))
	}
}

// TestValidateDatasetRunsEveryCheckAfterAFailure covers the reason nothing here stops at
// the first failure: a deployment with three problems should take one run to discover, not
// three. A broken run has to produce the same results as a healthy one, with the broken
// check failing and every other one still reporting what it found.
func TestValidateDatasetRunsEveryCheckAfterAFailure(t *testing.T) {
	healthyDeployment, _, _ := grantedDataset(t)
	healthy := checkNames(healthyDeployment.validate().Results())

	tests := []struct {
		name       string
		breakIt    func(d *datasetDeployment)
		wantFailed string
	}{
		{
			name: "the copy orchestration will not answer",
			breakIt: func(d *datasetDeployment) {
				d.sfn.DescribeExecutionFn = func(context.Context, *sfn.DescribeExecutionInput) (*sfn.DescribeExecutionOutput, error) {
					return nil, errors.New("AccessDeniedException")
				}
			},
			wantFailed: copyOrchestrationCheck,
		},
		{
			name: "the copy working bucket refuses the write",
			breakIt: func(d *datasetDeployment) {
				d.objects.PutObjectFn = func(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
					return nil, errors.New("AccessDenied")
				}
			},
			wantFailed: workingBucketCheck,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment, _, _ := grantedDataset(t)
			tt.breakIt(deployment)

			results := deployment.validate().Results()
			if got := checkNames(results); !slices.Equal(got, healthy) {
				t.Fatalf("a failing check changed which checks ran:\n got %v\nwant %v", got, healthy)
			}

			for _, result := range results {
				broken := strings.Contains(result.Name, tt.wantFailed)
				switch {
				case broken && result.Passed:
					t.Errorf("expected %q to fail, got a pass", result.Name)
				case !broken && !result.Passed:
					t.Errorf("expected %q to run and report despite the earlier failure, got: %s",
						result.Name, result.Error)
				}
			}
		})
	}
}

// TestValidateDatasetReportsTheDatasetKeyState covers what a key that cannot decrypt does
// to a grant. Bucket keys are off on these buckets, so the key is in the path of every
// read: a disabled or half-deleted key stops reads as surely as a withdrawn grant does, and
// the researcher sees a denial they cannot tell from a wrong VPC or a passed expiry.
//
// So the state is reported either way, and it is a failure only while a grant on a copy
// under that key is still live. A key nothing is granted through is worth saying out loud
// and is nobody's incident.
func TestValidateDatasetReportsTheDatasetKeyState(t *testing.T) {
	deletionDate := datasetNow.AddDate(0, 0, 30)

	tests := []struct {
		name         string
		state        kmstypes.KeyState
		deletionDate *time.Time
		revoked      bool
		wantPassed   bool
		wantText     []string
	}{
		{
			name:       "an enabled key serving a granted dataset",
			state:      kmstypes.KeyStateEnabled,
			wantPassed: true,
		},
		{
			name:     "a disabled key while a grant on it is still live",
			state:    kmstypes.KeyStateDisabled,
			wantText: []string{string(kmstypes.KeyStateDisabled), datasetResearcherAccount},
		},
		{
			name:         "a key being destroyed while a grant on it is still live",
			state:        kmstypes.KeyStatePendingDeletion,
			deletionDate: &deletionDate,
			wantText: []string{
				string(kmstypes.KeyStatePendingDeletion),
				deletionDate.Format(time.RFC3339),
			},
		},
		{
			name:       "a disabled key nothing is granted through",
			state:      kmstypes.KeyStateDisabled,
			revoked:    true,
			wantPassed: true,
			wantText:   []string{string(kmstypes.KeyStateDisabled), "no access is affected"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment := newDatasetDeployment(t)
			copied := deployment.record(t, cohortACopy())
			granted := deployment.grant(t, copied, grantOf(datasetResearcherAccount, datasetTargetVpc))
			if tt.revoked {
				granted = deployment.revoke(t, granted)
			}
			deployment.deploy(t)

			deployment.keys.DescribeKeyFn = func(_ context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
				return &kms.DescribeKeyOutput{KeyMetadata: &kmstypes.KeyMetadata{
					KeyId:        params.KeyId,
					Enabled:      tt.state == kmstypes.KeyStateEnabled,
					KeyState:     tt.state,
					DeletionDate: tt.deletionDate,
				}}, nil
			}

			check := findCheck(t, deployment.validate().Results(), keyStateCheckPrefix)
			if check.Passed != tt.wantPassed {
				t.Errorf("key state check passed = %v, want %v: %s", check.Passed, tt.wantPassed, check.Error)
			}
			want := tt.wantText
			if !tt.wantPassed {
				// A finding names the grant it is about, since revoking it is one of the
				// two ways out of the state being reported.
				want = append(want, granted.GrantID)
			}
			for _, text := range want {
				if !strings.Contains(check.Error, text) {
					t.Errorf("expected the key state report to name %q, got: %s", text, check.Error)
				}
			}
		})
	}
}

// TestValidateDatasetSkipsADestroyedDataset covers a purge that has run its course. The
// documents may still be deployed, but the key is gone and the objects under it are
// ciphertext nobody can read, so a comparison would fail every run from here on and teach
// an operator to stop reading the output.
func TestValidateDatasetSkipsADestroyedDataset(t *testing.T) {
	purgedAt := datasetNow.AddDate(0, 0, -31)
	deletionDate := datasetNow.AddDate(0, 0, -1)

	destroyed := cohortACopy()
	destroyed.State = dataset.CopyStatePurged
	destroyed.PurgedAt, destroyed.KeyDeletionDate = &purgedAt, &deletionDate

	deployment := newDatasetDeployment(t)
	copied := deployment.record(t, destroyed)
	deployment.deploy(t)

	validation := deployment.validate()

	for _, name := range []string{deployedAccessOf(copied), keyStateCheckPrefix} {
		check := findCheck(t, validation.Results(), name)
		if !check.Skipped {
			t.Errorf("expected %q to be skipped once the key is destroyed, got %+v", check.Name, check)
		}
		if !strings.Contains(check.Error, deletionDate.Format(time.RFC3339)) {
			t.Errorf("expected %q to say when the key was destroyed, got: %s", check.Name, check.Error)
		}
	}
}

// TestValidateDatasetReportsEachPrerequisite covers the prerequisites a copy depends on
// being reported one by one on a deployment where all of them are in place. Each fails
// late and expensively -- a working bucket that cannot be written to is discovered after a
// copy has started -- so each is worth a line of its own.
func TestValidateDatasetReportsEachPrerequisite(t *testing.T) {
	deployment, _, _ := grantedDataset(t)
	results := deployment.validate().Results()

	for _, name := range []string{
		copyOrchestrationCheck,
		workingBucketCheck,
		sourceCredentialsCheck,
		sourceEndpointCheck,
	} {
		if check := findCheck(t, results, name); !check.Passed {
			t.Errorf("expected %q to pass on a healthy deployment, got: %s", check.Name, check.Error)
		}
	}

	// Two of them cannot be established from here at all: the source store's credentials
	// never pass through this tool, and its endpoint is reached from the subnets a copy
	// runs in rather than from wherever this was run. Both say so instead of claiming a
	// result they did not establish.
	for _, name := range []string{sourceCredentialsCheck, sourceEndpointCheck} {
		check := findCheck(t, results, name)
		if !check.Skipped {
			t.Errorf("expected %q to report what it could not establish rather than passing outright: %s",
				check.Name, check.Error)
		}
		if check.Error == "" {
			t.Errorf("expected %q to say how far it got", check.Name)
		}
	}
}

// TestValidateDatasetNamesAMissingPrerequisite covers configuration that leaves a copy
// unable to run. The useful answer to "the copy will not start" is which resource is
// missing and whose deployment provides it, so each failure is asserted to name the
// resource rather than only the call that noticed.
func TestValidateDatasetNamesAMissingPrerequisite(t *testing.T) {
	tests := []struct {
		name      string
		configure func(opts *ValidateDatasetOpts)
		check     string
		wantText  string
	}{
		{
			name:      "no copy state machine configured",
			configure: func(opts *ValidateDatasetOpts) { opts.Copy.StateMachineARN = "" },
			check:     copyOrchestrationCheck,
			wantText:  "copy state machine",
		},
		{
			name:      "no working bucket for a copy to read its instructions from",
			configure: func(opts *ValidateDatasetOpts) { opts.Copy.WorkingBucket = "" },
			check:     workingBucketCheck,
			wantText:  "copy working bucket",
		},
		{
			name:      "no credentials secret for the source store",
			configure: func(opts *ValidateDatasetOpts) { opts.Source.CredentialsSecret = "" },
			check:     sourceCredentialsCheck,
			wantText:  "source store credentials secret",
		},
		{
			name: "a credentials identifier naming something that is not a secret",
			configure: func(opts *ValidateDatasetOpts) {
				opts.Source.CredentialsSecret = "arn:aws:s3:::not-a-secret" // pragma: allowlist secret
			},
			check:    sourceCredentialsCheck,
			wantText: "rather than a secret",
		},
		{
			name:      "no endpoint to read the source store at",
			configure: func(opts *ValidateDatasetOpts) { opts.Source.EndpointURL = "" },
			check:     sourceEndpointCheck,
			wantText:  "source store endpoint",
		},
		{
			name:      "no ledger to read the intended access from",
			configure: func(opts *ValidateDatasetOpts) { opts.Grants = nil },
			check:     datasetLedgerCheck,
			wantText:  "no ledger was configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment, _, _ := grantedDataset(t)
			tt.configure(&deployment.opts)

			check := findCheck(t, deployment.validate().Results(), tt.check)
			if check.Passed {
				t.Fatalf("expected %q to fail on a deployment missing it, got a pass", check.Name)
			}
			if !strings.Contains(check.Error, tt.wantText) {
				t.Errorf("expected the failure to name %q, got: %s", tt.wantText, check.Error)
			}
		})
	}
}

// TestValidateDatasetSkipsTheComparisonWithAnEmptyLedger covers a ledger that holds
// nothing. There is no intended access to compare the deployed documents against, and a
// check that passed here would report a deployment as verified when nothing was looked at.
// The ledger itself is readable, which is the distinction the skip has to keep.
func TestValidateDatasetSkipsTheComparisonWithAnEmptyLedger(t *testing.T) {
	deployment := newDatasetDeployment(t)
	validation := deployment.validate()

	if check := findCheck(t, validation.Results(), datasetLedgerCheck); !check.Passed {
		t.Errorf("expected an empty ledger to still read as a readable one, got: %s", check.Error)
	}

	comparison := findCheck(t, validation.Results(), deployedAccessCheck)
	if !comparison.Skipped {
		t.Errorf("expected the comparison to be skipped rather than reported as a result, got %+v", comparison)
	}
	if comparison.Error == "" {
		t.Error("expected the skip to say why there was nothing to compare against")
	}

	// No copy means no dataset, and so no key whose state bears on anything.
	for _, result := range validation.Results() {
		if strings.Contains(result.Name, keyStateCheckPrefix) {
			t.Errorf("expected no key to be reported on when no dataset is recorded, got %q", result.Name)
		}
	}
	if len(deployment.objects.PutBucketPolicyCalls) != 0 {
		t.Errorf("expected an empty ledger to write nothing, got %d bucket policy writes",
			len(deployment.objects.PutBucketPolicyCalls))
	}
}

// driftedAndUntouched returns two datasets, one of which has had a deployed grant edited
// outside this tool. One dataset in the ledger cannot tell reconciliation touching only
// what differs apart from reconciliation touching everything.
func driftedAndUntouched(t *testing.T) (*datasetDeployment, *dataset.Copy, *dataset.Copy) {
	t.Helper()

	deployment := newDatasetDeployment(t)
	drifted := deployment.record(t, cohortACopy())
	untouched := deployment.record(t, cohortBCopy())
	granted := deployment.grant(t, drifted, grantOf(datasetResearcherAccount, datasetTargetVpc))
	deployment.grant(t, untouched, grantOf(datasetResearcherAccount, datasetTargetVpc))
	deployment.deploy(t)

	deployment.drift(t, granted, func(st *policy.Statement) {
		st.Principal = policy.NewAWSPrincipal("arn:aws:iam::" + datasetStrangerAccount + ":root")
	})
	return deployment, drifted, untouched
}

// TestValidateDatasetReconcilesOnlyWhatDiffers covers the one thing here that writes.
// Re-applying a dataset replaces both of its documents whole, so doing it to a dataset that
// matches would be a write nobody asked for on a deployment nothing is wrong with.
func TestValidateDatasetReconcilesOnlyWhatDiffers(t *testing.T) {
	deployment, drifted, _ := driftedAndUntouched(t)
	deployment.opts.Reconcile = true

	validation := deployment.validate()

	if len(validation.Repairs) != 1 {
		t.Fatalf("expected only the drifted dataset to be re-applied, got %v", checkNames(validation.Repairs))
	}
	repair := validation.Repairs[0]
	if !strings.Contains(repair.Name, drifted.DatasetURI) {
		t.Errorf("re-applied %q, want the drifted dataset %s", repair.Name, drifted.DatasetURI)
	}
	if !repair.Passed {
		t.Errorf("expected the re-application to succeed, got: %s", repair.Error)
	}

	// A repair wrote to a bucket and to a key policy, so it is kept out of the
	// observations: an operator reading the output should not have to work out which of
	// the two a line is.
	for _, check := range validation.Checks {
		if strings.Contains(check.Name, repairCheckPrefix) {
			t.Errorf("expected repairs to be reported apart from the checks, found %q among them", check.Name)
		}
	}
	findCheck(t, validation.Results(), repairOf(drifted))

	if len(deployment.objects.PutBucketPolicyCalls) != 1 {
		t.Fatalf("expected one bucket policy to be replaced, got %d", len(deployment.objects.PutBucketPolicyCalls))
	}
	if got := aws.ToString(deployment.objects.PutBucketPolicyCalls[0].Bucket); got != drifted.HotCacheBucket {
		t.Errorf("replaced the policy of bucket %q, want %q", got, drifted.HotCacheBucket)
	}
	if len(deployment.keys.PutKeyPolicyCalls) != 1 {
		t.Fatalf("expected one key policy to be replaced, got %d", len(deployment.keys.PutKeyPolicyCalls))
	}
	if got := aws.ToString(deployment.keys.PutKeyPolicyCalls[0].KeyId); got != drifted.KeyArn {
		t.Errorf("replaced the policy of key %q, want %q", got, drifted.KeyArn)
	}
}

// TestValidateDatasetWritesNothingWithoutReconciliation covers the same drift left alone.
// Reconciliation is never implied: a pass an operator ran to find out what is deployed
// must not change what is deployed, however obvious the repair looks.
func TestValidateDatasetWritesNothingWithoutReconciliation(t *testing.T) {
	deployment, drifted, untouched := driftedAndUntouched(t)

	validation := deployment.validate()

	if check := findCheck(t, validation.Results(), deployedAccessOf(drifted)); check.Passed {
		t.Error("expected the drifted dataset to be reported as differing from the ledger")
	}
	if check := findCheck(t, validation.Results(), deployedAccessOf(untouched)); !check.Passed {
		t.Errorf("expected the untouched dataset to pass, got: %s", check.Error)
	}
	if len(validation.Repairs) != 0 {
		t.Errorf("expected no repairs, got %v", checkNames(validation.Repairs))
	}
	if writes := len(deployment.objects.PutBucketPolicyCalls); writes != 0 {
		t.Errorf("expected no bucket policy to be written, got %d", writes)
	}
	if writes := len(deployment.keys.PutKeyPolicyCalls); writes != 0 {
		t.Errorf("expected no key policy to be written, got %d", writes)
	}
}
