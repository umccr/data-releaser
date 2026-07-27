package warehouse

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/umccr/data-releaser/internal/config"
)

// mockAthenaClient implements the AthenaClient interface for testing.
type mockAthenaClient struct {
	startQueryFn func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error)
	getQueryFn   func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error)
	getResultsFn func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error)
}

func (m *mockAthenaClient) StartQueryExecution(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
	return m.startQueryFn(ctx, params, optFns...)
}

func (m *mockAthenaClient) GetQueryExecution(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	return m.getQueryFn(ctx, params, optFns...)
}

func (m *mockAthenaClient) GetQueryResults(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
	return m.getResultsFn(ctx, params, optFns...)
}

func newTestClient(mock *mockAthenaClient) *Client {
	cfg := config.WarehouseConfig{
		AthenaWorkgroup: "primary",
		Database:        "mart",
		Catalog:         "AwsDataCatalog",
	}
	client := NewClient(mock, cfg)
	client.SetPollInterval(1 * time.Millisecond) // fast polling for tests
	return client
}

// makeHeaderRow builds an Athena result header row.
func makeHeaderRow() athenatypes.Row {
	return athenatypes.Row{
		Data: []athenatypes.Datum{
			{VarCharValue: aws.String("library_id")},
			{VarCharValue: aws.String("portal_run_id")},
			{VarCharValue: aws.String("bucket")},
			{VarCharValue: aws.String("key")},
			{VarCharValue: aws.String("format")},
			{VarCharValue: aws.String("storage_class")},
		},
	}
}

// makeDataRow builds an Athena result data row.
func makeDataRow(libID, portalRunID, bucket, key, format, storageClass string) athenatypes.Row {
	return athenatypes.Row{
		Data: []athenatypes.Datum{
			{VarCharValue: aws.String(libID)},
			{VarCharValue: aws.String(portalRunID)},
			{VarCharValue: aws.String(bucket)},
			{VarCharValue: aws.String(key)},
			{VarCharValue: aws.String(format)},
			{VarCharValue: aws.String(storageClass)},
		},
	}
}

func TestResolveLibraries_Success(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			// Verify workgroup/database/catalog are passed correctly
			if *params.WorkGroup != "primary" {
				t.Errorf("expected workgroup 'primary', got %q", *params.WorkGroup)
			}
			if *params.QueryExecutionContext.Database != "mart" {
				t.Errorf("expected database 'mart', got %q", *params.QueryExecutionContext.Database)
			}
			if *params.QueryExecutionContext.Catalog != "AwsDataCatalog" {
				t.Errorf("expected catalog 'AwsDataCatalog', got %q", *params.QueryExecutionContext.Catalog)
			}
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-123"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: athenatypes.QueryExecutionStateSucceeded,
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			return &athena.GetQueryResultsOutput{
				ResultSet: &athenatypes.ResultSet{
					Rows: []athenatypes.Row{
						makeHeaderRow(),
						makeDataRow("L2400001", "run-001", "bucket-a", "data/output/sample.bam", "bam", "STANDARD"),
					},
				},
			}, nil
		},
	}

	client := newTestClient(mock)
	results, err := client.ResolveLibraries(context.Background(), []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.LibraryID != "L2400001" {
		t.Errorf("expected LibraryID 'L2400001', got %q", r.LibraryID)
	}
	if r.PortalRunID != "run-001" {
		t.Errorf("expected PortalRunID 'run-001', got %q", r.PortalRunID)
	}
	if r.Bucket != "bucket-a" {
		t.Errorf("expected Bucket 'bucket-a', got %q", r.Bucket)
	}
	if r.Key != "data/output/sample.bam" {
		t.Errorf("expected Key 'data/output/sample.bam', got %q", r.Key)
	}
	if r.Format != "bam" {
		t.Errorf("expected Format 'bam', got %q", r.Format)
	}
	if r.StorageClass != "STANDARD" {
		t.Errorf("expected StorageClass 'STANDARD', got %q", r.StorageClass)
	}
}

