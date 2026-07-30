// Command admin is the deployable Lambda entrypoint for the admin interface of
// the Authorization Service (create, revoke, extend, list), called by the CLI.
//
// It is wiring only: it reads configuration from the environment, constructs a
// DynamoDB client from the Lambda execution role, and delegates to the existing
// auth.AdminHandler in internal/auth. No business logic lives here (Req 16.6).
// AdminHandler.HandleRequest already routes by method+path and already maps
// state-conflict domain errors onto HTTP status codes (404 not found, 409
// already-revoked/expired), so this entrypoint only translates the API Gateway
// proxy event into that call and maps the returned (status, body) back.
//
// Environment variables (read once at cold start):
//
//	SHARE_RECORDS_TABLE_NAME  (required) DynamoDB table holding Share_Records.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/umccr/data-releaser/internal/auth"
)

// envTableName is the environment variable naming the Share_Records table.
// It matches the variable read by the authorization entrypoint.
const envTableName = "SHARE_RECORDS_TABLE_NAME"

// handler holds the cold-start-resolved admin handler.
type handler struct {
	admin *auth.AdminHandler
}

// handle adapts an API Gateway proxy event to AdminHandler.HandleRequest and
// maps the returned (status, body) back onto a proxy response. Routing and
// state-conflict → HTTP status mapping are already performed inside
// HandleRequest, so this is a pass-through.
func (h *handler) handle(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	status, body, err := h.admin.HandleRequest(ctx, req.HTTPMethod, req.Path, []byte(req.Body))
	if err != nil {
		// HandleRequest only returns a non-nil error for internal marshal
		// failures; everything else is encoded in (status, body). Surface it
		// as a 500 with a JSON body.
		log.Printf("admin request error: %v", err)
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       `{"error":"internal error"}`,
		}, nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}, nil
}

func main() {
	ctx := context.Background()

	tableName := os.Getenv(envTableName)
	if tableName == "" {
		log.Fatalf("%s is required but not set", envTableName)
	}

	// The Lambda uses its execution-role credentials from the environment; the
	// default config chain resolves them without an eager profile/credential
	// check (that check belongs to the CLI factory, not a Lambda).
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)
	store := auth.NewShareStore(client, tableName)

	h := &handler{
		admin: auth.NewAdminHandler(store),
	}

	lambda.Start(h.handle)
}
