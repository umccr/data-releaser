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

// Writer handles uploading and deleting manifests in the Data Control manifest bucket.
// It uses the Data Control account credentials and performs a same-account write to the
// single manifest bucket, storing each researcher's manifest under a per-account key.
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

// VerifyBucketWritable confirms the configured manifest bucket exists and is accessible
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
// Data Control manifest bucket using PutObject with ContentType "application/json".
func (w *Writer) Upload(ctx context.Context, bucket, key string, manifest *model.Manifest) error {
	return w.uploadJSON(ctx, bucket, key, manifest)
}

// uploadJSON writes any document as indented JSON to the given key. The indentation is
// deliberate: these documents are read by people at least as often as by anything else.
func (w *Writer) uploadJSON(ctx context.Context, bucket, key string, document any) error {
	data, err := json.MarshalIndent(document, "", "  ")
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

// Delete removes the manifest from the manifest bucket at the per-account key.
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
	return SyncDocument(ctx, w, bucket, key, manifest)
}

// SyncDocument uploads document to the given key, or removes whatever is at that key
// when document is nil.
//
// Nil meaning "remove" is the whole lifecycle one of these documents has: it exists
// while there is access to describe and is taken away once there is not, so an account
// whose access has ended is never left holding a description of access it no longer has.
//
// The document type is a parameter because each release mechanism publishes its own
// shape, under its own key prefix, into the same bucket — a researcher holding both
// kinds of access has two documents, and neither mechanism needs to know the other's
// schema to deliver its own. It is a function rather than a method only because Go
// methods cannot take type parameters.
func SyncDocument[T any](ctx context.Context, w *Writer, bucket, key string, document *T) error {
	if document == nil {
		return w.Delete(ctx, bucket, key)
	}
	return w.uploadJSON(ctx, bucket, key, document)
}
