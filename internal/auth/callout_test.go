package auth

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/model"
)

// --- ComputeCacheControl unit tests ---

func TestComputeCacheControl_DenyResponse(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	result := ComputeCacheControl(nil, now, ceiling, denyTTL)
	if result != "max-age=60" {
		t.Errorf("expected max-age=60, got %q", result)
	}
}

func TestComputeCacheControl_DenyResponse_CustomTTL(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 30 * time.Second

	result := ComputeCacheControl(nil, now, ceiling, denyTTL)
	if result != "max-age=30" {
		t.Errorf("expected max-age=30, got %q", result)
	}
}

func TestComputeCacheControl_DenyResponse_EmptyRecords(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	result := ComputeCacheControl([]*model.ShareRecord{}, now, ceiling, denyTTL)
	if result != "max-age=60" {
		t.Errorf("expected max-age=60, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_UnlimitedShares(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	records := []*model.ShareRecord{
		{ExpiresAt: nil}, // unlimited
		{ExpiresAt: nil}, // unlimited
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=3600, public" {
		t.Errorf("expected s-maxage=3600, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_ExpiringShare(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	// Share expires in 30 minutes
	expiry := now.Add(30 * time.Minute)
	records := []*model.ShareRecord{
		{ExpiresAt: &expiry},
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=1800, public" {
		t.Errorf("expected s-maxage=1800, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_MinRemainingLifetime(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	// Two shares: one expires in 10 minutes, another in 45 minutes
	expiry1 := now.Add(10 * time.Minute)
	expiry2 := now.Add(45 * time.Minute)
	records := []*model.ShareRecord{
		{ExpiresAt: &expiry1},
		{ExpiresAt: &expiry2},
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=600, public" {
		t.Errorf("expected s-maxage=600, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_CappedAtCeiling(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	// Share expires in 3 hours, but ceiling is 1 hour
	expiry := now.Add(3 * time.Hour)
	records := []*model.ShareRecord{
		{ExpiresAt: &expiry},
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=3600, public" {
		t.Errorf("expected s-maxage=3600, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_MixedUnlimitedAndExpiring(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	// One unlimited, one expiring in 20 minutes
	expiry := now.Add(20 * time.Minute)
	records := []*model.ShareRecord{
		{ExpiresAt: nil},     // unlimited -> contributes ceiling (1h)
		{ExpiresAt: &expiry}, // 20 minutes
	}

	// min(1h, 20m) = 20m = 1200s
	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=1200, public" {
		t.Errorf("expected s-maxage=1200, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_AlmostExpired(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	// Share expires in 5 seconds
	expiry := now.Add(5 * time.Second)
	records := []*model.ShareRecord{
		{ExpiresAt: &expiry},
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=5, public" {
		t.Errorf("expected s-maxage=5, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_JustExpired(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 1 * time.Hour
	denyTTL := 60 * time.Second

	// Share already expired 1 second ago (but still passed as "active" record)
	expiry := now.Add(-1 * time.Second)
	records := []*model.ShareRecord{
		{ExpiresAt: &expiry},
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=0, public" {
		t.Errorf("expected s-maxage=0, public, got %q", result)
	}
}

func TestComputeCacheControl_AllowResponse_CustomCeiling(t *testing.T) {
	now := time.Now().UTC()
	ceiling := 24 * time.Hour // 24 hour ceiling
	denyTTL := 60 * time.Second

	// Unlimited share with a 24h ceiling
	records := []*model.ShareRecord{
		{ExpiresAt: nil},
	}

	result := ComputeCacheControl(records, now, ceiling, denyTTL)
	if result != "s-maxage=86400, public" {
		t.Errorf("expected s-maxage=86400, public, got %q", result)
	}
}

// --- HandleCalloutWithHeaders integration tests ---

func TestHandleCalloutWithHeaders_DenyOnNoShares(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	// Override queryFn to return empty results (no shares for this VPC ARN)
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{
			Items: nil,
		}, nil
	}

	result, err := handler.HandleCalloutWithHeaders(context.Background(),
		"arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		1*time.Hour, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Response.HtsgetAuth) != 0 {
		t.Errorf("expected empty htsgetAuth (deny), got %d entries", len(result.Response.HtsgetAuth))
	}
	if result.CacheControl != "max-age=60" {
		t.Errorf("expected max-age=60 for deny, got %q", result.CacheControl)
	}
}

func TestHandleCalloutWithHeaders_DenyOnDynamoDBError(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	// Override queryFn to return error (DynamoDB unreachable)
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return nil, fmt.Errorf("dynamodb unavailable")
	}

	result, err := handler.HandleCalloutWithHeaders(context.Background(),
		"arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123",
		1*time.Hour, 60*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if result == nil {
		t.Fatal("expected non-nil result even on error")
	}
	if len(result.Response.HtsgetAuth) != 0 {
		t.Errorf("expected empty htsgetAuth (deny) on error, got %d entries", len(result.Response.HtsgetAuth))
	}
	if result.CacheControl != "max-age=60" {
		t.Errorf("expected max-age=60 for deny on error, got %q", result.CacheControl)
	}
}

func TestHandleCalloutWithHeaders_AllowWithCacheControl(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	// Create a share that expires in 30 minutes
	now := time.Now().UTC()
	expiry := now.Add(30 * time.Minute)
	expiryStr := expiry.Format(time.RFC3339)
	createdAtStr := now.Add(-1 * time.Hour).Format(time.RFC3339)

	vpcArn := "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123"

	// Override queryFn to return a matching share
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		item := map[string]types.AttributeValue{
			"shareId":             &types.AttributeValueMemberS{Value: "01TEST123456789012345678"},
			"researcherAccountId": &types.AttributeValueMemberS{Value: "534840902377"},
			"targetVpcArn":        &types.AttributeValueMemberS{Value: vpcArn},
			"libraryIds":          &types.AttributeValueMemberSS{Value: []string{"L2400001"}},
			"s3Prefixes":          &types.AttributeValueMemberSS{Value: []string{"s3://bucket/prefix1/"}},
			"createdAt":           &types.AttributeValueMemberS{Value: createdAtStr},
			"expiresAt":           &types.AttributeValueMemberS{Value: expiryStr},
		}
		return &dynamodb.QueryOutput{
			Items: []map[string]types.AttributeValue{item},
		}, nil
	}

	result, err := handler.HandleCalloutWithHeaders(context.Background(),
		vpcArn, 1*time.Hour, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be allow (non-empty htsgetAuth)
	if len(result.Response.HtsgetAuth) == 0 {
		t.Error("expected non-empty htsgetAuth for allow response")
	}

	// Cache-Control should be approximately s-maxage=1800, public (30 minutes)
	// Allow a small tolerance since time passes between test setup and computation
	if result.CacheControl == "max-age=60" {
		t.Errorf("expected allow cache-control (not deny TTL), got %q", result.CacheControl)
	}
	if !strings.HasPrefix(result.CacheControl, "s-maxage=") {
		t.Errorf("expected allow cache-control to start with s-maxage=, got %q", result.CacheControl)
	}
	if !strings.HasSuffix(result.CacheControl, ", public") {
		t.Errorf("expected allow cache-control to end with , public, got %q", result.CacheControl)
	}
}

func TestHandleCalloutWithHeaders_AllowUnlimitedShare(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	now := time.Now().UTC()
	createdAtStr := now.Add(-1 * time.Hour).Format(time.RFC3339)

	vpcArn := "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123"

	// Override queryFn to return an unlimited share (no expiresAt)
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		item := map[string]types.AttributeValue{
			"shareId":             &types.AttributeValueMemberS{Value: "01TEST123456789012345678"},
			"researcherAccountId": &types.AttributeValueMemberS{Value: "534840902377"},
			"targetVpcArn":        &types.AttributeValueMemberS{Value: vpcArn},
			"libraryIds":          &types.AttributeValueMemberSS{Value: []string{"L2400001"}},
			"s3Prefixes":          &types.AttributeValueMemberSS{Value: []string{"s3://bucket/prefix1/"}},
			"createdAt":           &types.AttributeValueMemberS{Value: createdAtStr},
			// No expiresAt -> unlimited
		}
		return &dynamodb.QueryOutput{
			Items: []map[string]types.AttributeValue{item},
		}, nil
	}

	result, err := handler.HandleCalloutWithHeaders(context.Background(),
		vpcArn, 1*time.Hour, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be allow
	if len(result.Response.HtsgetAuth) == 0 {
		t.Error("expected non-empty htsgetAuth for allow response")
	}

	// For unlimited share, max-age should equal ceiling (3600)
	if result.CacheControl != "s-maxage=3600, public" {
		t.Errorf("expected s-maxage=3600, public for unlimited share, got %q", result.CacheControl)
	}
}

func TestHandleCalloutWithHeaders_SkipsExpiredShares(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	now := time.Now().UTC()
	pastExpiry := now.Add(-1 * time.Hour)
	pastExpiryStr := pastExpiry.Format(time.RFC3339)
	createdAtStr := now.Add(-2 * time.Hour).Format(time.RFC3339)

	vpcArn := "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123"

	// Override queryFn to return an expired share
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		item := map[string]types.AttributeValue{
			"shareId":             &types.AttributeValueMemberS{Value: "01TEST123456789012345678"},
			"researcherAccountId": &types.AttributeValueMemberS{Value: "534840902377"},
			"targetVpcArn":        &types.AttributeValueMemberS{Value: vpcArn},
			"libraryIds":          &types.AttributeValueMemberSS{Value: []string{"L2400001"}},
			"s3Prefixes":          &types.AttributeValueMemberSS{Value: []string{"s3://bucket/prefix1/"}},
			"createdAt":           &types.AttributeValueMemberS{Value: createdAtStr},
			"expiresAt":           &types.AttributeValueMemberS{Value: pastExpiryStr},
		}
		return &dynamodb.QueryOutput{
			Items: []map[string]types.AttributeValue{item},
		}, nil
	}

	result, err := handler.HandleCalloutWithHeaders(context.Background(),
		vpcArn, 1*time.Hour, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expired share should be skipped -> deny
	if len(result.Response.HtsgetAuth) != 0 {
		t.Errorf("expected empty htsgetAuth (deny) for expired share, got %d entries", len(result.Response.HtsgetAuth))
	}
	if result.CacheControl != "max-age=60" {
		t.Errorf("expected max-age=60 for deny, got %q", result.CacheControl)
	}
}

func TestHandleCalloutWithHeaders_SkipsRevokedShares(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	now := time.Now().UTC()
	revokedAt := now.Add(-30 * time.Minute)
	revokedAtStr := revokedAt.Format(time.RFC3339)
	createdAtStr := now.Add(-2 * time.Hour).Format(time.RFC3339)

	vpcArn := "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123"

	// Override queryFn to return a revoked share
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		item := map[string]types.AttributeValue{
			"shareId":             &types.AttributeValueMemberS{Value: "01TEST123456789012345678"},
			"researcherAccountId": &types.AttributeValueMemberS{Value: "534840902377"},
			"targetVpcArn":        &types.AttributeValueMemberS{Value: vpcArn},
			"libraryIds":          &types.AttributeValueMemberSS{Value: []string{"L2400001"}},
			"s3Prefixes":          &types.AttributeValueMemberSS{Value: []string{"s3://bucket/prefix1/"}},
			"createdAt":           &types.AttributeValueMemberS{Value: createdAtStr},
			"revokedAt":           &types.AttributeValueMemberS{Value: revokedAtStr},
		}
		return &dynamodb.QueryOutput{
			Items: []map[string]types.AttributeValue{item},
		}, nil
	}

	result, err := handler.HandleCalloutWithHeaders(context.Background(),
		vpcArn, 1*time.Hour, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Revoked share should be skipped -> deny
	if len(result.Response.HtsgetAuth) != 0 {
		t.Errorf("expected empty htsgetAuth (deny) for revoked share, got %d entries", len(result.Response.HtsgetAuth))
	}
	if result.CacheControl != "max-age=60" {
		t.Errorf("expected max-age=60 for deny, got %q", result.CacheControl)
	}
}

func TestHandleCallout_BackwardCompatible(t *testing.T) {
	mock := newMockDynamoDBClient()
	handler := NewCalloutHandler(mock, "ShareRecords")

	// Override queryFn to return empty results
	mock.queryFn = func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		return &dynamodb.QueryOutput{
			Items: nil,
		}, nil
	}

	// The original HandleCallout should still work
	response, err := handler.HandleCallout(context.Background(),
		"arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response == nil {
		t.Fatal("expected non-nil response")
	}
	if len(response.HtsgetAuth) != 0 {
		t.Errorf("expected empty htsgetAuth (deny), got %d entries", len(response.HtsgetAuth))
	}
}
