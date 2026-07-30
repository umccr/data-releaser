package auth

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/model"
)

// GSI1IndexName is the name of the DynamoDB GSI used for authorization lookups
// by targetVpcArn.
const GSI1IndexName = "GSI1"

// LocationEntry represents a single location in the htsgetAuth response.
type LocationEntry struct {
	// Location contains the S3 prefix identifier and backend configuration.
	Location LocationID `json:"location"`

	// Rules is an optional array of AccessRules. Omitted means unrestricted access.
	Rules []model.AccessRule `json:"rules,omitempty"`
}

// LocationID contains the identifier and backend for a location entry.
type LocationID struct {
	// ID is the S3 prefix for the location.
	ID string `json:"id"`

	// Backend is the backend configuration (empty object for S3).
	Backend map[string]interface{} `json:"backend"`
}

// CalloutResponse represents the htsget-rs authorization callout response.
// It conforms to the htsget-rs authorization response schema (version 1).
type CalloutResponse struct {
	// Version is the schema version (always 1).
	Version int `json:"version"`

	// HtsgetAuth is the array of location entries with S3 prefixes.
	// An empty array means deny (no access).
	HtsgetAuth []LocationEntry `json:"htsgetAuth"`
}

// CalloutHandler handles authorization callouts from htsget-rs.
// It queries DynamoDB for active shares matching the source VPC ARN
// and returns the authorization response.
type CalloutHandler struct {
	client    internalaws.DynamoDBClient
	tableName string
}

// NewCalloutHandler creates a new CalloutHandler with the given DynamoDB client
// and table name.
func NewCalloutHandler(client internalaws.DynamoDBClient, tableName string) *CalloutHandler {
	return &CalloutHandler{
		client:    client,
		tableName: tableName,
	}
}

// CalloutResult contains the authorization callout response and the Cache-Control header value.
type CalloutResult struct {
	// Response is the htsget-rs authorization callout response.
	Response *CalloutResponse

	// CacheControl is the Cache-Control header value to include in the HTTP response.
	CacheControl string

	// Vary is the Vary header value naming every forwarded identity header
	// the decision depends on, so that any cache implementing secondary cache
	// keys can revalidate correctly and so that the fail-closed storage
	// precondition of Req 13.6 is satisfiable (Req 13.3).
	Vary string
}

// HandleCallout processes an authorization callout for the given source VPC ARN.
// It queries GSI1 (targetVpcArn) to find all shares for that VPC ARN, filters
// to active shares (non-expired, non-revoked) at read time, and returns a
// CalloutResponse with location entries (S3 prefixes) from matched shares.
// Returns an empty htsgetAuth array for no matches (deny).
//
// If DynamoDB is unreachable, returns a deny response (empty array) with an error.
//
// Requirements: 4.2, 4.3, 4.4, 2.4, 2.5, 2.6
func (h *CalloutHandler) HandleCallout(ctx context.Context, sourceVpcArn string) (*CalloutResponse, error) {
	result, err := h.HandleCalloutWithHeaders(ctx, sourceVpcArn, time.Hour, 60*time.Second)
	if err != nil {
		return result.Response, err
	}
	return result.Response, nil
}

// VaryIdentityHeaders is the Vary header value the Authorization_Service emits
// on every callout response (Req 13.3). It names the forwarded identity header(s)
// that the authorization decision depends on, so that:
//   - any cache implementing secondary cache keys can revalidate correctly, and
//   - the fail-closed storage precondition of Req 13.6 is satisfiable.
//
// Per-requester isolation itself rests on the declared cache key in htsget-rs
// (Req 14.4), NOT on this header.
const VaryIdentityHeaders = "X-Forwarded-Source-Vpc-Arn"

