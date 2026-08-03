package auth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"pgregory.net/rapid"

	"github.com/umccr/data-releaser/internal/model"
	"github.com/umccr/data-releaser/internal/testgen"
	"github.com/umccr/data-releaser/internal/warehouse"
)

// This file holds the generative (property-based) tests for the Authorization
// Service, built on pgregory.net/rapid and the shared generators in
// internal/testgen. Each test names the correctness property from design.md it
// validates.

// marshalItems marshals a set of ShareRecords into the DynamoDB item shape the
// callout handler unmarshals, reusing the package's own marshaller so the round
// trip matches production exactly.
func marshalItems(t *rapid.T, records []model.ShareRecord) []map[string]types.AttributeValue {
	items := make([]map[string]types.AttributeValue, 0, len(records))
	for i := range records {
		item, err := marshalShareRecord(&records[i])
		if err != nil {
			t.Fatalf("marshaling generated record: %v", err)
		}
		items = append(items, item)
	}
	return items
}

// activePrefixSet returns the union of S3 prefixes across the records that are
// effectively active at now.
func activePrefixSet(records []model.ShareRecord, now time.Time) map[string]bool {
	set := make(map[string]bool)
	for i := range records {
		if records[i].EffectiveStatus(now) != model.ShareStatusActive {
			continue
		}
		for _, p := range records[i].S3Prefixes {
			set[p] = true
		}
	}
	return set
}

func parseMaxAge(t *rapid.T, cacheControl string) int64 {
	// Allow responses use "s-maxage=<n>, public"; deny uses "max-age=<n>".
	const sMaxPrefix = "s-maxage="
	const maxPrefix = "max-age="

	var numStr string
	if strings.HasPrefix(cacheControl, sMaxPrefix) {
		// Extract the numeric portion from "s-maxage=<n>, public"
		after := strings.TrimPrefix(cacheControl, sMaxPrefix)
		parts := strings.SplitN(after, ",", 2)
		numStr = strings.TrimSpace(parts[0])
	} else if strings.HasPrefix(cacheControl, maxPrefix) {
		numStr = strings.TrimPrefix(cacheControl, maxPrefix)
	} else if cacheControl == "no-store" {
		// no-store has no numeric age; treat as 0.
		return 0
	} else {
		t.Fatalf("unexpected Cache-Control %q, want s-maxage=<n>, public or max-age=<n>", cacheControl)
		return 0
	}
	secs, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		t.Fatalf("parsing max-age from %q: %v", cacheControl, err)
	}
	return secs
}

// TestProperty1_FailClosed validates Property 1 (fail-closed): no data is served
// unless an active, non-expired, non-revoked Share_Record authorizes the request;
// any error, malformed response, or absent record yields denial, never access.
//
// Validates: Requirements 4.5, 4.6, 13.10
func TestProperty1_FailClosed(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		now := time.Now().UTC()
		accountID := testgen.AccountID(t)
		vpcArn := testgen.VPCArnForAccount(accountID)
		records := testgen.ShareRecords(t, now, accountID, vpcArn, 6)

		mode := rapid.SampledFrom([]string{"normal", "error", "malformed"}).Draw(t, "mode")

		mock := newMockDynamoDBClient()
		switch mode {
		case "error":
			mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return nil, fmt.Errorf("dynamodb unreachable")
			}
		case "malformed":
			mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{
					{
						"shareId":             &types.AttributeValueMemberS{Value: "01MALFORMED0000000000000A"},
						"researcherAccountId": &types.AttributeValueMemberS{Value: accountID},
						"targetVpcArn":        &types.AttributeValueMemberS{Value: vpcArn},
						// createdAt is unparseable -> unmarshal fails -> deny.
						"createdAt": &types.AttributeValueMemberS{Value: "not-a-timestamp"},
					},
				}}, nil
			}
		default: // "normal"
			items := marshalItems(t, records)
			mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return &dynamodb.QueryOutput{Items: items}, nil
			}
		}

		handler := NewCalloutHandler(mock, "ShareRecords")
		result, err := handler.HandleCalloutWithHeaders(context.Background(), vpcArn, time.Hour, 60*time.Second)

		if result == nil || result.Response == nil {
			t.Fatalf("expected a non-nil result even on failure")
		}

		if mode != "normal" {
			// Fail-closed: any error or malformed data must deny.
			if len(result.Response.HtsgetAuth) != 0 {
				t.Fatalf("mode=%s: expected deny (empty htsgetAuth), got %d entries", mode, len(result.Response.HtsgetAuth))
			}
			if mode == "error" && err == nil {
				t.Fatalf("mode=error: expected a surfaced error alongside the deny")
			}
			return
		}

		// Normal mode: allow only where an active record authorizes it.
		allowed := activePrefixSet(records, now)
		wantAllow := len(allowed) > 0
		gotAllow := len(result.Response.HtsgetAuth) > 0

		if gotAllow != wantAllow {
			t.Fatalf("allow decision mismatch: got %d locations, want allow=%v (active prefixes=%d)",
				len(result.Response.HtsgetAuth), wantAllow, len(allowed))
		}

		// Every returned location must be backed by an active record's prefix.
		for _, loc := range result.Response.HtsgetAuth {
			if !allowed[loc.Location.Prefix] {
				t.Fatalf("returned location %q is not authorized by any active record (fail-closed violation)", loc.Location.Prefix)
			}
		}
	})
}

