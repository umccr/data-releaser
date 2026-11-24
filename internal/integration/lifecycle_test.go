//go:build integration

// Package integration exercises the full share -> revoke -> manifest lifecycle
// against real service emulators (DynamoDB Local + an S3 emulator via MinIO)
// managed by testcontainers-go. It verifies both the persisted DynamoDB item
// state and the S3 manifest object presence/absence at each step.
//
// Run with: go test -tags=integration ./internal/integration/...
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/umccr/data-releaser/internal/auth"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
)

const (
	// DynamoDB GSI names must match the constants the store queries against
	// (internal/auth.GSI2IndexName == "GSI2") and the CDK stack (GSI1/GSI2).
	gsi1Name = "GSI1"
	gsi2Name = "GSI2"

	tableName = "data-releaser-ShareRecords"
	bucket    = "data-control-manifest-bucket"

	minioUser = "minioadmin"
	minioPass = "minioadmin"

	researcherAccountID = "777788889999"
	targetVpcArn        = "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-0abc123"
	htsgetBaseURL       = "https://htsget.example.org"
)

// TestShareRevokeManifestLifecycle drives the real ShareStore and manifest Writer
// against live emulators through the full lifecycle:
//
//  1. share create -> a ShareRecord is persisted in DynamoDB AND a manifest
//     object is written to S3 under manifests/<account-id>/manifest.json.
//  2. revoke      -> the ShareRecord is marked revoked AND, because no active
//     shares remain, the manifest object is deleted from S3.
//
// After each step it asserts the DynamoDB item state and the S3 object
// presence/absence directly (not via the store), so the assertions are
// independent of the code under test.
func TestShareRevokeManifestLifecycle(t *testing.T) {
	// Skip gracefully when Docker is unreachable so the tagged suite does not
	// hard-fail in environments without a container runtime.
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()

	ddbEndpoint := startDynamoDBLocal(ctx, t)
	s3Endpoint := startMinIO(ctx, t)

	// A single AWS config with static credentials. DynamoDB Local ignores
	// credentials; MinIO uses them as the S3 access key / secret, so they must
	// match the MinIO root user/password.
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

	createShareTable(ctx, t, ddbClient)
	createBucket(ctx, t, s3Client)

	// Wire the real components under test.
	store := auth.NewShareStore(ddbClient, tableName)
	writer := manifest.NewWriter(s3Client)
	generator := manifest.NewGenerator(htsgetBaseURL)

	manifestCfg := config.ManifestConfig{
		Bucket:      bucket,
		KeyTemplate: config.DefaultManifestKeyTemplate,
	}
	manifestKey, err := manifestCfg.RenderManifestKey(researcherAccountID)
	if err != nil {
		t.Fatalf("rendering manifest key: %v", err)
	}
	if manifestKey != "manifests/777788889999/manifest.json" {
		t.Fatalf("unexpected manifest key: %q", manifestKey)
	}

	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	record, reused, err := store.CreateIdempotent(ctx, auth.CreateInput{
		ResearcherAccountID: researcherAccountID,
		TargetVpcArn:        targetVpcArn,
		LibraryIDs:          []string{"L001", "L002"},
		S3Prefixes:          []string{"s3://data-bucket/L001/", "s3://data-bucket/L002/"},
		ExpiresAt:           &expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateIdempotent: %v", err)
	}
	if reused {
		t.Fatal("expected a freshly created share, but an existing one was reused")
	}
	if record.ShareID == "" {
		t.Fatal("created share has empty ShareID")
	}

	// Regenerate + sync the manifest exactly as the share command does.
	syncManifest(ctx, t, store, generator, writer, manifestCfg)

	// Assert DynamoDB item: present and NOT revoked.
	item := getItem(ctx, t, ddbClient, record.ShareID)
	if item == nil {
		t.Fatal("expected ShareRecord to be persisted in DynamoDB after create")
	}
	if _, revoked := item["revokedAt"]; revoked {
		t.Fatal("newly created share must not have a revokedAt attribute")
	}
	if got := stringAttr(t, item, "researcherAccountId"); got != researcherAccountID {
		t.Fatalf("researcherAccountId: got %q, want %q", got, researcherAccountID)
	}

	// Assert S3: manifest object exists under the per-account key.
	if !objectExists(ctx, t, s3Client, manifestKey) {
		t.Fatalf("expected manifest object s3://%s/%s to exist after create", bucket, manifestKey)
	}

	revoked, err := store.Revoke(ctx, record.ShareID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoked share must have RevokedAt set")
	}

	// No active shares remain -> manifest should be deleted.
	syncManifest(ctx, t, store, generator, writer, manifestCfg)

	// Assert DynamoDB item: still present, now marked revoked.
	item = getItem(ctx, t, ddbClient, record.ShareID)
	if item == nil {
		t.Fatal("revoked ShareRecord must remain visible in DynamoDB (no TTL)")
	}
	if _, ok := item["revokedAt"]; !ok {
		t.Fatal("expected revokedAt attribute to be set after revoke")
	}
	if got := stringAttr(t, item, "revokedAt"); got == "" {
		t.Fatal("revokedAt attribute is empty after revoke")
	}

	// Assert S3: manifest object has been deleted.
	if objectExists(ctx, t, s3Client, manifestKey) {
		t.Fatalf("expected manifest object s3://%s/%s to be deleted after revoke", bucket, manifestKey)
	}
}

// syncManifest lists the researcher's shares, regenerates the manifest from the
// active ones, and writes or deletes it in S3 via the Writer.Sync path — the
// same sequence the CLI performs after a create/revoke.
func syncManifest(
	ctx context.Context,
	t *testing.T,
	store *auth.ShareStore,
	generator *manifest.Generator,
	writer *manifest.Writer,
	cfg config.ManifestConfig,
) {
	t.Helper()

	shares, err := store.ListByAccount(ctx, researcherAccountID)
	if err != nil {
		t.Fatalf("ListByAccount: %v", err)
	}

	values := make([]model.ShareRecord, 0, len(shares))
	for _, s := range shares {
		values = append(values, *s)
	}

	m := generator.Generate(values, time.Now().UTC())

	key, err := cfg.RenderManifestKey(researcherAccountID)
	if err != nil {
		t.Fatalf("RenderManifestKey: %v", err)
	}
	if err := writer.Sync(ctx, cfg.Bucket, key, m); err != nil {
		t.Fatalf("manifest Sync: %v", err)
	}
}

// startDynamoDBLocal launches amazon/dynamodb-local and returns its HTTP endpoint.
func startDynamoDBLocal(ctx context.Context, t *testing.T) string {
	t.Helper()

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "amazon/dynamodb-local:2.5.2",
			ExposedPorts: []string{"8000/tcp"},
			Cmd:          []string{"-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb"},
			WaitingFor:   wait.ForListeningPort("8000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("starting dynamodb-local: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	endpoint, err := container.PortEndpoint(ctx, "8000/tcp", "http")
	if err != nil {
		t.Fatalf("dynamodb-local endpoint: %v", err)
	}
	return endpoint
}

// startMinIO launches minio/minio and returns its S3 endpoint.
func startMinIO(ctx context.Context, t *testing.T) string {
	t.Helper()

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:RELEASE.2024-10-13T13-34-11Z",
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     minioUser,
				"MINIO_ROOT_PASSWORD": minioPass,
			},
			Cmd: []string{"server", "/data"},
			WaitingFor: wait.ForHTTP("/minio/health/live").
				WithPort("9000/tcp").
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("starting minio: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	endpoint, err := container.PortEndpoint(ctx, "9000/tcp", "http")
	if err != nil {
		t.Fatalf("minio endpoint: %v", err)
	}
	return endpoint
}

// createShareTable creates the share record table with the shareId PK plus the
// two GSIs the store queries (GSI1 by targetVpcArn, GSI2 by researcherAccountId
// + createdAt) and waits for it to become ACTIVE.
func createShareTable(ctx context.Context, t *testing.T, client *dynamodb.Client) {
	t.Helper()

	_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(tableName),
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("shareId"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("targetVpcArn"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("researcherAccountId"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("createdAt"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("shareId"), KeyType: ddbtypes.KeyTypeHash},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String(gsi1Name),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("targetVpcArn"), KeyType: ddbtypes.KeyTypeHash},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
			{
				IndexName: aws.String(gsi2Name),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("researcherAccountId"), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("createdAt"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	waiter := dynamodb.NewTableExistsWaiter(client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(tableName)}, time.Minute); err != nil {
		t.Fatalf("waiting for table to become active: %v", err)
	}
}

// createBucket creates the manifest bucket in the S3 emulator.
func createBucket(ctx context.Context, t *testing.T, client *s3.Client) {
	t.Helper()

	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

// getItem fetches a ShareRecord item by shareId directly, independent of the store.
func getItem(ctx context.Context, t *testing.T, client *dynamodb.Client, shareID string) map[string]ddbtypes.AttributeValue {
	t.Helper()

	out, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]ddbtypes.AttributeValue{
			"shareId": &ddbtypes.AttributeValueMemberS{Value: shareID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	return out.Item
}

// stringAttr extracts an S attribute value, failing the test if absent or wrong type.
func stringAttr(t *testing.T, item map[string]ddbtypes.AttributeValue, name string) string {
	t.Helper()

	v, ok := item[name]
	if !ok {
		return ""
	}
	s, ok := v.(*ddbtypes.AttributeValueMemberS)
	if !ok {
		t.Fatalf("attribute %q is not a string", name)
	}
	return s.Value
}

// objectExists reports whether an object exists at the given key, distinguishing a
// genuine "not found" from any other (unexpected) S3 error.
func objectExists(ctx context.Context, t *testing.T, client *s3.Client, key string) bool {
	t.Helper()

	_, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true
	}

	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return false
	}
	// HeadObject surfaces a 404 as a generic API error in some emulators; treat
	// any not-found-shaped error as absence, but fail loudly on anything else.
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return false
	}
	t.Fatalf("HeadObject %q returned unexpected error: %v", key, err)
	return false
}