// HandleCalloutWithHeaders processes an authorization callout for the given source VPC ARN
// and computes the Cache-Control header value based on the matched shares' remaining lifetimes.
//
// Parameters:
//   - ctx: context for the DynamoDB query
//   - sourceVpcArn: the requester's VPC ARN
//   - ceiling: the maximum allowed max-age (TTL ceiling, Req 13.1)
//   - denyTTL: the max-age for deny responses (Req 13.2)
//
// Returns a CalloutResult containing both the response and the Cache-Control header.
//
// Requirements: 4.2, 4.3, 4.4, 2.4, 2.5, 2.6, 13.1, 13.2, 13.3
func (h *CalloutHandler) HandleCalloutWithHeaders(ctx context.Context, sourceVpcArn string, ceiling time.Duration, denyTTL time.Duration) (*CalloutResult, error) {
	now := time.Now().UTC()

	// Query GSI1 for all shares matching the target VPC ARN
	output, err := h.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(h.tableName),
		IndexName:              aws.String(GSI1IndexName),
		KeyConditionExpression: aws.String("targetVpcArn = :vpcArn"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":vpcArn": &types.AttributeValueMemberS{Value: sourceVpcArn},
		},
	})
	if err != nil {
		// DynamoDB unreachable: return deny (empty array) with error indication
		// Requirements: 10.7 - fail-closed, deny on error
		denyResponse := &CalloutResponse{
			Version:    1,
			HtsgetAuth: []LocationEntry{},
		}
		return &CalloutResult{
			Response:     denyResponse,
			CacheControl: ComputeCacheControl(nil, now, ceiling, denyTTL),
			Vary:         VaryIdentityHeaders,
		}, fmt.Errorf("querying shares for VPC ARN %q: %w", sourceVpcArn, err)
	}

	// Unmarshal results into ShareRecords
	records, err := unmarshalShareRecords(output.Items)
	if err != nil {
		// Malformed data: return deny with error
		denyResponse := &CalloutResponse{
			Version:    1,
			HtsgetAuth: []LocationEntry{},
		}
		return &CalloutResult{
			Response:     denyResponse,
			CacheControl: ComputeCacheControl(nil, now, ceiling, denyTTL),
			Vary:         VaryIdentityHeaders,
		}, fmt.Errorf("unmarshaling share records: %w", err)
	}

	// Filter to active shares at read time and collect S3 prefixes
	var locations []LocationEntry
	var activeRecords []*model.ShareRecord
	for _, record := range records {
		// Compute effective status at read time (Req 2.4, 2.5, 2.6)
		status := record.EffectiveStatus(now)
		if status != model.ShareStatusActive {
			continue
		}

		activeRecords = append(activeRecords, record)

		// Each active share contributes its S3 prefixes as location entries
		for _, prefix := range record.S3Prefixes {
			locations = append(locations, LocationEntry{
				Location: LocationID{
					ID:      prefix,
					Backend: map[string]interface{}{},
				},
			})
		}
	}

	// If no active shares, return empty array (deny) - Req 4.4, 2.6
	if locations == nil {
		locations = []LocationEntry{}
	}

	response := &CalloutResponse{
		Version:    1,
		HtsgetAuth: locations,
	}

	cacheControl := ComputeCacheControl(activeRecords, now, ceiling, denyTTL)

	return &CalloutResult{
		Response:     response,
		CacheControl: cacheControl,
		Vary:         VaryIdentityHeaders,
	}, nil
}

// ComputeCacheControl computes the Cache-Control header value for an authorization response.
//
// For allow responses (active shares exist):
//   - Computes remaining lifetime for each share:
//   - Unlimited shares (ExpiresAt == nil): use ceiling as their contribution
//   - Expiring shares: remaining = ExpiresAt - now
//   - Takes min(all remaining lifetimes), then caps at ceiling
//   - Returns "s-maxage=<seconds>, public" so a shared cache is permitted to
//     store and reuse the response even when the callout request carries an
//     Authorization header (RFC 9111 §3.5, Req 13.1).
//
// For deny responses (no active shares):
//   - Returns "max-age=<denyTTL seconds>" for short negative caching (Req 13.2).
//     This uses plain max-age rather than s-maxage so the deny response is also
//     usable by a private cache, and because the deny path carries no credentials
//     concern.
//
// Where no safe lifetime can be determined (should not happen with the above
// logic, but as a fallback):
//   - Returns "no-store" (Req 13.4).
//
// Requirements: 13.1, 13.2, 13.4
func ComputeCacheControl(records []*model.ShareRecord, now time.Time, ceiling time.Duration, denyTTL time.Duration) string {
	if len(records) == 0 {
		// Deny: short max-age for negative caching (Req 13.2)
		seconds := int64(math.Max(0, math.Floor(denyTTL.Seconds())))
		return fmt.Sprintf("max-age=%d", seconds)
	}

	// Allow: compute min(remaining lifetime) capped at ceiling (Req 13.1)
	minRemaining := ceiling
	for _, record := range records {
		var remaining time.Duration
		if record.ExpiresAt == nil {
			// Unlimited shares contribute the ceiling value
			remaining = ceiling
		} else {
			// Expiring shares: remaining = ExpiresAt - now
			remaining = record.ExpiresAt.Sub(now)
			if remaining < 0 {
				remaining = 0
			}
		}
		if remaining < minRemaining {
			minRemaining = remaining
		}
	}

	// Cap at ceiling
	if minRemaining > ceiling {
		minRemaining = ceiling
	}

	// Ensure non-negative
	if minRemaining < 0 {
		minRemaining = 0
	}

	seconds := int64(math.Floor(minRemaining.Seconds()))

	// Emit s-maxage + public so a shared cache (RFC 9111 §3.5) is explicitly
	// permitted to store the response even when the request carries an
	// Authorization header (Req 13.1).
	return fmt.Sprintf("s-maxage=%d, public", seconds)
}
