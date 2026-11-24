package cmd

// Fixtures shared by the tests of the commands that copy a dataset into this account and
// then grant, move, withdraw, or destroy access to it.
//
// Each of those commands is built around the same things -- the two ledgers, the applier
// that writes the two documents access is enforced by, and the publisher that rebuilds the
// researcher's own description of it -- so they are assembled once here and every test
// starts from the same ground. What differs between the tests is the state of the records
// and the behaviour of the doubles, which is exactly what each of them is about.
//
// Copies are seeded as records and grants are seeded by granting. A copy can be written
// straight into the table in any state, because its identifier means nothing beyond naming
// it. A grant cannot: the ledger mints its identifier, and the statements it contributes to
// the deployed documents are named after that identifier, so a hand-made record would have
// to reproduce the minting before it rendered the way a real one does. Where a test needs a
// withdrawn grant, it grants and then withdraws, which is the only way one comes to exist.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
)

const (
	releaseDataset    = "s3://ceph-source/datasets/cohort-a/"
	releaseBucket     = "data-releaser-dataset-cohort-a"
	releaseKeyArn     = "arn:aws:kms:ap-southeast-2:999988887777:key/1a2b3c4d-5e6f-7081-92a3-b4c5d6e7f809"
	releaseResearcher = "123456789012"
	releaseVpc        = "vpc-0a1b2c3d4e5f60718"

	// A second researcher, reading from a VPC of their own. One copy carrying two live
	// grants is now an ordinary case rather than a refusal, so the fixtures have to be
	// able to describe it.
	secondResearcher = "210987654321"
	secondVpc        = "vpc-0f1e2d3c4b5a69788"

	// objectSize is what every object a fixture bucket holds weighs, so a total is the
	// count multiplied by it and a report quoting the wrong one is visible.
	objectSize = 4096

	releaseAccount        = "999988887777"
	releaseRegion         = "ap-southeast-2"
	releaseManifestBucket = "data-releaser-manifests"
)

// releaseClock is the instant the fixtures are dated from.
//
// It is derived from the current time rather than pinned to a fixed date, because not
// every clock in this path can be pinned: the publisher builds a researcher's document
// against its own, and only the grants giving access at that moment appear in it. A
// fixture dated to a fixed day in the past would therefore read as expired however the
// commands behaved, and the document would describe nothing. Truncated to the second
// because that is the precision an expiry is written and compared at.
var releaseClock = time.Now().UTC().Truncate(time.Second)

// copyID renders a stand-in identifier of the same shape and length as one the ledger
// mints, and legible enough that a failure says which record it found.
func copyID(n int) string {
	const head = "01JCOPY"
	return head + strings.Repeat("0", 25-len(head)) + string(rune('0'+n))
}

// releaseConfig is a deployment configured for the direct-read mechanism and nothing
// else, which is the point: none of these commands may need a field belonging to the
// other mechanism.
func releaseConfig() *config.Config {
	return &config.Config{
		Account: releaseAccount,
		Region:  releaseRegion,
		Ledger:  config.LedgerConfig{Table: config.DefaultLedgerTable},
		S3: config.S3Config{
			Copy: config.CopyServiceConfig{
				StateMachineArn: "arn:aws:states:ap-southeast-2:999988887777:stateMachine:copy",
				WorkingBucket:   "copy-working",
				WriterRoleName:  "copy-writer",
			},
			Source: config.SourceStoreConfig{
				EndpointURL:       "https://ceph.example.org",
				Region:            "us-east-1",
				CredentialsSecret: "ceph/source",
			},
			HotCache: config.HotCacheConfig{
				NamePrefix: config.DefaultHotCacheNamePrefix,
				Buckets:    map[string]string{releaseDataset: releaseBucket},
			},
			Manifest: config.ManifestConfig{
				Bucket:      releaseManifestBucket,
				KeyTemplate: config.DefaultDatasetManifestKeyTemplate,
			},
			CustodianRoleArn: "arn:aws:iam::999988887777:role/custodian",
		},
	}
}