// TestProperty2_PerResearcherIsolation validates Property 2: an authorization
// decision is never shared across Researcher_Accounts. The callout keys on the
// requester identity (targetVpcArn / GSI1), so one researcher can never receive
// another's locations.
//
// Validates: Requirements 13.3, 13.9
func TestProperty2_PerResearcherIsolation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		now := time.Now().UTC()
		accountA, accountB := testgen.DistinctAccountIDs(t)
		vpcA := testgen.VPCArnForAccount(accountA)
		vpcB := testgen.VPCArnForAccount(accountB)

		recordsA := testgen.ShareRecords(t, now, accountA, vpcA, 4)
		recordsB := testgen.ShareRecords(t, now, accountB, vpcB, 4)

		// Tag every prefix with its owning account so cross-account leakage is
		// detectable regardless of any random collision.
		tag := func(records []model.ShareRecord, account string) {
			for i := range records {
				tagged := make([]string, len(records[i].S3Prefixes))
				for j := range records[i].S3Prefixes {
					tagged[j] = fmt.Sprintf("s3://acct-%s/%d/%d/", account, i, j)
				}
				// Guarantee at least one prefix so active records surface.
				if len(tagged) == 0 {
					tagged = []string{fmt.Sprintf("s3://acct-%s/%d/only/", account, i)}
				}
				records[i].S3Prefixes = tagged
			}
		}
		tag(recordsA, accountA)
		tag(recordsB, accountB)

		// Build one item table for both accounts; the mock filters by the queried
		// targetVpcArn, exactly as GSI1 does.
		all := append(append([]model.ShareRecord{}, recordsA...), recordsB...)
		items := marshalItems(t, all)

		mock := newMockDynamoDBClient()
		mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			var wantArn string
			if v, ok := params.ExpressionAttributeValues[":vpcArn"]; ok {
				if s, ok := v.(*types.AttributeValueMemberS); ok {
					wantArn = s.Value
				}
			}
			var out []map[string]types.AttributeValue
			for _, item := range items {
				if s, ok := item["targetVpcArn"].(*types.AttributeValueMemberS); ok && s.Value == wantArn {
					out = append(out, item)
				}
			}
			return &dynamodb.QueryOutput{Items: out}, nil
		}

		handler := NewCalloutHandler(mock, "ShareRecords")

		bTag := fmt.Sprintf("s3://acct-%s/", accountB)
		aTag := fmt.Sprintf("s3://acct-%s/", accountA)

		// A's decision must never include any of B's locations.
		resA, err := handler.HandleCallout(context.Background(), vpcA)
		if err != nil {
			t.Fatalf("unexpected error for account A: %v", err)
		}
		for _, loc := range resA.HtsgetAuth {
			if strings.HasPrefix(loc.Location.Prefix, bTag) {
				t.Fatalf("account A received account B's location %q (isolation violation)", loc.Location.Prefix)
			}
		}

		// And symmetrically for B.
		resB, err := handler.HandleCallout(context.Background(), vpcB)
		if err != nil {
			t.Fatalf("unexpected error for account B: %v", err)
		}
		for _, loc := range resB.HtsgetAuth {
			if strings.HasPrefix(loc.Location.Prefix, aTag) {
				t.Fatalf("account B received account A's location %q (isolation violation)", loc.Location.Prefix)
			}
		}
	})
}

