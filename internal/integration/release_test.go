//go:build integration

package integration

// This file drives one dataset from the moment its copy lands through to the researcher's
// access being withdrawn, against live emulators: DynamoDB Local holds the ledger, and
// MinIO holds both the cache bucket whose policy is the grant and the bucket the
// researcher's own description of that grant is delivered to.
//
// A copy and a grant are two records sharing one table, and that is the whole shape of what
// is exercised here. The copy says what was transferred and where it staged; the grant says
// who may read it, from which VPC, and until when. Both are found by the same reads the
// commands make, so a filter that stopped telling one kind from the other would fail here.
//
// The three commands that move a grant are mirrored rather than invoked, since they resolve
// their clients from configuration and from an AWS session. Everything below that edge is
// real -- the two stores, the renderer, the applier, and the publisher -- and it runs in the
// order and with the arguments cmd/share_s3.go, cmd/extend_s3.go and cmd/revoke_s3.go use.
//
// Two things are answered here rather than by an emulator, both because the emulators are
// not the services they stand for, and both documented where they are defined below: the
// key, which has no emulator at all, and the cache bucket's policy, which MinIO refuses
// because a grant is bounded by a VPC and MinIO has none. Everything they touch is still
// composed by the real renderer and written by the real applier through the interfaces the
// commands hand it.
//
// Every assertion reads a value back out of where it was put rather than from what the
// code under test said it wrote, so a component that agrees with itself and with nothing
// else still fails here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/dataset"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
)

const (
	// releaseTableName is a table of this test's own, so the ledger the two stores write
	// to carries the indexes they query through and nothing else's items.
	releaseTableName = "data-releaser-ReleaseRecords"

	// hotCacheBucketName holds the copy of one dataset and is the bucket whose policy is
	// the grant. It is a different bucket from the manifest one on purpose: only this one
	// is ever named in a document handed to a researcher.
	hotCacheBucketName = "data-releaser-hot-cache-pathology"

	releaseDatasetURI = "s3://ceph-source/datasets/pathology-2024/"
	releaseKeyArn     = "arn:aws:kms:ap-southeast-2:111122223333:key/2f8c1c5e-9c2a-4b7a-9f0e-6d3b1a5c7e21"

	// ownerAccountID owns the cache bucket, the key, and the ledger.
	ownerAccountID = "111122223333"

	// releaseRegion is the region the two documents are rendered for. It is deliberately
	// not the region the emulator clients are configured with, so a value that leaked
	// from the client rather than from the settings would show up in the assertions.
	releaseRegion = "ap-southeast-2"

	copyWriterRoleName      = "data-releaser-copy-writer"
	custodianRoleARN        = "arn:aws:iam::111122223333:role/data-custodian"
	releaseCopyExecutionArn = "arn:aws:states:ap-southeast-2:111122223333:execution/data-releaser-copy/pathology-2024"

	// targetVpcID is the bare identifier the bucket policy compares against, which is a
	// different form of the same fact from the ARN the htsget mechanism records.
	targetVpcID = "vpc-0abc123def4567890"

	// researcherRootARN is the principal a grant names: the researcher's account root,
	// which delegates to their own IAM which of their principals may read.
	researcherRootARN = "arn:aws:iam::" + researcherAccountID + ":root"
)