// copyFixture builds a copy in the given state, carrying everything it would have
// accumulated by the time it got there. The accumulated fields matter: a refused command
// has to be seen to have left all of them alone, not only the state.
func copyFixture(id string, state dataset.CopyState) *dataset.Copy {
	c := &dataset.Copy{
		CopyID:           id,
		DatasetURI:       releaseDataset,
		HotCacheBucket:   releaseBucket,
		KeyArn:           releaseKeyArn,
		State:            state,
		CreatedAt:        releaseClock.Add(-30 * 24 * time.Hour),
		CopyExecutionArn: "arn:aws:states:ap-southeast-2:999988887777:execution:copy:" + id,
	}

	switch state {
	case dataset.CopyStateCopying:
		// The transfer has not reported, so there are no numbers yet.
		return c
	case dataset.CopyStateFailed:
		// Whatever it managed before it stopped, which is what makes a failed copy
		// dangerous: the bucket holds some of the dataset.
		c.ObjectCount, c.TotalBytes = 3, 4096
		return c
	}

	c.ObjectCount, c.TotalBytes = 12, 1<<30
	c.CopySummaryLocation = "s3://" + releaseBucket + "/.copy-control/summary.csv"

	if state == dataset.CopyStatePurged || state == dataset.CopyStateDeleted {
		purgedAt := releaseClock.Add(-time.Hour)
		deletionDate := purgedAt.Add(30 * 24 * time.Hour)
		c.PurgedAt = &purgedAt
		c.KeyDeletionDate = &deletionDate
	}
	// A deleted copy was purged first and then had its objects removed, so it carries
	// both timestamps: what the transfer delivered is still the record of what happened,
	// and deleting the objects does not unsay it.
	if state == dataset.CopyStateDeleted {
		deletedAt := releaseClock.Add(-time.Minute)
		c.DeletedAt = &deletedAt
	}
	return c
}

// grantSeed describes one grant to make against a copy. The zero value grants the fixture
// researcher, from the fixture VPC, until the fixture expiry, which is what most tests
// want; the fields exist for the ones that need a second researcher or an expiry of their
// own.
type grantSeed struct {
	account string
	vpc     string
	// expiresAt overrides the fixture expiry. Set unlimited instead to grant with none at
	// all, since a nil here cannot mean both "the default" and "no expiry".
	expiresAt *time.Time
	unlimited bool
	// revoked withdraws the grant once it has been made, which is the only way a
	// withdrawn one exists.
	revoked bool
}

// releaseDoubles are the AWS services these commands touch, kept together so a test can
// assert on what was written as easily as on what came back.
type releaseDoubles struct {
	ledger  *awstest.DynamoDB
	objects *awstest.S3
	keys    *awstest.KMS
	copies  *awstest.StepFunctions
}

func newReleaseDoubles() *releaseDoubles {
	// The key reports itself enabled and names its own ARN, which is the case every test
	// but the ones about a key's state starts from. The shared double reports no ARN, and
	// a copy that cannot name the key its data is encrypted under can never be purged,
	// so staging refuses it.
	keys := &awstest.KMS{DescribeKeyFn: keyInState(kmstypes.KeyStateEnabled)}

	return &releaseDoubles{
		ledger:  awstest.NewLedger(),
		objects: &awstest.S3{},
		keys:    keys,
		copies:  &awstest.StepFunctions{},
	}
}

// keyInState returns a key description reporting the given state, for the tests that turn
// on what the key can still do.
func keyInState(state kmstypes.KeyState) func(context.Context, *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
	return func(_ context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
		metadata := &kmstypes.KeyMetadata{
			KeyId:    params.KeyId,
			Arn:      aws.String(releaseKeyArn),
			KeyState: state,
			Enabled:  state == kmstypes.KeyStateEnabled,
		}
		if state == kmstypes.KeyStatePendingDeletion {
			deletionDate := releaseClock.Add(30 * 24 * time.Hour)
			metadata.DeletionDate = &deletionDate
		}
		return &kms.DescribeKeyOutput{KeyMetadata: metadata}, nil
	}
}

// keyRecoveringFromDeletion answers for a key being taken back off a scheduled deletion:
// pending deletion until that deletion is cancelled, disabled once it is -- which is what
// a cancellation actually leaves behind -- and enabled only once something has enabled it.
//
// The state is derived from the calls the double recorded rather than from a counter, so a
// caller that cancels and stops there is answered honestly, with the disabled key that
// decrypts nothing.
func keyRecoveringFromDeletion(keys *awstest.KMS) func(context.Context, *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
	return func(ctx context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
		state := kmstypes.KeyStatePendingDeletion
		switch {
		case len(keys.EnableKeyCalls) > 0:
			state = kmstypes.KeyStateEnabled
		case len(keys.CancelDeletionCalls) > 0:
			state = kmstypes.KeyStateDisabled
		}
		return keyInState(state)(ctx, params)
	}
}

// keyDestroyed answers as KMS does once a scheduled deletion has matured: the key is not
// there at all, and neither is the material that could read what it encrypted.
func keyDestroyed() func(context.Context, *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
	return func(context.Context, *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
		return nil, &kmstypes.NotFoundException{Message: aws.String("key does not exist")}
	}
}

