package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/umccr/data-releaser/internal/awstest"
)

func setupAdminHandler() (*AdminHandler, *awstest.DynamoDB) {
	mock := awstest.NewDynamoDB()
	store := NewShareStore(mock, "ShareRecords")
	handler := NewAdminHandler(store)
	return handler, mock
}

func TestHandleCreate_Success(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}
	resp, errResp, status := handler.HandleCreate(context.Background(), req)
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, status)
	}
	if resp.ShareID == "" {
		t.Error("expected non-empty ShareID")
	}
	if resp.Reused {
		t.Error("expected reused=false for new share")
	}
}

func TestHandleCreate_WithExpiry(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           new(time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)),
	}
	resp, errResp, status := handler.HandleCreate(context.Background(), req)
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, status)
	}
	if resp.ExpiresAt == nil {
		t.Error("expected non-nil ExpiresAt")
	}
}

func TestHandleCreate_Idempotent(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001", "L2400002"},
		S3Prefixes:          []string{"s3://bucket/prefix1/", "s3://bucket/prefix2/"},
	}
	resp1, _, _ := handler.HandleCreate(context.Background(), req)
	req2 := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400002", "L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix2/", "s3://bucket/prefix1/"},
	}
	resp2, _, status2 := handler.HandleCreate(context.Background(), req2)
	if status2 != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status2)
	}
	if !resp2.Reused {
		t.Error("expected reused=true")
	}
	if resp2.ShareID != resp1.ShareID {
		t.Errorf("expected same shareID %q, got %q", resp1.ShareID, resp2.ShareID)
	}
}