// TestDatasetCopyAndGrantLifecycle drives one dataset through the four moves that change
// what a researcher can read, asserting the ledger items, the deployed bucket policy, and
// the researcher's manifest after each of them:
//
//  1. stage-record -> the copy has staged and the dataset is grantable. There is one
//     record, it describes a transfer, and it names nobody.
//  2. share        -> a second record appears alongside it naming the researcher and their
//     VPC, the bucket policy grants them until the recorded expiry, and the manifest
//     describes the dataset.
//  3. extend       -> the recorded expiry, the deployed time condition, and the manifest
//     all move to the same later moment. The copy is untouched.
//  4. revoke       -> the grant leaves the deployed policy, and the manifest is removed
//     because the account holds no dataset access left to describe.
func TestDatasetCopyAndGrantLifecycle(t *testing.T) {
	// Skip gracefully when Docker is unreachable so the tagged suite does not hard-fail
	// in environments without a container runtime.
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()

	ddbEndpoint := startDynamoDBLocal(ctx, t)
	s3Endpoint := startMinIO(ctx, t)

	// DynamoDB Local ignores credentials and both emulators ignore the region; MinIO
	// uses the credentials as the S3 access key and secret, so they have to match its
	// root user and password.
	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(minioUser, minioPass, ""),
	}

	ddbClient := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(ddbEndpoint)
	})
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true // MinIO / path-style addressing
	})

	createReleaseTable(ctx, t, ddbClient)
	createBucket(ctx, t, s3Client) // the manifest bucket
	createHotCacheBucket(ctx, t, s3Client)

	// The components the commands build, wired the way newStager and newReleaser wire
	// them. The ledger is two stores over one table because a copy and a grant are two
	// records: one answers what has been transferred, the other who may read it.
	copies := dataset.NewCopyStore(ddbClient, releaseTableName)
	grants := dataset.NewGrantStore(ddbClient, releaseTableName)
	keys := &awstest.KMS{}
	objects := newBucketPolicyHoldingS3(s3Client)
	applier := dataset.NewPolicyApplier(dataset.PolicySettings{
		AccountID:               ownerAccountID,
		Region:                  releaseRegion,
		CopyWriterRoleName:      copyWriterRoleName,
		CopyWriterRoleAccountID: ownerAccountID,
		CustodianRoleARN:        custodianRoleARN,
	}, objects, keys)

	manifestCfg := config.ManifestConfig{
		Bucket:      bucket,
		KeyTemplate: config.DefaultDatasetManifestKeyTemplate,
	}
	// Built from the grant ledger alone: a researcher's document describes the access they
	// hold, and a copy nobody was granted is not access.
	publisher, err := dataset.NewManifestPublisher(
		grants,
		manifest.NewWriter(s3Client),
		dataset.NewManifestBuilder(releaseRegion),
		dataset.ManifestTarget{Bucket: manifestCfg.Bucket, RenderKey: manifestCfg.RenderManifestKey},
	)
	if err != nil {
		t.Fatalf("building the manifest publisher: %v", err)
	}

	manifestKey, err := manifestCfg.RenderManifestKey(researcherAccountID)
	if err != nil {
		t.Fatalf("rendering the manifest key: %v", err)
	}
	// The dataset document sits under its own prefix so a researcher holding both kinds
	// of access does not have one description overwrite the other.
	if manifestKey != "datasets/777788889999/manifest.json" {
		t.Fatalf("unexpected manifest key: %q", manifestKey)
	}

	// Staging: the transfer is recorded before it runs and moved to staged when it
	// reports.
	created, err := copies.Create(ctx, dataset.CreateCopyInput{
		DatasetURI:       releaseDatasetURI,
		HotCacheBucket:   hotCacheBucketName,
		KeyArn:           releaseKeyArn,
		CopyExecutionArn: releaseCopyExecutionArn,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.CopyID == "" {
		t.Fatal("a created copy has no identifier for a grant to be made against")
	}

	staged, err := copies.MarkStaged(ctx, created.CopyID, dataset.CopyOutcome{
		ObjectCount:     128,
		TotalBytes:      4 << 30,
		SummaryLocation: "s3://" + hotCacheBucketName + "/" + dataset.ControlPrefix + "/" + created.CopyID + "/summary.json",
	})
	if err != nil {
		t.Fatalf("MarkStaged: %v", err)
	}

	item := requireLedgerItem(ctx, t, ddbClient, staged.CopyID, "after staging")
	if got := stringAttr(t, item, "state"); got != string(dataset.CopyStateStaged) {
		t.Fatalf("state after staging: got %q, want %q", got, dataset.CopyStateStaged)
	}
	if got := stringAttr(t, item, model.MechanismAttribute); got != string(model.MechanismS3) {
		t.Fatalf("mechanism: got %q, want %q; the ledger is shared, so an unmarked item reads as the other mechanism's", got, model.MechanismS3)
	}
	// The kind is what tells a copy from a grant once both are in the table under the same
	// dataset. An item carrying none is neither, and every read refuses it.
	if got := stringAttr(t, item, "kind"); got != "copy" {
		t.Fatalf("kind: got %q, want %q", got, "copy")
	}
	if got := stringAttr(t, item, "datasetUri"); got != releaseDatasetURI {
		t.Fatalf("datasetUri: got %q, want %q", got, releaseDatasetURI)
	}
	if got := stringAttr(t, item, "hotCacheBucket"); got != hotCacheBucketName {
		t.Fatalf("hotCacheBucket: got %q, want %q", got, hotCacheBucketName)
	}
	if got := stringAttr(t, item, "keyArn"); got != releaseKeyArn {
		t.Fatalf("keyArn: got %q, want %q", got, releaseKeyArn)
	}
	// A transfer is given to nobody, so a copy stays out of the per-researcher index
	// entirely rather than appearing there under a blank account.
	if _, named := item["researcherAccountId"]; named {
		t.Fatal("a copy must carry no researcher account")
	}
	if _, dated := item["expiresAt"]; dated {
		t.Fatal("a copy must carry no expiry: nothing has been granted to expire")
	}

	// Nothing has been granted, so the researcher has no access for a document to
	// describe and no document.
	if objectExists(ctx, t, s3Client, manifestKey) {
		t.Fatalf("no manifest may exist at s3://%s/%s before anything is granted", bucket, manifestKey)
	}

	// Sharing. The command finds the copy to grant against through the dataset index, so
	// the index is exercised the way it will be in service -- and so is the filter that
	// keeps the grants written below from answering as transfers.
	ofDataset, err := copies.CopiesOfDataset(ctx, releaseDatasetURI)
	if err != nil {
		t.Fatalf("CopiesOfDataset: %v", err)
	}
	if len(ofDataset) != 1 || ofDataset[0].CopyID != staged.CopyID {
		t.Fatalf("dataset index: got %d copy(s), want the one staged copy %s", len(ofDataset), staged.CopyID)
	}

	// To the second, because that is the precision the deployed time condition is written
	// and compared at.
	grantedUntil := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)

	// The state of the key is read before the record is written, since a key that cannot
	// decrypt makes a grant undeployable rather than merely undeployed. The double reports
	// an enabled key, so this passes and nothing is written by it.
	if err := applier.CheckGrantable(ctx, datasetPolicyTarget(staged, nil)); err != nil {
		t.Fatalf("CheckGrantable: %v", err)
	}

	granted, err := grants.Create(ctx, staged, dataset.GrantRequest{
		ResearcherAccountID: researcherAccountID,
		TargetVpcID:         targetVpcID,
		ExpiresAt:           &grantedUntil,
	})
	if err != nil {
		t.Fatalf("granting: %v", err)
	}
	if granted.GrantID == staged.CopyID {
		t.Fatal("the grant took the copy's identifier; the two are separate records and one must not overwrite the other")
	}

	// Rendered from the ledger after it has been written, and from every grant against the
	// copy rather than the one just made: the documents are shared, so rendering from one
	// grant would take everyone else's access away.
	applyDeployedGrants(ctx, t, applier, grants, staged)
	if _, err := publisher.Publish(ctx, researcherAccountID); err != nil {
		t.Fatalf("publishing the manifest after the grant: %v", err)
	}

	item = requireLedgerItem(ctx, t, ddbClient, granted.GrantID, "after the grant")
	if got := stringAttr(t, item, "state"); got != string(dataset.GrantStateGranted) {
		t.Fatalf("state after the grant: got %q, want %q", got, dataset.GrantStateGranted)
	}
	if got := stringAttr(t, item, "kind"); got != "grant" {
		t.Fatalf("kind: got %q, want %q", got, "grant")
	}
	// The copy the grant reads from is on the record, which is what lets the grants of one
	// copy be found without an index of their own.
	if got := stringAttr(t, item, "copyId"); got != staged.CopyID {
		t.Fatalf("copyId: got %q, want %q", got, staged.CopyID)
	}
	if got := stringAttr(t, item, "researcherAccountId"); got != researcherAccountID {
		t.Fatalf("researcherAccountId: got %q, want %q", got, researcherAccountID)
	}
	if got := stringAttr(t, item, "targetVpcId"); got != targetVpcID {
		t.Fatalf("targetVpcId: got %q, want %q", got, targetVpcID)
	}
	recordedExpiry := stringAttr(t, item, "expiresAt")
	if want := grantedUntil.Format(time.RFC3339); recordedExpiry != want {
		t.Fatalf("expiresAt: got %q, want %q", recordedExpiry, want)
	}

	// The copy is untouched by the grant: what was transferred and who may read it are two
	// records precisely so that changing one cannot rewrite the other.
	copyItem := requireLedgerItem(ctx, t, ddbClient, staged.CopyID, "after the grant")
	if got := stringAttr(t, copyItem, "state"); got != string(dataset.CopyStateStaged) {
		t.Fatalf("the copy is %q after the grant; granting changes nothing about the transfer", got)
	}
	if _, named := copyItem["researcherAccountId"]; named {
		t.Fatal("granting wrote a researcher onto the copy record")
	}

	deployed := deployedBucketPolicy(ctx, t, objects, hotCacheBucketName)
	grant := requireGrantStatement(t, deployed, granted.GrantID, "Allow", "the bucket policy after the grant")
	if principals := principalARNs(t, grant); !slices.Contains(principals, researcherRootARN) {
		t.Fatalf("granting statement names %v, want the researcher account root %s", principals, researcherRootARN)
	}
	if got := soleConditionValue(t, grant, "StringEquals", "aws:SourceVpc"); got != targetVpcID {
		t.Fatalf("deployed VPC bound: got %q, want %q", got, targetVpcID)
	}
	// The deployed condition and the recorded expiry have to be the same moment, or
	// reconciliation reads the difference as drift.
	if got := soleConditionValue(t, grant, "DateLessThan", "aws:CurrentTime"); got != recordedExpiry {
		t.Fatalf("deployed expiry: got %q, want the recorded %q", got, recordedExpiry)
	}
	// The grant also withholds the copy orchestration's own artefacts, which share the
	// bucket with the data and are not part of the dataset.
	requireGrantStatement(t, deployed, granted.GrantID, "Deny", "the bucket policy after the grant")
	// A rendered document replaces the deployed one whole, so the baseline the deployment
	// establishes has to survive every render.
	requireStatement(t, deployed, dataset.SidDenyInsecureTransport, "the bucket policy after the grant")

	// The key policy is the other half of the grant: a bucket policy admitting a
	// researcher whose reads cannot be decrypted denies them indistinguishably.
	requireGrantStatement(t, keyPolicyPut(t, keys, 0), granted.GrantID, "Allow", "the first key policy written")

	if !objectExists(ctx, t, s3Client, manifestKey) {
		t.Fatalf("expected the manifest at s3://%s/%s after the grant", bucket, manifestKey)
	}
	published := readDatasetManifest(ctx, t, s3Client, manifestKey)
	if published.AccountID != researcherAccountID {
		t.Fatalf("manifest accountId: got %q, want %q", published.AccountID, researcherAccountID)
	}
	if len(published.Datasets) != 1 {
		t.Fatalf("manifest describes %d dataset(s), want 1", len(published.Datasets))
	}
	describedGrant := published.Datasets[0]
	if describedGrant.DatasetURI != releaseDatasetURI {
		t.Fatalf("manifest datasetUri: got %q, want %q", describedGrant.DatasetURI, releaseDatasetURI)
	}
	// The entry is identified by the grant it describes, which is what the researcher
	// quotes when they ask about the access rather than about the transfer behind it.
	if describedGrant.GrantID != granted.GrantID {
		t.Fatalf("manifest grantId: got %q, want %q", describedGrant.GrantID, granted.GrantID)
	}
	if describedGrant.Bucket != hotCacheBucketName || describedGrant.Region != releaseRegion {
		t.Fatalf("manifest addresses %s in %s, want %s in %s",
			describedGrant.Bucket, describedGrant.Region, hotCacheBucketName, releaseRegion)
	}
	if describedGrant.KeyArn != releaseKeyArn {
		t.Fatalf("manifest keyArn: got %q, want %q", describedGrant.KeyArn, releaseKeyArn)
	}
	if describedGrant.TargetVpcID != targetVpcID {
		t.Fatalf("manifest targetVpcId: got %q, want %q", describedGrant.TargetVpcID, targetVpcID)
	}
	if describedGrant.ExpiresAt == nil || !describedGrant.ExpiresAt.Equal(grantedUntil) {
		t.Fatalf("manifest expiry: got %v, want %s", describedGrant.ExpiresAt, grantedUntil)
	}

	// Extending. The record is written first and the documents are rendered from what came
	// back, so the deployed condition carries the moment the ledger minted rather than a
	// second reading of a second clock.
	extended, err := grants.Extend(ctx, granted.GrantID, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if extended.ExpiresAt == nil || !extended.ExpiresAt.After(*granted.ExpiresAt) {
		t.Fatalf("the extension did not move the expiry: %v is not after %s", extended.ExpiresAt, granted.ExpiresAt)
	}
	applyDeployedGrants(ctx, t, applier, grants, staged)
	if _, err := publisher.Publish(ctx, researcherAccountID); err != nil {
		t.Fatalf("publishing the manifest after the extension: %v", err)
	}

	item = requireLedgerItem(ctx, t, ddbClient, extended.GrantID, "after the extension")
	extendedExpiry := stringAttr(t, item, "expiresAt")
	if extendedExpiry == recordedExpiry {
		t.Fatalf("the recorded expiry is still %q after the extension", recordedExpiry)
	}
	if want := extended.ExpiresAt.Format(time.RFC3339); extendedExpiry != want {
		t.Fatalf("expiresAt after the extension: got %q, want %q", extendedExpiry, want)
	}
	if got := stringAttr(t, item, "state"); got != string(dataset.GrantStateGranted) {
		t.Fatalf("an extension moves the expiry and nothing else, but the state is now %q", got)
	}

	deployed = deployedBucketPolicy(ctx, t, objects, hotCacheBucketName)
	grant = requireGrantStatement(t, deployed, extended.GrantID, "Allow", "the bucket policy after the extension")
	if got := soleConditionValue(t, grant, "DateLessThan", "aws:CurrentTime"); got != extendedExpiry {
		t.Fatalf("the deployed time condition is %q, want the extended %q", got, extendedExpiry)
	}
	if got := soleConditionValue(t, grant, "StringEquals", "aws:SourceVpc"); got != targetVpcID {
		t.Fatalf("the extension changed the deployed VPC bound to %q", got)
	}

	republished := readDatasetManifest(ctx, t, s3Client, manifestKey)
	if len(republished.Datasets) != 1 {
		t.Fatalf("manifest describes %d dataset(s) after the extension, want 1", len(republished.Datasets))
	}
	if got := republished.Datasets[0].ExpiresAt; got == nil || !got.Equal(*extended.ExpiresAt) {
		t.Fatalf("manifest expiry after the extension: got %v, want %s", got, extended.ExpiresAt)
	}

	// Withdrawing. There is no separate operation on the documents: the copy is rendered
	// with the withdrawn grant described as the ledger is about to describe it, which
	// leaves the baseline pair a copy nobody has ever been given renders. The documents go
	// first here and the record second, so the gate closes before anything else.
	withdrawn := *extended
	withdrawn.State = dataset.GrantStateRevoked
	if _, err := applier.Apply(ctx, datasetPolicyTarget(staged, []*dataset.Grant{&withdrawn})); err != nil {
		t.Fatalf("applying the withdrawal: %v", err)
	}

	revoked, err := grants.MarkRevoked(ctx, extended.GrantID)
	if err != nil {
		t.Fatalf("MarkRevoked: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("a withdrawn grant must record when its access was taken back")
	}
	if _, err := publisher.Publish(ctx, researcherAccountID); err != nil {
		t.Fatalf("publishing the manifest after the withdrawal: %v", err)
	}

	item = requireLedgerItem(ctx, t, ddbClient, revoked.GrantID, "after the withdrawal")
	if got := stringAttr(t, item, "state"); got != string(dataset.GrantStateRevoked) {
		t.Fatalf("state after the withdrawal: got %q, want %q", got, dataset.GrantStateRevoked)
	}
	if got := stringAttr(t, item, "revokedAt"); got == "" {
		t.Fatal("revokedAt is absent or empty after the withdrawal")
	}
	// The researcher and the VPC stay on the record: the ledger keeps what was granted,
	// and neither is read while the state says revoked.
	if got := stringAttr(t, item, "researcherAccountId"); got != researcherAccountID {
		t.Fatalf("researcherAccountId after the withdrawal: got %q, want it kept as %q", got, researcherAccountID)
	}
	// The copy outlives the grant. The data is still in the bucket and can be granted
	// again without being transferred a second time, which is the point of holding the two
	// apart.
	copyItem = requireLedgerItem(ctx, t, ddbClient, staged.CopyID, "after the withdrawal")
	if got := stringAttr(t, copyItem, "state"); got != string(dataset.CopyStateStaged) {
		t.Fatalf("the copy is %q after the withdrawal; withdrawing access destroys nothing", got)
	}

	deployed = deployedBucketPolicy(ctx, t, objects, hotCacheBucketName)
	if found := statementsOfGrant(deployed, revoked.GrantID); len(found) != 0 {
		t.Fatalf("the withdrawn grant still contributes %d statement(s), so the researcher can still read", len(found))
	}
	// Removing the grant's own statements is not enough on its own: the withdrawal has to
	// leave no statement addressing the researcher at all.
	for _, st := range deployed.Statement {
		if principals := principalARNs(t, st); slices.Contains(principals, researcherRootARN) {
			t.Fatalf("statement %q still addresses the researcher account after the withdrawal", st.Sid)
		}
	}
	requireStatement(t, deployed, dataset.SidDenyInsecureTransport, "the bucket policy after the withdrawal")

	// The key policy is written second when access is being withdrawn, so the gate closes
	// first if only one of the two lands.
	if found := statementsOfGrant(keyPolicyPut(t, keys, 2), revoked.GrantID); len(found) != 0 {
		t.Fatal("the key still carries the researcher's decrypt permission after the withdrawal")
	}

	// The account holds no dataset access left, so its description is removed rather than
	// left advertising access that has ended.
	if objectExists(ctx, t, s3Client, manifestKey) {
		t.Fatalf("expected the manifest at s3://%s/%s to be deleted after the withdrawal", bucket, manifestKey)
	}
}

// datasetPolicyTarget gathers the whole of what a copy's two documents have to say: the
// bucket and key the copy staged in, and the grants standing against it. It mirrors
// cmd/releaser.go's own gatherer, which is the only place a target is built there for the
// same reason it is the only place here -- a target assembled from one grant would render
// documents that take every other researcher's access away.
func datasetPolicyTarget(c *dataset.Copy, grants []*dataset.Grant) dataset.PolicyTarget {
	return dataset.PolicyTarget{
		DatasetURI:     c.DatasetURI,
		HotCacheBucket: c.HotCacheBucket,
		KeyArn:         c.KeyArn,
		Grants:         grants,
	}
}

// applyDeployedGrants renders and writes both documents from the ledger as it now stands,
// which is what granting and extending both do once the record is written. The grants are
// re-read rather than carried forward, so what is deployed comes from the table and not
// from what the caller happened to be holding.
func applyDeployedGrants(
	ctx context.Context,
	t *testing.T,
	applier *dataset.PolicyApplier,
	grants *dataset.GrantStore,
	c *dataset.Copy,
) {
	t.Helper()

	deployed, err := grants.GrantsOfCopy(ctx, c)
	if err != nil {
		t.Fatalf("reading the grants against copy %s: %v", c.CopyID, err)
	}
	if _, err := applier.Apply(ctx, datasetPolicyTarget(c, deployed)); err != nil {
		t.Fatalf("applying the policies of copy %s: %v", c.CopyID, err)
	}
}

// createReleaseTable creates the ledger table with the shareId partition key and the two
// indexes the two stores query: the per-researcher index shared with the other mechanism,
// and the per-dataset index only this mechanism's records populate.
//
// The index the htsget authorization callout queries is deliberately absent. Neither a copy
// nor a grant writes a VPC ARN attribute, so neither ever appears there, and a table
// without it is a truer stand-in than one carrying an index nothing here is ever in.
func createReleaseTable(ctx context.Context, t *testing.T, client *dynamodb.Client) {
	t.Helper()

	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(releaseTableName),
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("shareId"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("researcherAccountId"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("datasetUri"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("createdAt"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("shareId"), KeyType: ddbtypes.KeyTypeHash},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String(dataset.ResearcherIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("researcherAccountId"), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("createdAt"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
			{
				IndexName: aws.String(dataset.DatasetIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("datasetUri"), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("createdAt"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable %s: %v", releaseTableName, err)
	}

	waiter := dynamodb.NewTableExistsWaiter(client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(releaseTableName)}, time.Minute); err != nil {
		t.Fatalf("waiting for %s to become active: %v", releaseTableName, err)
	}
}

// createHotCacheBucket creates the bucket holding the dataset's copy, which is the bucket
// whose policy the grant is written into.
func createHotCacheBucket(ctx context.Context, t *testing.T, client *s3.Client) {
	t.Helper()

	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(hotCacheBucketName),
	})
	if err != nil {
		t.Fatalf("CreateBucket %s: %v", hotCacheBucketName, err)
	}
}

// requireLedgerItem fetches a copy's or a grant's item by identifier directly, failing when
// it is absent. Both kinds live under the same partition key, so one read serves for
// either. The stage names the moment in the lifecycle the read belongs to, so a failure
// says which of the four steps left the ledger wrong.
func requireLedgerItem(
	ctx context.Context,
	t *testing.T,
	client *dynamodb.Client,
	recordID string,
	stage string,
) map[string]ddbtypes.AttributeValue {
	t.Helper()

	out, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(releaseTableName),
		Key: map[string]ddbtypes.AttributeValue{
			"shareId": &ddbtypes.AttributeValueMemberS{Value: recordID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("GetItem %s (%s): %v", recordID, stage, err)
	}
	if out.Item == nil {
		t.Fatalf("record %s has no ledger item %s", recordID, stage)
	}
	return out.Item
}

// deployedPolicy is a policy document as it is read back out of the emulator.
//
// The fields that may be a single value or a list are kept as raw JSON and widened on
// demand: MinIO parses what it is given and re-emits its own spelling of it, so anything
// comparing the deployed document to the rendered one as text would be comparing
// formatting rather than access.
type deployedPolicy struct {
	Version   string              `json:"Version"`
	Statement []deployedStatement `json:"Statement"`
}

type deployedStatement struct {
	Sid       string                                `json:"Sid"`
	Effect    string                                `json:"Effect"`
	Principal json.RawMessage                       `json:"Principal"`
	Action    json.RawMessage                       `json:"Action"`
	Resource  json.RawMessage                       `json:"Resource"`
	Condition map[string]map[string]json.RawMessage `json:"Condition"`
}

// deployedBucketPolicy reads the policy in force on a bucket back out of the client the
// applier wrote it through.
func deployedBucketPolicy(
	ctx context.Context,
	t *testing.T,
	client *bucketPolicyHoldingS3,
	bucketName string,
) deployedPolicy {
	t.Helper()

	out, err := client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("GetBucketPolicy %s: %v", bucketName, err)
	}
	if out.Policy == nil {
		t.Fatalf("bucket %s has no policy deployed", bucketName)
	}
	return parsePolicy(t, []byte(*out.Policy), "the policy of bucket "+bucketName)
}

// bucketPolicyHoldingS3 is the S3 emulator with the two bucket-policy calls answered here
// instead of by it. Every other call — the manifest object, the buckets themselves — goes
// straight through.
//
// MinIO refuses a granting document outright, with "invalid condition key
// 'aws:SourceVpc'". That is not a defect in the rendered policy: a grant is bounded to one
// VPC in the researcher's account, and MinIO has no VPCs for a request to arrive from, so
// the key it does not recognise is the one the grant most depends on. It accepts the
// baseline document a copy with no deployed grant renders, which carries no such condition
// — so with the emulator answering, only the two steps that grant nothing could be applied
// at all.
//
// What is lost is a second opinion on whether the document would be accepted, from a
// service not in a position to give one. What is kept is that the document is the
// renderer's own, that the applier writes it through the same interface the command hands
// it, and that the assertions read it back from where it was put. The bucket is still
// confirmed to exist through the emulator, so a policy written for a bucket nobody
// deployed fails here as it would in service.
type bucketPolicyHoldingS3 struct {
	*s3.Client

	// documents holds the last policy put for each bucket, which is the whole of what a
	// bucket policy is: a rendered document replaces the deployed one entire.
	documents map[string]string
}

func newBucketPolicyHoldingS3(client *s3.Client) *bucketPolicyHoldingS3 {
	return &bucketPolicyHoldingS3{Client: client, documents: map[string]string{}}
}

func (c *bucketPolicyHoldingS3) PutBucketPolicy(
	ctx context.Context,
	params *s3.PutBucketPolicyInput,
	optFns ...func(*s3.Options),
) (*s3.PutBucketPolicyOutput, error) {
	if _, err := c.Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: params.Bucket}, optFns...); err != nil {
		return nil, err
	}
	c.documents[aws.ToString(params.Bucket)] = aws.ToString(params.Policy)
	return &s3.PutBucketPolicyOutput{}, nil
}

func (c *bucketPolicyHoldingS3) GetBucketPolicy(
	ctx context.Context,
	params *s3.GetBucketPolicyInput,
	optFns ...func(*s3.Options),
) (*s3.GetBucketPolicyOutput, error) {
	document, deployed := c.documents[aws.ToString(params.Bucket)]
	if !deployed {
		return nil, fmt.Errorf("no bucket policy has been put for %q", aws.ToString(params.Bucket))
	}
	return &s3.GetBucketPolicyOutput{Policy: aws.String(document)}, nil
}

// keyPolicyPut returns the nth key policy the applier wrote.
//
// It comes from the double rather than from a service, for the reason given at the top of
// this file, but the document itself is the renderer's own output: what is lost is only
// whether KMS would have accepted it.
func keyPolicyPut(t *testing.T, keys *awstest.KMS, n int) deployedPolicy {
	t.Helper()

	if len(keys.PutKeyPolicyCalls) <= n {
		t.Fatalf("the key policy was written %d time(s), want at least %d", len(keys.PutKeyPolicyCalls), n+1)
	}
	call := keys.PutKeyPolicyCalls[n]
	if call.KeyId == nil || *call.KeyId != releaseKeyArn {
		t.Fatalf("key policy %d was written to %v, want %s", n, call.KeyId, releaseKeyArn)
	}
	if call.Policy == nil {
		t.Fatalf("key policy %d was written with no document", n)
	}
	return parsePolicy(t, []byte(*call.Policy), "the key policy")
}

func parsePolicy(t *testing.T, document []byte, what string) deployedPolicy {
	t.Helper()

	var parsed deployedPolicy
	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatalf("parsing %s: %v\n%s", what, err, document)
	}
	return parsed
}

// findStatement returns the statement carrying the given identifier. Statements are
// matched by identifier because marshalling and the emulator are both free to reorder
// them.
func findStatement(doc deployedPolicy, sid string) (deployedStatement, bool) {
	for _, statement := range doc.Statement {
		if statement.Sid == sid {
			return statement, true
		}
	}
	return deployedStatement{}, false
}

// requireStatement returns the statement carrying the given identifier, failing when it
// is absent.
func requireStatement(t *testing.T, doc deployedPolicy, sid string, what string) deployedStatement {
	t.Helper()

	statement, found := findStatement(doc, sid)
	if !found {
		t.Fatalf("%s carries no statement %q", what, sid)
	}
	return statement
}

// statementsOfGrant returns the statements one grant contributes to a document.
//
// They are matched by the grant identifier their own identifiers carry, which is how the
// deployed documents are actually keyed: several grants share one pair of documents, and
// each names its statements after itself so that withdrawing one can be told from
// withdrawing another. The prefixes those identifiers begin with are the renderer's
// business, so nothing here repeats them.
func statementsOfGrant(doc deployedPolicy, grantID string) []deployedStatement {
	var found []deployedStatement
	for _, statement := range doc.Statement {
		if strings.Contains(statement.Sid, grantID) {
			found = append(found, statement)
		}
	}
	return found
}

// requireGrantStatement returns the one statement a grant contributes with the given
// effect, failing when there is not exactly one. Each grant writes a single allow and a
// single deny per document, so a second of either is a statement that would widen or
// narrow the access with nothing in the ledger behind it.
func requireGrantStatement(t *testing.T, doc deployedPolicy, grantID, effect, what string) deployedStatement {
	t.Helper()

	var matched []deployedStatement
	for _, statement := range statementsOfGrant(doc, grantID) {
		if statement.Effect == effect {
			matched = append(matched, statement)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("%s carries %d %s statement(s) for grant %s, want exactly one",
			what, len(matched), effect, grantID)
	}
	return matched[0]
}

// principalARNs returns the principals a statement addresses, and nothing for the
// statement addressed to everyone: that one is about how a request travelled rather than
// about who made it, so reading it as naming an account would be wrong.
func principalARNs(t *testing.T, statement deployedStatement) []string {
	t.Helper()

	if len(statement.Principal) == 0 {
		return nil
	}

	var everyone string
	if err := json.Unmarshal(statement.Principal, &everyone); err == nil {
		return nil
	}

	var byKind map[string]json.RawMessage
	if err := json.Unmarshal(statement.Principal, &byKind); err != nil {
		t.Fatalf("the principal of statement %q is neither a string nor an object: %v", statement.Sid, err)
	}
	return jsonStrings(t, byKind["AWS"])
}

// soleConditionValue returns the single operand of one condition, failing when the
// condition is absent or carries more than one value. Every condition these documents
// render compares against exactly one value, and a second would widen or narrow the grant
// invisibly.
func soleConditionValue(t *testing.T, statement deployedStatement, operator, conditionKey string) string {
	t.Helper()

	operands := jsonStrings(t, statement.Condition[operator][conditionKey])
	if len(operands) != 1 {
		t.Fatalf("statement %q compares %s %s against %v, want exactly one value",
			statement.Sid, operator, conditionKey, operands)
	}
	return operands[0]
}

// jsonStrings reads a policy value that may be written either as one string or as a list
// of them, which is a choice the renderer and the emulator make independently.
func jsonStrings(t *testing.T, raw json.RawMessage) []string {
	t.Helper()

	if len(raw) == 0 {
		return nil
	}

	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		t.Fatalf("policy value %s is neither a string nor a list of strings: %v", raw, err)
	}
	return many
}

// readDatasetManifest reads and parses the document published to a researcher account.
func readDatasetManifest(ctx context.Context, t *testing.T, client *s3.Client, key string) dataset.Manifest {
	t.Helper()

	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject s3://%s/%s: %v", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading s3://%s/%s: %v", bucket, key, err)
	}

	var published dataset.Manifest
	if err := json.Unmarshal(body, &published); err != nil {
		t.Fatalf("parsing the manifest at s3://%s/%s: %v\n%s", bucket, key, err, body)
	}
	return published
}