// TestProperty3_CachesNeverWidenAccess validates Property 3 on the Go-side
// decision/Cache-Control logic: a cache may only make a decision staler, never
// turn a deny into an allow, and correctness holds with caches removed.
//
// The Cache-Control max-age computed by the handler bounds how long any decision
// may be reused: a deny is bounded by the (short) deny TTL and an allow never
// exceeds the ceiling, so no cached entry can outlive or widen the fresh decision.
// The handler itself is cacheless — every call recomputes from the records — so
// the fresh decision is deterministic (correctness with caches removed).
//
// Note: the Rust in-process cache portion of this property is covered by task 15.6.
//
// Validates: Requirements 13.10
func TestProperty3_CachesNeverWidenAccess(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		now := time.Now().UTC()
		ceiling := time.Duration(rapid.Int64Range(1, 24).Draw(t, "ceilingHours")) * time.Hour
		denyTTL := time.Duration(rapid.Int64Range(1, 300).Draw(t, "denyTTLSeconds")) * time.Second

		// Draw 0..5 active records (0 -> deny path).
		accountID := testgen.AccountID(t)
		vpcArn := testgen.VPCArnForAccount(accountID)
		n := rapid.IntRange(0, 5).Draw(t, "numActive")
		var records []*model.ShareRecord
		for i := 0; i < n; i++ {
			kind := rapid.SampledFrom([]testgen.ShareKind{
				testgen.KindActiveLimited, testgen.KindActiveUnlimited,
			}).Draw(t, "activeKind")
			rec := testgen.ShareRecordOfKind(t, now, accountID, vpcArn, kind)
			records = append(records, &rec)
		}

		cc := ComputeCacheControl(records, now, ceiling, denyTTL)
		maxAge := parseMaxAge(t, cc)

		if maxAge < 0 {
			t.Fatalf("max-age must never be negative, got %d", maxAge)
		}

		if len(records) == 0 {
			// Deny: bounded by the short deny TTL. Because it is finite the cache
			// re-issues a fresh callout after it lapses; a deny never persists as
			// an allow.
			wantDeny := int64(denyTTL.Seconds())
			if maxAge != wantDeny {
				t.Fatalf("deny max-age must equal deny TTL %d, got %d", wantDeny, maxAge)
			}
			return
		}

		// Allow: never cacheable beyond the ceiling, so staleness is bounded.
		if maxAge > int64(ceiling.Seconds()) {
			t.Fatalf("allow max-age %d exceeds ceiling %d (unbounded staleness)", maxAge, int64(ceiling.Seconds()))
		}

		// Cache-removed correctness: recomputing yields an identical bound.
		if again := ComputeCacheControl(records, now, ceiling, denyTTL); again != cc {
			t.Fatalf("decision is not deterministic without a cache: %q vs %q", cc, again)
		}
	})
}

// TestProperty4_NoPartialShareEndToEnd validates Property 4 across the share
// pipeline: failures at the preflight, resolution, or archived-file stages leave
// no Share_Record persisted. This drives the real warehouse.ValidateResolution
// logic within the same preflight -> resolve -> validate -> create ordering the
// share command uses, and asserts nothing is written on any failure path.
//
// This covers tasks 3.4 and 9.2.
//
// Validates: Requirements 1.6, 1.7, 1.8
func TestProperty4_NoPartialShareEndToEnd(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		account := testgen.AccountID(t)
		vpcArn := testgen.VPCArnForAccount(account)
		requested := testgen.LibraryIDs(t)

		preflightOK := rapid.Bool().Draw(t, "preflightOK")

		// Build the resolution result, injecting missing and archived libraries.
		resolved, anyProblem := buildResolution(t, requested)

		mock := newMockDynamoDBClient()
		store := NewShareStore(mock, "ShareRecords")

		rec, err := runSharePipeline(context.Background(), store, account, vpcArn, requested, resolved, preflightOK)

		persisted := len(mock.items) > 0
		shouldFail := !preflightOK || anyProblem

		if shouldFail {
			if err == nil {
				t.Fatalf("expected pipeline failure (preflightOK=%v, anyProblem=%v), got nil error", preflightOK, anyProblem)
			}
			if persisted {
				t.Fatalf("partial share persisted despite failure (preflightOK=%v, anyProblem=%v)", preflightOK, anyProblem)
			}
			if rec != nil {
				t.Fatalf("expected no share record on failure, got %+v", rec)
			}
			return
		}

		if err != nil {
			t.Fatalf("unexpected pipeline failure on the happy path: %v", err)
		}
		if !persisted {
			t.Fatalf("expected exactly one persisted share on success, got none")
		}
		if len(mock.items) != 1 {
			t.Fatalf("expected exactly one persisted share on success, got %d", len(mock.items))
		}
	})
}