func (d *releaseDoubles) clients() releaseClients {
	return releaseClients{ledger: d.ledger, objects: d.objects, keys: d.keys}
}

// copyStore and grantStore are the two ledgers, built against the same table the commands
// build theirs against.
func (d *releaseDoubles) copyStore() *dataset.CopyStore {
	return dataset.NewCopyStore(d.ledger, config.DefaultLedgerTable)
}

func (d *releaseDoubles) grantStore() *dataset.GrantStore {
	return dataset.NewGrantStore(d.ledger, config.DefaultLedgerTable)
}

// seedCopies writes copies straight into the table, so a test can start from any state
// without walking the lifecycle to get there.
func (d *releaseDoubles) seedCopies(t *testing.T, copies ...*dataset.Copy) {
	t.Helper()

	store := d.copyStore()
	for _, c := range copies {
		if err := store.Record(context.Background(), c); err != nil {
			t.Fatalf("seeding copy %s: %v", c.CopyID, err)
		}
	}
}

// seedCopy writes one copy in the given state and returns it, which is what most tests
// want: a copy to hang grants off and to assert against afterwards.
func (d *releaseDoubles) seedCopy(t *testing.T, id string, state dataset.CopyState) *dataset.Copy {
	t.Helper()

	c := copyFixture(id, state)
	d.seedCopies(t, c)
	return c
}

// seedPurgedCopy stages a copy, grants it, and purges it, which is the only order that
// produces what a purge leaves behind. A copy cannot be granted once it is purged and a
// grant cannot be made against a record written by hand, so the fixture walks the same
// path the commands do and returns the copy and the grants as they now stand.
func (d *releaseDoubles) seedPurgedCopy(t *testing.T, id string, seeds ...grantSeed) (*dataset.Copy, []*dataset.Grant) {
	t.Helper()

	ctx := context.Background()
	staged := d.seedCopy(t, id, dataset.CopyStateStaged)
	grants := d.seedGrants(t, staged, seeds...)

	purged, err := d.copyStore().MarkPurged(ctx, staged.CopyID, releaseClock.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("purging seeded copy %s: %v", staged.CopyID, err)
	}
	return purged, grants
}

// seedGrants makes grants against a copy through the store the commands write through,
// and returns them in the order they were asked for.
//
// The copy has to have staged for this to work, which is the same condition a real grant
// is made under. A test wanting a grant on a copy in some other state grants it first and
// moves the copy afterwards, in that order, because that is the only order the ledger
// allows.
func (d *releaseDoubles) seedGrants(t *testing.T, c *dataset.Copy, seeds ...grantSeed) []*dataset.Grant {
	t.Helper()

	ctx := context.Background()
	store := d.grantStore()

	granted := make([]*dataset.Grant, 0, len(seeds))
	for _, seed := range seeds {
		expiresAt := seed.expiresAt
		if expiresAt == nil && !seed.unlimited {
			expiresAt = grantExpiry()
		}

		grant, err := store.Create(ctx, c, dataset.GrantRequest{
			ResearcherAccountID: valueOr(seed.account, releaseResearcher),
			TargetVpcID:         valueOr(seed.vpc, releaseVpc),
			ExpiresAt:           expiresAt,
		})
		if err != nil {
			t.Fatalf("seeding a grant against copy %s: %v", c.CopyID, err)
		}

		if seed.revoked {
			grant, err = store.MarkRevoked(ctx, grant.GrantID)
			if err != nil {
				t.Fatalf("withdrawing seeded grant %s: %v", grant.GrantID, err)
			}
		}
		granted = append(granted, grant)
	}
	return granted
}

// seedGrant makes one grant and returns it.
func (d *releaseDoubles) seedGrant(t *testing.T, c *dataset.Copy, seed grantSeed) *dataset.Grant {
	t.Helper()
	return d.seedGrants(t, c, seed)[0]
}

// grantExpiry is the expiry a seeded grant carries unless a test asks for another: far
// enough ahead of the clock that nothing incidental makes it lapse.
func grantExpiry() *time.Time {
	expiresAt := releaseClock.Add(90 * 24 * time.Hour)
	return &expiresAt
}

// valueOr fills in a fixture default for a field a seed left empty.
func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// item returns the stored record, failing the test when the identifier names none.
func (d *releaseDoubles) item(t *testing.T, id string) map[string]types.AttributeValue {
	t.Helper()

	stored, ok := d.ledger.Items[id]
	if !ok {
		t.Fatalf("no record stored under %s", id)
	}
	return stored
}

