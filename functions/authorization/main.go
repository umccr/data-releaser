// Command authorization is the deployable Lambda entrypoint for the htsget-rs
// authorization callout interface of the Authorization Service.
//
// It is wiring only: it reads configuration from the environment, constructs a
// DynamoDB client from the Lambda execution role, and delegates to the existing
// auth.CalloutHandler in internal/auth. No business logic lives here (Req 16.6);
// the handler logic has exactly one implementation, shared with the CLI's tests.
//
// Environment variables (read once at cold start):
//
//	SHARE_RECORDS_TABLE_NAME  (required) DynamoDB table holding Share_Records.
//	CACHE_TTL_CEILING         (optional) max-age ceiling as a Go duration, e.g. "1h".
//	                          Default 1h. Caps the allow Cache-Control (Req 13.1).
//	CACHE_DENY_TTL            (optional) negative-cache max-age as a Go duration,
//	                          e.g. "60s". Default 60s (Req 13.2).
//
// The source VPC ARN of the requester arrives as the "sourceVpcArn" query string
// parameter of the API Gateway proxy event (the same shape the CLI's validate
// probe uses). The handler emits a Cache-Control header computed from the matched
// shares' remaining lifetimes; that value is copied onto the proxy response.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/umccr/data-releaser/internal/auth"
)

// Environment variable names and defaults.
const (
	envTableName  = "SHARE_RECORDS_TABLE_NAME"
	envTTLCeiling = "CACHE_TTL_CEILING"
	envDenyTTL    = "CACHE_DENY_TTL"

	defaultTTLCeiling = time.Hour
	defaultDenyTTL    = 60 * time.Second

	// sourceVpcArnParam is the query string parameter carrying the requester's
	// source VPC ARN, matching the convention used elsewhere in this project.
	sourceVpcArnParam = "sourceVpcArn"
)

// handler holds the cold-start-resolved dependencies and configuration.
type handler struct {
	callout *auth.CalloutHandler
	ceiling time.Duration
	denyTTL time.Duration
}

// handle adapts an API Gateway proxy event to the CalloutHandler and maps the
// result back onto a proxy response.
func (h *handler) handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	sourceVpcArn := req.QueryStringParameters[sourceVpcArnParam]

	result, calloutErr := h.callout.HandleCalloutWithHeaders(ctx, sourceVpcArn, h.ceiling, h.denyTTL)

	// HandleCalloutWithHeaders always returns a non-nil result (a deny response)
	// even on error, so we always have a body and Cache-Control value to emit.
	body, marshalErr := json.Marshal(result.Response)
	if marshalErr != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       `{"error":"failed to marshal authorization response"}`,
		}, nil
	}

	resp := events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers: map[string]string{
			"Content-Type":  "application/json",
			"Cache-Control": result.CacheControl,
			"Vary":          result.Vary,
		},
		Body: string(body),
	}

	// On a backend error the handler still returns a fail-closed deny body; log
	// it for observability but keep serving the deny response with its short
	// negative-cache max-age.
	if calloutErr != nil {
		log.Printf("authorization callout error (serving fail-closed deny): %v", calloutErr)
	}

	return resp, nil
}

func main() {
	ctx := context.Background()

	tableName := os.Getenv(envTableName)
	if tableName == "" {
		log.Fatalf("%s is required but not set", envTableName)
	}

	ceiling, err := parseDurationEnv(envTTLCeiling, defaultTTLCeiling)
	if err != nil {
		log.Fatalf("invalid %s: %v", envTTLCeiling, err)
	}

	denyTTL, err := parseDurationEnv(envDenyTTL, defaultDenyTTL)
	if err != nil {
		log.Fatalf("invalid %s: %v", envDenyTTL, err)
	}

	// The Lambda uses its execution-role credentials from the environment; the
	// default config chain resolves them without an eager profile/credential
	// check (that check belongs to the CLI factory, not a Lambda).
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	h := &handler{
		callout: auth.NewCalloutHandler(client, tableName),
		ceiling: ceiling,
		denyTTL: denyTTL,
	}

	lambda.Start(h.handle)
}

// parseDurationEnv reads a Go duration from the named environment variable.
// If the variable is unset or empty, def is returned. If it is set but cannot
// be parsed, an error is returned so the caller can fail fast at cold start.
func parseDurationEnv(name string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing %q as a duration: %w", raw, err)
	}
	return d, nil
}
