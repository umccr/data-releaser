package warehouse

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/config"
	"github.com/umccr/data-releaser/internal/model"
)

// DefaultPollInterval is the default time between Athena query status polls.
const DefaultPollInterval = 1 * time.Second

// Client wraps an Athena client and warehouse configuration to resolve library IDs
// to S3 file locations via Athena queries against the OrcaVault Mart.
type Client struct {
	athena       internalaws.AthenaClient
	workgroup    string
	database     string
	catalog      string
	pollInterval time.Duration
}

// NewClient creates a new warehouse Client from the given Athena client and config.
func NewClient(athenaClient internalaws.AthenaClient, whCfg config.WarehouseConfig) *Client {
	return &Client{
		athena:       athenaClient,
		workgroup:    whCfg.AthenaWorkgroup,
		database:     whCfg.Database,
		catalog:      whCfg.Catalog,
		pollInterval: DefaultPollInterval,
	}
}

// SetPollInterval overrides the default poll interval (useful for testing).
func (c *Client) SetPollInterval(d time.Duration) {
	c.pollInterval = d
}

// ResolveLibraries resolves library IDs to their associated S3 file locations by
// querying the OrcaVault Mart via Athena. It:
//  1. Builds the resolution SQL query
//  2. Submits the query to Athena with the configured workgroup/database/catalog
//  3. Polls until the query reaches a terminal state (SUCCEEDED, FAILED, CANCELLED)
//  4. On FAILED/CANCELLED: returns an error with the Athena error message
//  5. On SUCCEEDED: parses the result rows into ResolvedLibrary structs
//
// Requirements: 1.3, 1.8
func (c *Client) ResolveLibraries(ctx context.Context, libraryIDs []string) ([]model.ResolvedLibrary, error) {
	sql, err := BuildResolutionQuery(c.database, libraryIDs)
	if err != nil {
		return nil, fmt.Errorf("building resolution query: %w", err)
	}

	queryID, err := c.startQuery(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("starting Athena query: %w", err)
	}

	if err := c.pollQueryCompletion(ctx, queryID); err != nil {
		return nil, err
	}

	results, err := c.getResults(ctx, queryID)
	if err != nil {
		return nil, fmt.Errorf("retrieving Athena query results: %w", err)
	}

	return results, nil
}

// startQuery submits a query to Athena and returns the query execution ID.
func (c *Client) startQuery(ctx context.Context, sql string) (string, error) {
	input := &athena.StartQueryExecutionInput{
		QueryString: &sql,
		WorkGroup:   &c.workgroup,
		QueryExecutionContext: &athenatypes.QueryExecutionContext{
			Database: &c.database,
			Catalog:  &c.catalog,
		},
	}

	output, err := c.athena.StartQueryExecution(ctx, input)
	if err != nil {
		return "", err
	}

	if output.QueryExecutionId == nil {
		return "", fmt.Errorf("Athena returned nil query execution ID")
	}

	return *output.QueryExecutionId, nil
}

// pollQueryCompletion polls GetQueryExecution until the query reaches a terminal state.
// Returns nil on SUCCEEDED, or an error on FAILED/CANCELLED with the Athena error message.
func (c *Client) pollQueryCompletion(ctx context.Context, queryID string) error {
	input := &athena.GetQueryExecutionInput{
		QueryExecutionId: &queryID,
	}

	for {
		output, err := c.athena.GetQueryExecution(ctx, input)
		if err != nil {
			return fmt.Errorf("polling Athena query %s: %w", queryID, err)
		}

		if output.QueryExecution == nil || output.QueryExecution.Status == nil {
			return fmt.Errorf("Athena returned nil query execution status for query %s", queryID)
		}

		state := output.QueryExecution.Status.State
		switch state {
		case athenatypes.QueryExecutionStateSucceeded:
			return nil
		case athenatypes.QueryExecutionStateFailed:
			reason := "unknown error"
			if output.QueryExecution.Status.StateChangeReason != nil {
				reason = *output.QueryExecution.Status.StateChangeReason
			}
			return fmt.Errorf("Athena query %s failed: %s", queryID, reason)
		case athenatypes.QueryExecutionStateCancelled:
			reason := "query was cancelled"
			if output.QueryExecution.Status.StateChangeReason != nil {
				reason = *output.QueryExecution.Status.StateChangeReason
			}
			return fmt.Errorf("Athena query %s cancelled: %s", queryID, reason)
		case athenatypes.QueryExecutionStateQueued, athenatypes.QueryExecutionStateRunning:
			// Still in progress, wait and retry
		default:
			return fmt.Errorf("Athena query %s in unexpected state: %s", queryID, state)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while polling Athena query %s: %w", queryID, ctx.Err())
		case <-time.After(c.pollInterval):
		}
	}
}