func TestHandleCreate_MissingAccount(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := CreateRequest{
		TargetVpcArn: "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:   []string{"L2400001"},
		S3Prefixes:   []string{"s3://bucket/prefix1/"},
	}
	_, errResp, status := handler.HandleCreate(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

func TestHandleCreate_EmptyLibraries(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}
	_, errResp, status := handler.HandleCreate(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

func TestHandleCreate_InvalidExpiry(t *testing.T) {
	handler, _ := setupAdminHandler()
	bad := "not-a-date"
	req := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &bad,
	}
	_, errResp, status := handler.HandleCreate(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

// TestHandleCreate_InvalidTargetVpcArn covers the admin API rejecting a target VPC ARN
// that could never match an authorization callout. Before ShareStore.Create validated
// this, the Lambda write path persisted such a share happily and the mistake only
// surfaced later as an unexplained deny.
func TestHandleCreate_InvalidTargetVpcArn(t *testing.T) {
	for name, arn := range map[string]string{
		"not an arn":          "arn",
		"lattice association": "arn:aws:vpc-lattice:ap-southeast-2:777788889999:servicenetworkserviceassociation/*",
		"other account":       "arn:aws:ec2:ap-southeast-2:999999999999:vpc/vpc-0000000000000001",
	} {
		t.Run(name, func(t *testing.T) {
			handler, mock := setupAdminHandler()
			_, errResp, status := handler.HandleCreate(context.Background(), CreateRequest{
				ResearcherAccountID: "777788889999",
				TargetVpcArn:        arn,
				LibraryIDs:          []string{"L2400001"},
				S3Prefixes:          []string{"demo/L2400001/"},
			})
			if errResp == nil {
				t.Fatal("expected an error response")
			}
			// A rejected ARN is caller error, not a store failure.
			if status != http.StatusBadRequest {
				t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
			}
			if errResp.Code != ErrCodeInvalidInput {
				t.Errorf("expected code %q, got %q", ErrCodeInvalidInput, errResp.Code)
			}
			if len(mock.Items) != 0 {
				t.Errorf("expected no share to be persisted, got %d", len(mock.Items))
			}
		})
	}
}

func TestHandleRevoke_Success(t *testing.T) {
	handler, _ := setupAdminHandler()
	createReq := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}
	createResp, _, _ := handler.HandleCreate(context.Background(), createReq)
	revokeReq := RevokeRequest{ShareID: createResp.ShareID}
	resp, errResp, status := handler.HandleRevoke(context.Background(), revokeReq)
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status)
	}
	if resp.ShareID != createResp.ShareID {
		t.Errorf("expected shareID %q, got %q", createResp.ShareID, resp.ShareID)
	}
	if resp.RevokedAt == "" {
		t.Error("expected non-empty RevokedAt")
	}
}

func TestHandleRevoke_NotFound(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := RevokeRequest{ShareID: "nonexistent-id"}
	_, errResp, status := handler.HandleRevoke(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusNotFound {
		t.Errorf("expected %d, got %d", http.StatusNotFound, status)
	}
	if errResp.Code != ErrCodeNotFound {
		t.Errorf("expected code %q, got %q", ErrCodeNotFound, errResp.Code)
	}
}

func TestHandleRevoke_AlreadyRevoked(t *testing.T) {
	handler, mock := setupAdminHandler()
	createReq := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}
	createResp, _, _ := handler.HandleCreate(context.Background(), createReq)
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[createResp.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}
	req := RevokeRequest{ShareID: createResp.ShareID}
	_, errResp, status := handler.HandleRevoke(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusConflict {
		t.Errorf("expected %d, got %d", http.StatusConflict, status)
	}
	if errResp.Code != ErrCodeAlreadyRevoked {
		t.Errorf("expected code %q, got %q", ErrCodeAlreadyRevoked, errResp.Code)
	}
}

func TestHandleRevoke_Expired(t *testing.T) {
	handler, _ := setupAdminHandler()
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	createReq := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &pastExpiry,
	}
	createResp, _, _ := handler.HandleCreate(context.Background(), createReq)
	req := RevokeRequest{ShareID: createResp.ShareID}
	_, errResp, status := handler.HandleRevoke(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusConflict {
		t.Errorf("expected %d, got %d", http.StatusConflict, status)
	}
	if errResp.Code != ErrCodeExpired {
		t.Errorf("expected code %q, got %q", ErrCodeExpired, errResp.Code)
	}
}

func TestHandleRevoke_EmptyShareID(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := RevokeRequest{ShareID: ""}
	_, errResp, status := handler.HandleRevoke(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

func TestHandleExtend_Success(t *testing.T) {
	handler, _ := setupAdminHandler()
	futureExpiry := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	createReq := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
		ExpiresAt:           &futureExpiry,
	}
	createResp, _, _ := handler.HandleCreate(context.Background(), createReq)
	extendReq := ExtendRequest{ShareID: createResp.ShareID, Duration: "168h"}
	resp, errResp, status := handler.HandleExtend(context.Background(), extendReq)
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status)
	}
	if resp.ShareID != createResp.ShareID {
		t.Errorf("expected shareID %q, got %q", createResp.ShareID, resp.ShareID)
	}
	if resp.ExpiresAt == "" {
		t.Error("expected non-empty ExpiresAt")
	}
}

func TestHandleExtend_NotFound(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := ExtendRequest{ShareID: "nonexistent-id", Duration: "168h"}
	_, errResp, status := handler.HandleExtend(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusNotFound {
		t.Errorf("expected %d, got %d", http.StatusNotFound, status)
	}
}

func TestHandleExtend_Revoked(t *testing.T) {
	handler, mock := setupAdminHandler()
	createReq := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}
	createResp, _, _ := handler.HandleCreate(context.Background(), createReq)
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[createResp.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}
	req := ExtendRequest{ShareID: createResp.ShareID, Duration: "168h"}
	_, errResp, status := handler.HandleExtend(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusConflict {
		t.Errorf("expected %d, got %d", http.StatusConflict, status)
	}
}

func TestHandleExtend_DurationTooShort(t *testing.T) {
	handler, _ := setupAdminHandler()
	createReq := CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	}
	createResp, _, _ := handler.HandleCreate(context.Background(), createReq)
	req := ExtendRequest{ShareID: createResp.ShareID, Duration: "1h"}
	_, errResp, status := handler.HandleExtend(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

func TestHandleExtend_InvalidDuration(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := ExtendRequest{ShareID: "some-id", Duration: "not-valid"}
	_, errResp, status := handler.HandleExtend(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

func TestHandleExtend_EmptyShareID(t *testing.T) {
	handler, _ := setupAdminHandler()
	req := ExtendRequest{ShareID: "", Duration: "168h"}
	_, errResp, status := handler.HandleExtend(context.Background(), req)
	if errResp == nil {
		t.Fatal("expected error response")
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
}

func TestHandleList_All(t *testing.T) {
	handler, _ := setupAdminHandler()
	handler.HandleCreate(context.Background(), CreateRequest{
		ResearcherAccountID: "111111111111",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:111111111111:vpc/vpc-0000000000000001",
		LibraryIDs:          []string{"L001"},
		S3Prefixes:          []string{"s3://bucket/p1/"},
	})
	handler.HandleCreate(context.Background(), CreateRequest{
		ResearcherAccountID: "222222222222",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:222222222222:vpc/vpc-0000000000000002",
		LibraryIDs:          []string{"L002"},
		S3Prefixes:          []string{"s3://bucket/p2/"},
	})
	resp, errResp, status := handler.HandleList(context.Background(), ListRequest{})
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status)
	}
	if len(resp.Shares) != 2 {
		t.Errorf("expected 2 shares, got %d", len(resp.Shares))
	}
}

func TestHandleList_ByAccount(t *testing.T) {
	handler, _ := setupAdminHandler()
	handler.HandleCreate(context.Background(), CreateRequest{
		ResearcherAccountID: "111111111111",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:111111111111:vpc/vpc-0000000000000001",
		LibraryIDs:          []string{"L001"},
		S3Prefixes:          []string{"s3://bucket/p1/"},
	})
	handler.HandleCreate(context.Background(), CreateRequest{
		ResearcherAccountID: "222222222222",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:222222222222:vpc/vpc-0000000000000002",
		LibraryIDs:          []string{"L002"},
		S3Prefixes:          []string{"s3://bucket/p2/"},
	})
	resp, errResp, status := handler.HandleList(context.Background(), ListRequest{ResearcherAccountID: "111111111111"})
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status)
	}
	if len(resp.Shares) != 1 {
		t.Errorf("expected 1 share, got %d", len(resp.Shares))
	}
}

func TestHandleList_Empty(t *testing.T) {
	handler, _ := setupAdminHandler()
	resp, errResp, status := handler.HandleList(context.Background(), ListRequest{})
	if errResp != nil {
		t.Fatalf("unexpected error: %+v", errResp)
	}
	if status != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status)
	}
	if len(resp.Shares) != 0 {
		t.Errorf("expected 0 shares, got %d", len(resp.Shares))
	}
}