// attr returns one string attribute of a stored record, or the empty string where the
// record does not carry it. An absent attribute is a real answer here: an unlimited grant
// is one whose expiry attribute is not there at all.
func (d *releaseDoubles) attr(t *testing.T, id, name string) string {
	t.Helper()

	value, ok := d.item(t, id)[name].(*types.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return value.Value
}

// newTestReleaser assembles the shared machinery of the three commands against the
// doubles, with its clock pinned.
func newTestReleaser(t *testing.T, doubles *releaseDoubles) *releaser {
	t.Helper()

	r, err := newReleaser(releaseConfig(), doubles.clients())
	if err != nil {
		t.Fatalf("assembling the release commands: %v", err)
	}
	r.now = func() time.Time { return releaseClock }
	return r
}

// testCommand returns a command whose streams are captured and whose input is whatever the
// test wants read from it, so a confirmation prompt can be answered.
func testCommand(input string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetIn(strings.NewReader(input))
	return cmd, out, errOut
}

// bucketPolicyWrites returns the bucket policy documents that were written, in order.
func bucketPolicyWrites(doubles *releaseDoubles) []string {
	written := make([]string, 0, len(doubles.objects.PutBucketPolicyCalls))
	for _, call := range doubles.objects.PutBucketPolicyCalls {
		if call.Policy != nil {
			written = append(written, *call.Policy)
		}
	}
	return written
}

// manifestWrites returns the keys of the manifest objects that were written.
func manifestWrites(doubles *releaseDoubles) []string {
	var keys []string
	for _, call := range doubles.objects.PutCalls {
		if call.Bucket != nil && *call.Bucket == releaseManifestBucket && call.Key != nil {
			keys = append(keys, *call.Key)
		}
	}
	return keys
}

// stagedObjects renders the keys one copy of the fixture dataset left in its bucket: the
// objects themselves, and the copy's own control artefact, which is ours and is deleted
// with the rest.
func stagedObjects(count int) []string {
	keys := make([]string, 0, count+1)
	keys = append(keys, dataset.ControlPrefix+"/"+copyID(1)+"/summary.json")
	for i := range count {
		keys = append(keys, fmt.Sprintf("reads/sample-%05d.bam", i))
	}
	return keys
}

// bucketHolding answers a listing of the fixture cache bucket with the given keys, a page
// at a time.
//
// It paginates because the caller has to be seen to drain the listing: a bucket answered
// in one page would let a command that reads only the first page report the whole dataset
// as deleted having deleted part of it. Any other bucket answers empty, so a command
// reaching for the wrong one is visible as an empty plan rather than as somebody else's
// objects.
func bucketHolding(keys []string, pageSize int) func(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return func(_ context.Context, params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		if aws.ToString(params.Bucket) != releaseBucket {
			return &s3.ListObjectsV2Output{}, nil
		}

		from := 0
		if token := aws.ToString(params.ContinuationToken); token != "" {
			parsed, err := strconv.Atoi(token)
			if err != nil {
				return nil, fmt.Errorf("the listing was continued from %q, which this bucket never issued", token)
			}
			from = parsed
		}
		to := min(from+pageSize, len(keys))

		contents := make([]s3types.Object, 0, to-from)
		for _, key := range keys[from:to] {
			contents = append(contents, s3types.Object{Key: aws.String(key), Size: aws.Int64(objectSize)})
		}

		page := &s3.ListObjectsV2Output{Contents: contents}
		if to < len(keys) {
			page.IsTruncated = aws.Bool(true)
			page.NextContinuationToken = aws.String(strconv.Itoa(to))
		}
		return page, nil
	}
}

// deletedObjectKeys returns the keys of every object deleted through the batched call, in
// the order they were asked for.
func deletedObjectKeys(doubles *releaseDoubles) []string {
	var keys []string
	for _, call := range doubles.objects.DeleteObjectsCalls {
		if call.Delete == nil {
			continue
		}
		for _, object := range call.Delete.Objects {
			keys = append(keys, aws.ToString(object.Key))
		}
	}
	return keys
}

// manifestDeletes returns the keys of the manifest objects that were removed.
func manifestDeletes(doubles *releaseDoubles) []string {
	var keys []string
	for _, call := range doubles.objects.DeleteCalls {
		if call.Bucket != nil && *call.Bucket == releaseManifestBucket && call.Key != nil {
			keys = append(keys, *call.Key)
		}
	}
	return keys
}

// errNotFound stands in for the answer AWS gives when a resource is simply not there,
// which is the case every prerequisite check is written for.
func errNotFound(what string) error {
	return fmt.Errorf("the %s does not exist", what)
}

// failingUpdate makes every ledger write fail, so a test can exercise what a command does
// when the mutation it already applied cannot be written down.
func failingUpdate(context.Context, *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
	return nil, errors.New("the ledger is unavailable")
}