// buildResolution assigns each requested library a resolution outcome (resolved,
// missing, or archived) and returns the resolved rows plus whether any problem
// (missing or archived) was injected.
func buildResolution(t *rapid.T, requested []string) ([]model.ResolvedLibrary, bool) {
	var resolved []model.ResolvedLibrary
	anyProblem := false
	for i, id := range requested {
		kind := rapid.SampledFrom([]string{"ok", "missing", "archived"}).Draw(t, "resolveKind")
		switch kind {
		case "missing":
			anyProblem = true // no row emitted for this library
		case "archived":
			anyProblem = true
			resolved = append(resolved, model.ResolvedLibrary{
				LibraryID:    id,
				PortalRunID:  fmt.Sprintf("run-%d", i),
				Bucket:       "bucket",
				Key:          fmt.Sprintf("data/%s/sample.bam", id),
				Format:       "bam",
				StorageClass: "DEEP_ARCHIVE",
			})
		default: // "ok"
			resolved = append(resolved, model.ResolvedLibrary{
				LibraryID:    id,
				PortalRunID:  fmt.Sprintf("run-%d", i),
				Bucket:       "bucket",
				Key:          fmt.Sprintf("data/%s/sample.bam", id),
				Format:       "bam",
				StorageClass: "STANDARD",
			})
		}
	}
	return resolved, anyProblem
}

// runSharePipeline mirrors the ordering of cmd/share.go: connectivity preflight,
// then library resolution, then resolution validation, and only then the
// Share_Record write. Any earlier failure returns before a record is created.
func runSharePipeline(ctx context.Context, store *ShareStore, account, vpcArn string, requested []string, resolved []model.ResolvedLibrary, preflightOK bool) (*model.ShareRecord, error) {
	// Stage 1: connectivity / bucket preflight.
	if !preflightOK {
		return nil, fmt.Errorf("preflight failed: lattice/RAM or manifest bucket unavailable")
	}

	// Stage 2 + 3: resolve + validate (archived / no-shareable-reads).
	if err := warehouse.ValidateResolution(requested, resolved); err != nil {
		return nil, err
	}
	prefixes := warehouse.ExtractPrefixes(resolved)

	// Stage 4: persist the Share_Record (only reached when all stages passed).
	rec, _, err := store.CreateIdempotent(ctx, CreateInput{
		ResearcherAccountID: account,
		TargetVpcArn:        vpcArn,
		LibraryIDs:          requested,
		S3Prefixes:          prefixes,
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// TestProperty10_IdempotentCreation validates Property 10: repeating an identical
// share create yields the same share ID and never a duplicate active grant,
// independent of library-ID ordering.
//
// Validates: Requirements 12.1
func TestProperty10_IdempotentCreation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		now := time.Now().UTC()
		account := testgen.AccountID(t)
		vpcArn := testgen.VPCArnForAccount(account)
		libs := testgen.LibraryIDs(t)
		prefixes := testgen.S3Prefixes(t)

		// The created share must be active for idempotency to apply: either
		// unlimited or a future expiry.
		var expiresAt *time.Time
		if rapid.Bool().Draw(t, "hasExpiry") {
			fut := now.Add(time.Duration(rapid.Int64Range(1, 8760).Draw(t, "expiryHours")) * time.Hour)
			expiresAt = &fut
		}

		mock := newMockDynamoDBClient()
		store := NewShareStore(mock, "ShareRecords")

		input := CreateInput{
			ResearcherAccountID: account,
			TargetVpcArn:        vpcArn,
			LibraryIDs:          libs,
			S3Prefixes:          prefixes,
			ExpiresAt:           expiresAt,
		}

		first, reused, err := store.CreateIdempotent(context.Background(), input)
		if err != nil {
			t.Fatalf("first create failed: %v", err)
		}
		if reused {
			t.Fatalf("first create should not reuse an existing share")
		}
		if len(mock.items) != 1 {
			t.Fatalf("expected exactly one share after first create, got %d", len(mock.items))
		}

		// Second create with the same set but reversed order must be idempotent.
		reordered := make([]string, len(libs))
		for i, l := range libs {
			reordered[len(libs)-1-i] = l
		}
		reorderedInput := input
		reorderedInput.LibraryIDs = reordered

		second, reused2, err := store.CreateIdempotent(context.Background(), reorderedInput)
		if err != nil {
			t.Fatalf("second create failed: %v", err)
		}
		if !reused2 {
			t.Fatalf("second identical create should reuse the existing share")
		}
		if second.ShareID != first.ShareID {
			t.Fatalf("idempotent create returned different share ID: %q vs %q", first.ShareID, second.ShareID)
		}
		if len(mock.items) != 1 {
			t.Fatalf("idempotent create must not add a duplicate active grant, got %d shares", len(mock.items))
		}
	})
}
