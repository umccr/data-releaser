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
//
// This maps onto htsget-rs's `AuthorizationRule`, which accepts exactly two
// fields: `location` and an optional `rules`.
type LocationEntry struct {
	// Location contains the S3 key prefix that the htsget id must start with.
	Location LocationID `json:"location"`

	// Rules is an optional array of AccessRules. Omitted means unrestricted access.
	Rules []model.AccessRule `json:"rules,omitempty"`
}

// LocationID contains the prefix matcher for a location entry.
//
// It deserialises in htsget-rs as a `LocationWrapper::String` carrying only a
// `PrefixOrId::Prefix`. Two deliberate choices:
//
//   - `prefix`, not `id`. htsget-rs matches `id` by equality against the request
//     path (e.g. "demo/L2400001/NA12878"), whereas a share grants access to a
//     directory (e.g. "demo/L2400001/"). Only `prefix` has the right semantics.
//
//   - No backend. Omitting it leaves htsget-rs with a defaulted backend, which
//     `into_remote_locations()` filters out, so htsget-rs resolves the object
//     against its own statically configured location instead. The share decides
//     *what* may be read; the htsget-rs deployment decides *where from*. Emitting
//     a backend here would require this service to know the storage topology and
//     would force htsget-rs to hold broad S3 read permissions.
type LocationID struct {
	// Prefix is the bucket-less S3 key prefix for the location, e.g.
	// "demo/L2400001/". It is matched with `starts_with` against the htsget id.
	Prefix string `json:"prefix"`
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
	// keys can revalidate correctly.
	Vary string
}

// HandleCallout processes an authorization callout for the given source VPC ARN and
// returns only the response body, using the default cache TTLs.
//
// This is a convenience wrapper over HandleCalloutWithHeaders for callers that do not
// care about the cache headers; the deployed Lambda uses the full form so it can emit
// them. Behaviour is otherwise identical: active shares contribute location entries, no
// match yields an empty htsgetAuth array (deny), and an unreachable store yields a deny
// plus an error.
func (h *CalloutHandler) HandleCallout(ctx context.Context, sourceVpcArn string) (*CalloutResponse, error) {
	result, err := h.HandleCalloutWithHeaders(ctx, sourceVpcArn, DefaultCacheTTLCeiling, DefaultCacheDenyTTL)
	return result.Response, err
}

// Default cache TTLs applied when a caller does not supply its own.
const (
	DefaultCacheTTLCeiling = time.Hour
	DefaultCacheDenyTTL    = 60 * time.Second
)

// SourceVpcArnHeader is the request header carrying the requester's source VPC
// ARN on an htsget-rs authorization callout.
//
// htsget-rs forwards configured context values as headers named with its
// `Htsget-Context-` prefix, so a `forward.context.extensions` entry of
// `{ json_path = "$.requestContext.identity.sourceVpcArn", name = "SourceVpcArn" }`
// arrives as this header. htsget-rs issues the callout as a bare GET with
// headers only, so this is the only channel the identity travels over.
const SourceVpcArnHeader = "Htsget-Context-SourceVpcArn"

// VaryIdentityHeaders is the Vary header value the authorization service emits
// on every callout response. It names the forwarded identity header(s)
// that the authorization decision depends on, so that:
//   - any cache implementing secondary cache keys can revalidate correctly, and
//   - the fail-closed storage precondition is satisfiable.
//
// Per-requester isolation itself rests on the declared cache key in htsget-rs, NOT on this header.
const VaryIdentityHeaders = SourceVpcArnHeader

// HandleCalloutWithHeaders processes an authorization callout for the given source VPC ARN
// and computes the Cache-Control header value based on the matched shares' remaining lifetimes.
//
// Parameters:
//   - ctx: context for the DynamoDB query
//   - sourceVpcArn: the requester's VPC ARN
//   - ceiling: the maximum allowed max-age (TTL ceiling)
//   - denyTTL: the max-age for deny responses
//
// Returns a CalloutResult containing both the response and the Cache-Control header.
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
		return denyResult(now, ceiling, denyTTL), fmt.Errorf("querying shares for VPC ARN %q: %w", sourceVpcArn, err)
	}

	// Unmarshal results into ShareRecords
	records, err := unmarshalShareRecords(output.Items)
	if err != nil {
		// Malformed data: return deny with error
		return denyResult(now, ceiling, denyTTL), fmt.Errorf("unmarshaling share records: %w", err)
	}

	// Filter to active shares at read time and collect S3 prefixes.
	// An empty (not nil) slice matters: it serialises as [] rather than null, which is
	// what htsget-rs reads as an explicit deny.
	locations := []LocationEntry{}
	var activeRecords []*model.ShareRecord
	for _, record := range records {
		status := record.EffectiveStatus(now)
		if status != model.ShareStatusActive {
			continue
		}

		activeRecords = append(activeRecords, record)

		// Each active share contributes its S3 prefixes as location entries
		for _, prefix := range record.S3Prefixes {
			locations = append(locations, LocationEntry{
				Location: LocationID{Prefix: prefix},
			})
		}
	}

	return &CalloutResult{
		Response: &CalloutResponse{
			Version:    1,
			HtsgetAuth: locations,
		},
		CacheControl: ComputeCacheControl(activeRecords, now, ceiling, denyTTL),
		Vary:         VaryIdentityHeaders,
	}, nil
}

// denyResult builds the fail-closed deny response returned when the store cannot be
// read. The empty htsgetAuth array is the deny signal, and the short
// negative-cache max-age keeps a backend outage from being cached for long.
func denyResult(now time.Time, ceiling, denyTTL time.Duration) *CalloutResult {
	return &CalloutResult{
		Response: &CalloutResponse{
			Version:    1,
			HtsgetAuth: []LocationEntry{},
		},
		CacheControl: ComputeCacheControl(nil, now, ceiling, denyTTL),
		Vary:         VaryIdentityHeaders,
	}
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
//     Authorization header (RFC 9111 §3.5).
//
// For deny responses (no active shares):
//   - Returns "max-age=<denyTTL seconds>" for short negative caching.
//     This uses plain max-age rather than s-maxage so the deny response is also
//     usable by a private cache, and because the deny path carries no credentials
//     concern.
//
// Where no safe lifetime can be determined (should not happen with the above
// logic, but as a fallback):
//   - Returns "no-store".
func ComputeCacheControl(records []*model.ShareRecord, now time.Time, ceiling time.Duration, denyTTL time.Duration) string {
	if len(records) == 0 {
		// Deny: short max-age for negative caching.
		seconds := int64(math.Max(0, math.Floor(denyTTL.Seconds())))
		return fmt.Sprintf("max-age=%d", seconds)
	}

	// Allow: compute min(remaining lifetime), which is already capped at the ceiling
	// because that is the starting value.
	minRemaining := ceiling
	for _, record := range records {
		// An unlimited share contributes the ceiling, so it never lowers the result.
		remaining := ceiling
		if record.ExpiresAt != nil {
			remaining = max(record.ExpiresAt.Sub(now), 0)
		}
		minRemaining = min(minRemaining, remaining)
	}

	// A negative ceiling would otherwise yield a negative max-age.
	seconds := int64(math.Floor(max(minRemaining, 0).Seconds()))

	// Emit s-maxage + public so a shared cache (RFC 9111 §3.5) is explicitly
	// permitted to store the response even when the request carries an
	// Authorization header.
	return fmt.Sprintf("s-maxage=%d, public", seconds)
}
