package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/umccr/data-releaser/internal/auth"
	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/testgen"
)

// These tests exercise the command orchestration seams — the auth.ShareStore operations
// and manifest generation that the share/revoke/extend/list commands drive — through the
// same code paths the commands use, backed by an in-memory DynamoDB mock. They avoid any
// live AWS dependency.

const testTable = config.DefaultLedgerTable

// otherAccount is a second valid researcher account used by the state-transition tests.
// Both it and its VPC ARN must be well-formed: ShareStore.Create rejects an ARN that
// could never match an authorization callout.
const otherAccount = "123456789012"

func newStore() (*auth.ShareStore, *awstest.DynamoDB) {
	m := awstest.NewDynamoDB()
	return auth.NewShareStore(m, testTable), m
}

func activeShares(t *testing.T, store *auth.ShareStore, account string, now time.Time) []model.ShareRecord {
	t.Helper()
	all, err := store.ListByAccount(context.Background(), account)
	if err != nil {
		t.Fatalf("ListByAccount: %v", err)
	}
	var active []model.ShareRecord
	for _, s := range all {
		if s.EffectiveStatus(now) == model.ShareStatusActive {
			active = append(active, *s)
		}
	}
	return active
}

// TestShareCreationFlow covers the happy path of the share command: an idempotent create
// persists a share record and the generated manifest reflects it.
func TestShareCreationFlow(t *testing.T) {
	store, m := newStore()
	account := "534840902377"
	expiry := time.Now().UTC().Add(7 * 24 * time.Hour)

	rec, reused, err := store.CreateIdempotent(context.Background(), auth.CreateInput{
		ResearcherAccountID: account,
		TargetVpcArn:        testgen.VPCArnForAccount(account),
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://b/p1/", "s3://b/p2/"},
		ExpiresAt:           &expiry,
	})
	if err != nil {
		t.Fatalf("CreateIdempotent: %v", err)
	}
	if reused {
		t.Error("expected a new share, not a reuse")
	}
	if len(m.Items) != 1 {
		t.Fatalf("expected 1 persisted record, got %d", len(m.Items))
	}

	// Manifest reflects the created share.
	now := time.Now().UTC()
	gen := manifest.NewGenerator("https://htsget.example.com")
	doc := gen.Generate(activeShares(t, store, account, now), now)
	if doc == nil {
		t.Fatal("expected a non-nil manifest for an active share")
	}
	if len(doc.Libraries) != 2 {
		t.Errorf("expected 2 libraries in manifest, got %d", len(doc.Libraries))
	}

	// Idempotent repeat with the same library set (different order) reuses the record.
	rec2, reused2, err := store.CreateIdempotent(context.Background(), auth.CreateInput{
		ResearcherAccountID: account,
		TargetVpcArn:        testgen.VPCArnForAccount(account),
		LibraryIDs:          []string{"L2400002", "L2400001"},
		S3Prefixes:          []string{"s3://b/p2/", "s3://b/p1/"},
		ExpiresAt:           &expiry,
	})
	if err != nil {
		t.Fatalf("CreateIdempotent (repeat): %v", err)
	}
	if !reused2 {
		t.Error("expected the identical create to be reused (idempotent)")
	}
	if rec2.ShareID != rec.ShareID {
		t.Errorf("expected same share ID on reuse, got %q vs %q", rec2.ShareID, rec.ShareID)
	}
	if len(m.Items) != 1 {
		t.Errorf("expected no duplicate record, still 1; got %d", len(m.Items))
	}
}