func TestHandleList_IncludesStatus(t *testing.T) {
	handler, mock := setupAdminHandler()
	createResp, _, _ := handler.HandleCreate(context.Background(), CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	})
	revokedAt := time.Now().UTC().Format(time.RFC3339)
	mock.Items[createResp.ShareID]["revokedAt"] = &types.AttributeValueMemberS{Value: revokedAt}
	resp, _, _ := handler.HandleList(context.Background(), ListRequest{})
	if len(resp.Shares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(resp.Shares))
	}
	if resp.Shares[0].Status != "revoked" {
		t.Errorf("expected status 'revoked', got %q", resp.Shares[0].Status)
	}
}

func TestHandleRequest_CreateRoute(t *testing.T) {
	handler, _ := setupAdminHandler()
	body, _ := json.Marshal(CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	})
	status, respBody, err := handler.HandleRequest(context.Background(), http.MethodPost, "/admin/shares", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("expected %d, got %d", http.StatusCreated, status)
	}
	var resp CreateResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ShareID == "" {
		t.Error("expected non-empty ShareID")
	}
}

func TestHandleRequest_ListRoute(t *testing.T) {
	handler, _ := setupAdminHandler()
	body, _ := json.Marshal(CreateRequest{
		ResearcherAccountID: "777788889999",
		TargetVpcArn:        "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-123",
		LibraryIDs:          []string{"L2400001"},
		S3Prefixes:          []string{"s3://bucket/prefix1/"},
	})
	handler.HandleRequest(context.Background(), http.MethodPost, "/admin/shares", body)
	status, respBody, err := handler.HandleRequest(context.Background(), http.MethodGet, "/admin/shares", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, status)
	}
	var resp ListResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Shares) != 1 {
		t.Errorf("expected 1 share, got %d", len(resp.Shares))
	}
}

func TestHandleRequest_UnknownRoute(t *testing.T) {
	handler, _ := setupAdminHandler()
	status, respBody, err := handler.HandleRequest(context.Background(), http.MethodDelete, "/unknown", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("expected %d, got %d", http.StatusNotFound, status)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(respBody, &errResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errResp.Code != "NOT_FOUND" {
		t.Errorf("expected code 'NOT_FOUND', got %q", errResp.Code)
	}
}

func TestHandleRequest_InvalidJSON(t *testing.T) {
	handler, _ := setupAdminHandler()
	status, respBody, err := handler.HandleRequest(context.Background(), http.MethodPost, "/admin/shares", []byte("bad"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, status)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(respBody, &errResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errResp.Code != ErrCodeInvalidInput {
		t.Errorf("expected code %q, got %q", ErrCodeInvalidInput, errResp.Code)
	}
}
