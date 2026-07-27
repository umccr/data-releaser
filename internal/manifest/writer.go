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

// ManifestKey is the well-known S3 object key where the manifest is stored
// in each researcher's bucket.
const ManifestKey = "data-releaser/manifest.json"

// Writer handles uploading and deleting manifests in researcher S3 buckets.
// It uses the Data Control account credentials; cross-account access relies on
// the researcher bucket policy granting the Data Control account write/delete
// permissions (Req 11.5).
type Writer struct {
	s3Client internalaws.S3Client
}

// NewWriter creates a Writer with the given S3 client.
func NewWriter(s3Client internalaws.S3Client) *Writer {
	return &Writer{
		s3Client: s3Client,
	}
}

// Upload serializes the manifest to JSON and writes it to the well-known key
// in the researcher's S3 bucket using PutObject with ContentType "application/json".
//
// Requirements: 7.2
func (w *Writer) Upload(ctx context.Context, bucket string, manifest *model.Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize manifest: %w", err)
	}

	_, err = w.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(ManifestKey),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to upload manifest to s3://%s/%s: %w", bucket, ManifestKey, err)
	}

	return nil
}

// Delete removes the manifest from the researcher's S3 bucket.
// This is called when no active shares remain for the account (Req 7.4).
//
// Requirements: 7.3, 7.4
func (w *Writer) Delete(ctx context.Context, bucket string) error {
	_, err := w.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(ManifestKey),
	})
	if err != nil {
		return fmt.Errorf("failed to delete manifest from s3://%s/%s: %w", bucket, ManifestKey, err)
	}

	return nil
}

// Sync uploads or deletes the manifest based on whether active shares exist.
// If manifest is nil (no active shares), the manifest is deleted from the bucket.
// If manifest is non-nil, it is uploaded to the well-known key.
//
// Requirements: 7.2, 7.3, 7.4, 11.5
func (w *Writer) Sync(ctx context.Context, bucket string, manifest *model.Manifest) error {
	if manifest == nil {
		return w.Delete(ctx, bucket)
	}
	return w.Upload(ctx, bucket, manifest)
}