// TestRevokeTransition covers revoke marking an active share revoked and regenerating the
// manifest (which becomes empty when no active shares remain).
func TestRevokeTransition(t *testing.T) {
	store, _ := newStore()
	account := "534840902377"

	created, err := store.Create(context.Background(), auth.CreateInput{
		ResearcherAccountID: account,
		TargetVpcArn:        testgen.VPCArnForAccount(account),
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://b/p1/"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	revoked, err := store.Revoke(context.Background(), created.ShareID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Error("expected RevokedAt to be set after revoke")
	}
	if got := revoked.EffectiveStatus(time.Now().UTC()); got != model.ShareStatusRevoked {
		t.Errorf("expected status revoked, got %q", got)
	}

	// After revoke, no active shares remain, so the manifest should be deleted (nil).
	now := time.Now().UTC()
	gen := manifest.NewGenerator("https://htsget.example.com")
	if doc := gen.Generate(activeShares(t, store, account, now), now); doc != nil {
		t.Errorf("expected nil manifest when no active shares remain, got %+v", doc)
	}
}

func TestRevokeConflicts(t *testing.T) {
	// Unknown share id.
	store, _ := newStore()
	if _, err := store.Revoke(context.Background(), "does-not-exist"); !errors.Is(err, auth.ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}

	// Already revoked leaves state unchanged (second revoke reports the conflict).
	store2, _ := newStore()
	created, err := store2.Create(context.Background(), auth.CreateInput{
		ResearcherAccountID: otherAccount,
		TargetVpcArn:        testgen.VPCArnForAccount(otherAccount),
		LibraryIDs:          []string{"L1"},
		S3Prefixes:          []string{"s3://b/p/"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store2.Revoke(context.Background(), created.ShareID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if _, err := store2.Revoke(context.Background(), created.ShareID); !errors.Is(err, auth.ErrShareAlreadyRevoked) {
		t.Errorf("expected ErrShareAlreadyRevoked on second revoke, got %v", err)
	}

	// Expired share reports the expired conflict.
	store3, _ := newStore()
	past := time.Now().UTC().Add(-1 * time.Hour)
	expired, err := store3.Create(context.Background(), auth.CreateInput{
		ResearcherAccountID: otherAccount,
		TargetVpcArn:        testgen.VPCArnForAccount(otherAccount),
		LibraryIDs:          []string{"L1"},
		S3Prefixes:          []string{"s3://b/p/"},
		ExpiresAt:           &past,
	})
	if err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	if _, err := store3.Revoke(context.Background(), expired.ShareID); !errors.Is(err, auth.ErrShareExpired) {
		t.Errorf("expected ErrShareExpired, got %v", err)
	}
}

// TestExtendTransition covers extend updating the expiry of an active share.
func TestExtendTransition(t *testing.T) {
	store, _ := newStore()
	initial := time.Now().UTC().Add(24 * time.Hour)
	created, err := store.Create(context.Background(), auth.CreateInput{
		ResearcherAccountID: otherAccount,
		TargetVpcArn:        testgen.VPCArnForAccount(otherAccount),
		LibraryIDs:          []string{"L1"},
		S3Prefixes:          []string{"s3://b/p/"},
		ExpiresAt:           &initial,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := store.Extend(context.Background(), created.ShareID, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if updated.ExpiresAt == nil {
		t.Fatal("expected a non-nil expiry after extend")
	}
	// New expiry should be well beyond the original 24h expiry.
	if !updated.ExpiresAt.After(initial.Add(24 * time.Hour)) {
		t.Errorf("expected extended expiry to move further out, got %v (was %v)", updated.ExpiresAt, initial)
	}
}

func TestExtendConflicts(t *testing.T) {
	// Unknown share id.
	store, _ := newStore()
	if _, err := store.Extend(context.Background(), "nope", 30*24*time.Hour); !errors.Is(err, auth.ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got %v", err)
	}

	// Revoked share cannot be extended.
	store2, _ := newStore()
	created, err := store2.Create(context.Background(), auth.CreateInput{
		ResearcherAccountID: otherAccount,
		TargetVpcArn:        testgen.VPCArnForAccount(otherAccount),
		LibraryIDs:          []string{"L1"},
		S3Prefixes:          []string{"s3://b/p/"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store2.Revoke(context.Background(), created.ShareID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store2.Extend(context.Background(), created.ShareID, 30*24*time.Hour); !errors.Is(err, auth.ErrShareAlreadyRevoked) {
		t.Errorf("expected ErrShareAlreadyRevoked, got %v", err)
	}

	// Expired share cannot be extended.
	store3, _ := newStore()
	past := time.Now().UTC().Add(-1 * time.Hour)
	expired, err := store3.Create(context.Background(), auth.CreateInput{
		ResearcherAccountID: otherAccount,
		TargetVpcArn:        testgen.VPCArnForAccount(otherAccount),
		LibraryIDs:          []string{"L1"},
		S3Prefixes:          []string{"s3://b/p/"},
		ExpiresAt:           &past,
	})
	if err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	if _, err := store3.Extend(context.Background(), expired.ShareID, 30*24*time.Hour); !errors.Is(err, auth.ErrShareExpired) {
		t.Errorf("expected ErrShareExpired, got %v", err)
	}
}