// getResults retrieves all result rows from a completed Athena query, handling
// pagination via NextToken, and parses them into ResolvedLibrary structs.
func (c *Client) getResults(ctx context.Context, queryID string) ([]model.ResolvedLibrary, error) {
	var results []model.ResolvedLibrary
	var nextToken *string
	var colIndex map[string]int
	firstPage := true

	for {
		input := &athena.GetQueryResultsInput{
			QueryExecutionId: &queryID,
			NextToken:        nextToken,
		}

		output, err := c.athena.GetQueryResults(ctx, input)
		if err != nil {
			return nil, err
		}

		if output.ResultSet == nil {
			break
		}

		rows := output.ResultSet.Rows
		startRow := 0
		if firstPage {
			if len(rows) == 0 {
				return nil, fmt.Errorf("no header row found in Athena query results")
			}
			colIndex = buildColumnIndex(rows[0])
			startRow = 1
			firstPage = false
		}

		for i := startRow; i < len(rows); i++ {
			resolved, err := parseResultRow(rows[i], colIndex)
			if err != nil {
				return nil, fmt.Errorf("parsing result row %d: %w", i, err)
			}
			results = append(results, resolved)
		}

		nextToken = output.NextToken
		if nextToken == nil {
			break
		}
	}

	return results, nil
}

// buildColumnIndex creates a mapping from column name to index from the header row.
func buildColumnIndex(headerRow athenatypes.Row) map[string]int {
	index := make(map[string]int)
	for i, datum := range headerRow.Data {
		if datum.VarCharValue != nil {
			index[*datum.VarCharValue] = i
		}
	}
	return index
}

// parseResultRow converts a single Athena result row into a ResolvedLibrary struct
// using the column index built from the header row.
func parseResultRow(row athenatypes.Row, colIndex map[string]int) (model.ResolvedLibrary, error) {
	getValue := func(colName string) (string, error) {
		idx, ok := colIndex[colName]
		if !ok {
			return "", fmt.Errorf("column %q not found in result set", colName)
		}
		if idx >= len(row.Data) {
			return "", fmt.Errorf("column %q index %d out of range (row has %d columns)", colName, idx, len(row.Data))
		}
		datum := row.Data[idx]
		if datum.VarCharValue == nil {
			return "", nil
		}
		return *datum.VarCharValue, nil
	}

	libraryID, err := getValue("library_id")
	if err != nil {
		return model.ResolvedLibrary{}, err
	}
	portalRunID, err := getValue("portal_run_id")
	if err != nil {
		return model.ResolvedLibrary{}, err
	}
	bucket, err := getValue("bucket")
	if err != nil {
		return model.ResolvedLibrary{}, err
	}
	key, err := getValue("key")
	if err != nil {
		return model.ResolvedLibrary{}, err
	}
	format, err := getValue("format")
	if err != nil {
		return model.ResolvedLibrary{}, err
	}
	storageClass, err := getValue("storage_class")
	if err != nil {
		return model.ResolvedLibrary{}, err
	}

	return model.ResolvedLibrary{
		LibraryID:    libraryID,
		PortalRunID:  portalRunID,
		Bucket:       bucket,
		Key:          key,
		Format:       format,
		StorageClass: storageClass,
	}, nil
}