func TestResolveLibraries_QueryFailed(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-fail"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State:             athenatypes.QueryExecutionStateFailed,
						StateChangeReason: aws.String("Table not found: mart.bam"),
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			t.Fatal("GetQueryResults should not be called on failed query")
			return nil, nil
		},
	}

	client := newTestClient(mock)
	_, err := client.ResolveLibraries(context.Background(), []string{"L2400001"})
	if err == nil {
		t.Fatal("expected error for failed query")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestResolveLibraries_QueryCancelled(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-cancel"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State:             athenatypes.QueryExecutionStateCancelled,
						StateChangeReason: aws.String("User cancelled"),
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			t.Fatal("GetQueryResults should not be called on cancelled query")
			return nil, nil
		},
	}

	client := newTestClient(mock)
	_, err := client.ResolveLibraries(context.Background(), []string{"L2400001"})
	if err == nil {
		t.Fatal("expected error for cancelled query")
	}
}

func TestResolveLibraries_PollingTransition(t *testing.T) {
	// Simulate QUEUED -> RUNNING -> SUCCEEDED
	callCount := 0
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-poll"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			callCount++
			var state athenatypes.QueryExecutionState
			switch callCount {
			case 1:
				state = athenatypes.QueryExecutionStateQueued
			case 2:
				state = athenatypes.QueryExecutionStateRunning
			default:
				state = athenatypes.QueryExecutionStateSucceeded
			}
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: state,
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			return &athena.GetQueryResultsOutput{
				ResultSet: &athenatypes.ResultSet{
					Rows: []athenatypes.Row{
						makeHeaderRow(),
						makeDataRow("L2400001", "run-001", "bucket-a", "data/sample.bam", "bam", "STANDARD"),
					},
				},
			}, nil
		},
	}

	client := newTestClient(mock)
	results, err := client.ResolveLibraries(context.Background(), []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if callCount < 3 {
		t.Errorf("expected at least 3 poll calls, got %d", callCount)
	}
}

func TestResolveLibraries_StartQueryError(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return nil, fmt.Errorf("access denied")
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			t.Fatal("GetQueryExecution should not be called")
			return nil, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			t.Fatal("GetQueryResults should not be called")
			return nil, nil
		},
	}

	client := newTestClient(mock)
	_, err := client.ResolveLibraries(context.Background(), []string{"L2400001"})
	if err == nil {
		t.Fatal("expected error when StartQueryExecution fails")
	}
}

func TestResolveLibraries_MultipleResults(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-multi"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: athenatypes.QueryExecutionStateSucceeded,
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			return &athena.GetQueryResultsOutput{
				ResultSet: &athenatypes.ResultSet{
					Rows: []athenatypes.Row{
						makeHeaderRow(),
						makeDataRow("L2400001", "run-001", "bucket-a", "data/output1/sample1.bam", "bam", "STANDARD"),
						makeDataRow("L2400002", "run-002", "bucket-a", "data/output2/sample2.bam", "bam", "STANDARD"),
						makeDataRow("L2400001", "run-003", "bucket-b", "data/output3/sample3.bam", "bam", "INTELLIGENT_TIERING"),
					},
				},
			}, nil
		},
	}

	client := newTestClient(mock)
	results, err := client.ResolveLibraries(context.Background(), []string{"L2400001", "L2400002"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].LibraryID != "L2400001" || results[0].Key != "data/output1/sample1.bam" {
		t.Errorf("first result mismatch: %+v", results[0])
	}
	if results[1].LibraryID != "L2400002" || results[1].Key != "data/output2/sample2.bam" {
		t.Errorf("second result mismatch: %+v", results[1])
	}
	if results[2].LibraryID != "L2400001" || results[2].StorageClass != "INTELLIGENT_TIERING" {
		t.Errorf("third result mismatch: %+v", results[2])
	}
}

