// Package manifest implements manifest generation and S3 delivery for researcher accounts.
package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/model"
)

// Writer handles uploading and deleting manifests in the Data Control Manifest_Bucket.
// It uses the Data Control account credentials and performs a same-account write to the
// single Manifest_Bucket, storing each researcher's manifest under a per-account key.
// Researcher access is read-only and cross-account, granted by the bucket
// policy — the Writer never grants access.
type Writer struct {
	s3Client internalaws.S3Client
}

// NewWriter creates a Writer with the given S3 client.
func NewWriter(s3Client internalaws.S3Client) *Writer {
	return &Writer{
		s3Client: s3Client,
	}
}

// VerifyBucketWritable confirms the configured Manifest_Bucket exists and is accessible
// to the Data Control identity via a HeadBucket call. A HeadBucket success confirms
// existence and basic access; writability is conferred by the same-account identity's
// IAM (the Manifest write is a same-account write). This is used as a share preflight
// check so a share is never persisted only to fail on the subsequent manifest upload.
func (w *Writer) VerifyBucketWritable(ctx context.Context, bucket string) error {
	_, err := w.s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return fmt.Errorf("failed to access manifest bucket %q: %w", bucket, err)
	}
	return nil
}

// Upload serializes the manifest to JSON and writes it to the per-account key in the
// Data Control Manifest_Bucket using PutObject with ContentType "application/json".
func (w *Writer) Upload(ctx context.Context, bucket, key string, manifest *model.Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize manifest: %w", err)
	}

	_, err = w.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to upload manifest to s3://%s/%s: %w", bucket, key, err)
	}

	return nil
}

// Delete removes the manifest from the Manifest_Bucket at the per-account key.
// This is called when no active shares remain for the account.
func (w *Writer) Delete(ctx context.Context, bucket, key string) error {
	_, err := w.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete manifest from s3://%s/%s: %w", bucket, key, err)
	}

	return nil
}

// Sync uploads or deletes the manifest based on whether active shares exist.
// If manifest is nil (no active shares), the manifest is deleted from the bucket.
// If manifest is non-nil, it is uploaded to the per-account key.
func (w *Writer) Sync(ctx context.Context, bucket, key string, manifest *model.Manifest) error {
	if manifest == nil {
		return w.Delete(ctx, bucket, key)
	}
	return w.Upload(ctx, bucket, key, manifest)
}
