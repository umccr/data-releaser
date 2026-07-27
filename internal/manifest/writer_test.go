package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/umccr/data-releaser/internal/model"
)

// mockS3Client implements the S3Client interface for testing.
type mockS3Client struct {
	putObjectFn    func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	deleteObjectFn func(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	headObjectFn   func(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)

	// Track calls for assertions
	putCalls    []s3.PutObjectInput
	deleteCalls []s3.DeleteObjectInput
}

func (m *mockS3Client) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.putCalls = append(m.putCalls, *params)
	if m.putObjectFn != nil {
		return m.putObjectFn(ctx, params, optFns...)
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *mockS3Client) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.deleteCalls = append(m.deleteCalls, *params)
	if m.deleteObjectFn != nil {
		return m.deleteObjectFn(ctx, params, optFns...)
	}
	return &s3.DeleteObjectOutput{}, nil
}

func (m *mockS3Client) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headObjectFn != nil {
		return m.headObjectFn(ctx, params, optFns...)
	}
	return &s3.HeadObjectOutput{}, nil
}

func TestWriter_Upload_Success(t *testing.T) {
	mock := &mockS3Client{}
	writer := NewWriter(mock)

	now := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	manifest := &model.Manifest{
		HtsgetBaseUrl: "https://htsget-lattice.data-control.umccr.org",
		GeneratedAt:   now,
		Libraries: []model.ManifestLibrary{
			{
				LibraryID: "L2400001",
				ExpiresAt: &expiry,
				Files: []model.ManifestFile{
					{Request: "reads/L2400001"},
				},
			},
		},
	}

	err := writer.Upload(context.Background(), "researcher-bucket", manifest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 PutObject call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if *call.Bucket != "researcher-bucket" {
		t.Errorf("bucket: expected %q, got %q", "researcher-bucket", *call.Bucket)
	}
	if *call.Key != ManifestKey {
		t.Errorf("key: expected %q, got %q", ManifestKey, *call.Key)
	}
	if *call.ContentType != "application/json" {
		t.Errorf("content-type: expected %q, got %q", "application/json", *call.ContentType)
	}

	// Verify body is valid JSON matching the manifest
	body, err := io.ReadAll(call.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}

	var decoded model.Manifest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if decoded.HtsgetBaseUrl != manifest.HtsgetBaseUrl {
		t.Errorf("body HtsgetBaseUrl mismatch: expected %q, got %q", manifest.HtsgetBaseUrl, decoded.HtsgetBaseUrl)
	}
	if len(decoded.Libraries) != 1 || decoded.Libraries[0].LibraryID != "L2400001" {
		t.Errorf("body libraries mismatch")
	}

	// Verify indented JSON
	if !bytes.Contains(body, []byte("\n")) {
		t.Error("expected indented JSON (contains newlines)")
	}
}

func TestWriter_Upload_S3Error(t *testing.T) {
	mock := &mockS3Client{
		putObjectFn: func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			return nil, errors.New("access denied")
		},
	}
	writer := NewWriter(mock)

	manifest := &model.Manifest{
		HtsgetBaseUrl: "https://htsget.example.com",
		GeneratedAt:   time.Now().UTC(),
		Libraries:     []model.ManifestLibrary{},
	}

	err := writer.Upload(context.Background(), "researcher-bucket", manifest)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errors.Unwrap(err)) {
		// Just check the error message contains useful context
		if got := err.Error(); got == "" {
			t.Error("expected non-empty error message")
		}
	}
}

func TestWriter_Delete_Success(t *testing.T) {
	mock := &mockS3Client{}
	writer := NewWriter(mock)

	err := writer.Delete(context.Background(), "researcher-bucket")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteObject call, got %d", len(mock.deleteCalls))
	}

	call := mock.deleteCalls[0]
	if *call.Bucket != "researcher-bucket" {
		t.Errorf("bucket: expected %q, got %q", "researcher-bucket", *call.Bucket)
	}
	if *call.Key != ManifestKey {
		t.Errorf("key: expected %q, got %q", ManifestKey, *call.Key)
	}
}

func TestWriter_Delete_S3Error(t *testing.T) {
	mock := &mockS3Client{
		deleteObjectFn: func(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
			return nil, errors.New("access denied")
		},
	}
	writer := NewWriter(mock)

	err := writer.Delete(context.Background(), "researcher-bucket")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestWriter_Sync_NilManifest_Deletes(t *testing.T) {
	mock := &mockS3Client{}
	writer := NewWriter(mock)

	err := writer.Sync(context.Background(), "researcher-bucket", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should call DeleteObject, not PutObject
	if len(mock.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteObject call, got %d", len(mock.deleteCalls))
	}
	if len(mock.putCalls) != 0 {
		t.Fatalf("expected 0 PutObject calls, got %d", len(mock.putCalls))
	}
}

func TestWriter_Sync_NonNilManifest_Uploads(t *testing.T) {
	mock := &mockS3Client{}
	writer := NewWriter(mock)

	manifest := &model.Manifest{
		HtsgetBaseUrl: "https://htsget.example.com",
		GeneratedAt:   time.Now().UTC(),
		Libraries: []model.ManifestLibrary{
			{
				LibraryID: "L001",
				Files:     []model.ManifestFile{{Request: "reads/L001"}},
			},
		},
	}

	err := writer.Sync(context.Background(), "researcher-bucket", manifest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should call PutObject, not DeleteObject
	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 PutObject call, got %d", len(mock.putCalls))
	}
	if len(mock.deleteCalls) != 0 {
		t.Fatalf("expected 0 DeleteObject calls, got %d", len(mock.deleteCalls))
	}
}

func TestWriter_Upload_CorrectKey(t *testing.T) {
	// Verify the well-known key path is "data-releaser/manifest.json"
	if ManifestKey != "data-releaser/manifest.json" {
		t.Errorf("ManifestKey: expected %q, got %q", "data-releaser/manifest.json", ManifestKey)
	}
}
