// Command copycompletion is the deployable Lambda entrypoint that closes the loop on a
// dataset copy. A copy runs for hours or days, so no command waits on it; the copy service
// announces its terminal state on the event bus, an EventBridge rule delivers that here,
// and the copy record is moved to staged or failed.
//
// It is wiring only: it reads configuration from the environment, constructs clients from
// the Lambda execution role, and delegates to dataset.CopyCompletionHandler. Matching an
// event to a copy, deciding what to record, and deciding what is worth retrying all live
// there, where they are testable without a deployment.
//
// It is handed a copy ledger and nothing else. Recording what a transfer delivered has no
// business reaching a grant: who may read the data is decided by the commands, from a
// record this entrypoint cannot see and would have no reason to write.
//
// The handler returns an error only for a failure another attempt could get past, so a
// non-nil error here is a genuine invitation to retry. An event that names a copy nothing
// is waiting on, or a copy that has already reported, is not a failure and is logged as
// nothing to do.
//
// Environment variables (read once at cold start):
//
//	SHARE_RECORDS_TABLE_NAME  (required) DynamoDB table holding the ledger, shared with
//	                          the htsget mechanism.
//
// The execution role needs write access to that table, permission to describe executions
// of the copy state machine, and read access to the copy service's working bucket -- where
// the copy retains its summary of what it moved. It deliberately needs no access to any
// dataset's own bucket or key.
package main

import (
	"context"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"

	"github.com/umccr/data-releaser/internal/dataset"
)

// envTableName is the environment variable naming the ledger table. It matches the
// variable read by the other entrypoints, which address the same table.
const envTableName = "SHARE_RECORDS_TABLE_NAME"

// handler holds the cold-start-resolved completion handler.
type handler struct {
	completions *dataset.CopyCompletionHandler
}

// handle adapts an EventBridge event to the completion handler. Only the detail is passed
// on: the envelope says which bus and account the event came from, and neither bears on
// which copy the execution was performing.
func (h *handler) handle(ctx context.Context, event events.EventBridgeEvent) error {
	completion, err := h.completions.HandleEvent(ctx, event.Detail)
	if err != nil {
		// Worth another attempt, so it is returned as well as logged.
		log.Printf("copy completion event %s could not be recorded: %v", event.ID, err)
		return err
	}

	log.Print(completion)
	return nil
}

func main() {
	ctx := context.Background()

	tableName := os.Getenv(envTableName)
	if tableName == "" {
		log.Fatalf("%s is required but not set", envTableName)
	}

	// The Lambda uses its execution-role credentials from the environment; the default
	// config chain resolves them without an eager credential check (that check belongs
	// to the CLI factory, not a Lambda).
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	h := &handler{
		completions: dataset.NewCopyCompletionHandler(
			dataset.NewCopyStore(dynamodb.NewFromConfig(cfg), tableName),
			sfn.NewFromConfig(cfg),
			s3.NewFromConfig(cfg),
		),
	}

	lambda.Start(h.handle)
}