func TestResolveLibraries_Pagination(t *testing.T) {
	page := 0
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-paginated"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: athenatypes.QueryExecutionStateSucceeded,
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			page++
			switch page {
			case 1:
				return &athena.GetQueryResultsOutput{
					ResultSet: &athenatypes.ResultSet{
						Rows: []athenatypes.Row{
							makeHeaderRow(),
							makeDataRow("L2400001", "run-001", "bucket-a", "data/p1.bam", "bam", "STANDARD"),
						},
					},
					NextToken: aws.String("token-2"),
				}, nil
			case 2:
				return &athena.GetQueryResultsOutput{
					ResultSet: &athenatypes.ResultSet{
						Rows: []athenatypes.Row{
							makeDataRow("L2400002", "run-002", "bucket-a", "data/p2.bam", "bam", "STANDARD"),
						},
					},
				}, nil
			default:
				t.Fatal("unexpected page request")
				return nil, nil
			}
		},
	}

	client := newTestClient(mock)
	results, err := client.ResolveLibraries(context.Background(), []string{"L2400001", "L2400002"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results across pages, got %d", len(results))
	}
	if results[0].LibraryID != "L2400001" {
		t.Errorf("expected first result L2400001, got %q", results[0].LibraryID)
	}
	if results[1].LibraryID != "L2400002" {
		t.Errorf("expected second result L2400002, got %q", results[1].LibraryID)
	}
}

func TestResolveLibraries_EmptyResults(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-empty"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: athenatypes.QueryExecutionStateSucceeded,
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			// Only header row, no data
			return &athena.GetQueryResultsOutput{
				ResultSet: &athenatypes.ResultSet{
					Rows: []athenatypes.Row{
						makeHeaderRow(),
					},
				},
			}, nil
		},
	}

	client := newTestClient(mock)
	results, err := client.ResolveLibraries(context.Background(), []string{"L2400001"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestResolveLibraries_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	callCount := 0
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			return &athena.StartQueryExecutionOutput{
				QueryExecutionId: aws.String("query-ctx"),
			}, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			callCount++
			if callCount >= 2 {
				cancel() // cancel after second poll
			}
			return &athena.GetQueryExecutionOutput{
				QueryExecution: &athenatypes.QueryExecution{
					Status: &athenatypes.QueryExecutionStatus{
						State: athenatypes.QueryExecutionStateRunning,
					},
				},
			}, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			t.Fatal("GetQueryResults should not be called when context is cancelled")
			return nil, nil
		},
	}

	client := newTestClient(mock)
	_, err := client.ResolveLibraries(ctx, []string{"L2400001"})
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

func TestResolveLibraries_InvalidLibraryIDs(t *testing.T) {
	mock := &mockAthenaClient{
		startQueryFn: func(ctx context.Context, params *athena.StartQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.StartQueryExecutionOutput, error) {
			t.Fatal("StartQueryExecution should not be called for invalid IDs")
			return nil, nil
		},
		getQueryFn: func(ctx context.Context, params *athena.GetQueryExecutionInput, optFns ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
			t.Fatal("GetQueryExecution should not be called")
			return nil, nil
		},
		getResultsFn: func(ctx context.Context, params *athena.GetQueryResultsInput, optFns ...func(*athena.Options)) (*athena.GetQueryResultsOutput, error) {
			t.Fatal("GetQueryResults should not be called")
			return nil, nil
		},
	}

	client := newTestClient(mock)
	_, err := client.ResolveLibraries(context.Background(), []string{"bad;id"})
	if err == nil {
		t.Fatal("expected error for invalid library ID")
	}
}

func TestNewClient(t *testing.T) {
	mock := &mockAthenaClient{}
	cfg := config.WarehouseConfig{
		AthenaWorkgroup: "test-wg",
		Database:        "test-db",
		Catalog:         "test-cat",
	}
	client := NewClient(mock, cfg)
	if client.workgroup != "test-wg" {
		t.Errorf("expected workgroup 'test-wg', got %q", client.workgroup)
	}
	if client.database != "test-db" {
		t.Errorf("expected database 'test-db', got %q", client.database)
	}
	if client.catalog != "test-cat" {
		t.Errorf("expected catalog 'test-cat', got %q", client.catalog)
	}
	if client.pollInterval != DefaultPollInterval {
		t.Errorf("expected default poll interval %v, got %v", DefaultPollInterval, client.pollInterval)
	}
}
