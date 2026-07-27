package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/umccr/data-releaser/internal/model"
)

// AdminHandler provides HTTP-style handlers for the admin API endpoints
// (create, revoke, extend, list) of the Authorization Service.
// These handlers are designed to be deployed as a Lambda behind API Gateway.
//
// Requirements: 10.2, 10.3, 10.6, 10.7
type AdminHandler struct {
	store *ShareStore
}

// NewAdminHandler creates a new AdminHandler backed by the given ShareStore.
func NewAdminHandler(store *ShareStore) *AdminHandler {
	return &AdminHandler{store: store}
}

// CreateRequest represents the JSON body for the create share endpoint.
type CreateRequest struct {
	ResearcherAccountID string   `json:"researcherAccountId"`
	TargetVpcArn        string   `json:"targetVpcArn"`
	LibraryIDs          []string `json:"libraryIds"`
	S3Prefixes          []string `json:"s3Prefixes"`
	ExpiresAt           *string  `json:"expiresAt,omitempty"` // ISO 8601 or null for unlimited
}

// CreateResponse represents the JSON response for the create share endpoint.
type CreateResponse struct {
	ShareID             string   `json:"shareId"`
	ResearcherAccountID string   `json:"researcherAccountId"`
	LibraryIDs          []string `json:"libraryIds"`
	S3Prefixes          []string `json:"s3Prefixes"`
	CreatedAt           string   `json:"createdAt"`
	ExpiresAt           *string  `json:"expiresAt,omitempty"`
	Reused              bool     `json:"reused"`
}

// RevokeRequest represents the JSON body for the revoke share endpoint.
type RevokeRequest struct {
	ShareID string `json:"shareId"`
}

// RevokeResponse represents the JSON response for the revoke share endpoint.
type RevokeResponse struct {
	ShareID   string `json:"shareId"`
	RevokedAt string `json:"revokedAt"`
}

// ExtendRequest represents the JSON body for the extend share endpoint.
type ExtendRequest struct {
	ShareID  string `json:"shareId"`
	Duration string `json:"duration"` // Go duration string, e.g. "168h"
}

// ExtendResponse represents the JSON response for the extend share endpoint.
type ExtendResponse struct {
	ShareID   string `json:"shareId"`
	ExpiresAt string `json:"expiresAt"`
}

// ListRequest represents the query parameters for the list shares endpoint.
type ListRequest struct {
	ResearcherAccountID string `json:"researcherAccountId,omitempty"` // optional filter
}

// ShareRecordResponse represents a single share record in the list response.
type ShareRecordResponse struct {
	ShareID             string   `json:"shareId"`
	ResearcherAccountID string   `json:"researcherAccountId"`
	TargetVpcArn        string   `json:"targetVpcArn"`
	LibraryIDs          []string `json:"libraryIds"`
	S3Prefixes          []string `json:"s3Prefixes"`
	CreatedAt           string   `json:"createdAt"`
	ExpiresAt           *string  `json:"expiresAt,omitempty"`
	RevokedAt           *string  `json:"revokedAt,omitempty"`
	Status              string   `json:"status"`
}

// ListResponse represents the JSON response for the list shares endpoint.
type ListResponse struct {
	Shares []ShareRecordResponse `json:"shares"`
}

// ErrorResponse represents a JSON error response.
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details string `json:"details,omitempty"`
}

// Admin API error codes for state-conflict guards.
const (
	ErrCodeNotFound       = "SHARE_NOT_FOUND"
	ErrCodeAlreadyRevoked = "SHARE_ALREADY_REVOKED"
	ErrCodeExpired        = "SHARE_EXPIRED"
	ErrCodeInvalidInput   = "INVALID_INPUT"
	ErrCodeInternal       = "INTERNAL_ERROR"
)

// HandleCreate processes a create share request. It persists a ShareRecord to DynamoDB
// and returns the share ID. Uses idempotent creation to prevent duplicates.
//
// Requirements: 10.2 (DynamoDB storage), 12.1 (idempotent creation)
func (h *AdminHandler) HandleCreate(ctx context.Context, req CreateRequest) (*CreateResponse, *ErrorResponse, int) {
	// Validate input
	if req.ResearcherAccountID == "" {
		return nil, &ErrorResponse{
			Error: "researcherAccountId is required",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}
	if req.TargetVpcArn == "" {
		return nil, &ErrorResponse{
			Error: "targetVpcArn is required",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}
	if len(req.LibraryIDs) == 0 {
		return nil, &ErrorResponse{
			Error: "libraryIds must contain at least one library ID",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}
	if len(req.S3Prefixes) == 0 {
		return nil, &ErrorResponse{
			Error: "s3Prefixes must contain at least one S3 prefix",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}

	// Parse optional expiresAt
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			return nil, &ErrorResponse{
				Error:   "invalid expiresAt format, expected ISO 8601 (RFC3339)",
				Code:    ErrCodeInvalidInput,
				Details: err.Error(),
			}, http.StatusBadRequest
		}
		expiresAt = &t
	}

	input := CreateInput{
		ResearcherAccountID: req.ResearcherAccountID,
		TargetVpcArn:        req.TargetVpcArn,
		LibraryIDs:          req.LibraryIDs,
		S3Prefixes:          req.S3Prefixes,
		ExpiresAt:           expiresAt,
	}

	record, reused, err := h.store.CreateIdempotent(ctx, input)
	if err != nil {
		return nil, &ErrorResponse{
			Error:   "failed to create share record",
			Code:    ErrCodeInternal,
			Details: err.Error(),
		}, http.StatusInternalServerError
	}

	resp := &CreateResponse{
		ShareID:             record.ShareID,
		ResearcherAccountID: record.ResearcherAccountID,
		LibraryIDs:          record.LibraryIDs,
		S3Prefixes:          record.S3Prefixes,
		CreatedAt:           record.CreatedAt.Format(time.RFC3339),
		Reused:              reused,
	}
	if record.ExpiresAt != nil {
		s := record.ExpiresAt.Format(time.RFC3339)
		resp.ExpiresAt = &s
	}

	status := http.StatusCreated
	if reused {
		status = http.StatusOK
	}
	return resp, nil, status
}

// HandleRevoke processes a revoke share request. It performs a conditional update
// setting revokedAt, with state-conflict guards for already-revoked and expired shares.
//
// Requirements: 10.3 (conditional update), state-conflict guards (3.3, 3.5, 3.6)
func (h *AdminHandler) HandleRevoke(ctx context.Context, req RevokeRequest) (*RevokeResponse, *ErrorResponse, int) {
	if req.ShareID == "" {
		return nil, &ErrorResponse{
			Error: "shareId is required",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}

	record, err := h.store.Revoke(ctx, req.ShareID)
	if err != nil {
		return nil, mapStateError(err), mapStateHTTPStatus(err)
	}

	return &RevokeResponse{
		ShareID:   record.ShareID,
		RevokedAt: record.RevokedAt.Format(time.RFC3339),
	}, nil, http.StatusOK
}

// HandleExtend processes an extend share request. It performs a conditional update
// to expiresAt, with state-conflict guards for revoked and expired shares.
//
// Requirements: 10.3 (conditional update), state-conflict guards (9.2, 9.4)
func (h *AdminHandler) HandleExtend(ctx context.Context, req ExtendRequest) (*ExtendResponse, *ErrorResponse, int) {
	if req.ShareID == "" {
		return nil, &ErrorResponse{
			Error: "shareId is required",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}
	if req.Duration == "" {
		return nil, &ErrorResponse{
			Error: "duration is required",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}

	duration, err := time.ParseDuration(req.Duration)
	if err != nil {
		return nil, &ErrorResponse{
			Error:   "invalid duration format",
			Code:    ErrCodeInvalidInput,
			Details: fmt.Sprintf("expected Go duration string (e.g. '168h'), got %q: %v", req.Duration, err),
		}, http.StatusBadRequest
	}

	if duration < 24*time.Hour {
		return nil, &ErrorResponse{
			Error: "duration must be at least 24h (1 day)",
			Code:  ErrCodeInvalidInput,
		}, http.StatusBadRequest
	}

	record, err := h.store.Extend(ctx, req.ShareID, duration)
	if err != nil {
		return nil, mapStateError(err), mapStateHTTPStatus(err)
	}

	return &ExtendResponse{
		ShareID:   record.ShareID,
		ExpiresAt: record.ExpiresAt.Format(time.RFC3339),
	}, nil, http.StatusOK
}

// HandleList processes a list shares request. It queries GSI2 by researcherAccountId
// (if provided) or scans all records.
//
// Requirements: 10.6 (list), 10.7 (DynamoDB error → deny/error indication)
func (h *AdminHandler) HandleList(ctx context.Context, req ListRequest) (*ListResponse, *ErrorResponse, int) {
	var records []*model.ShareRecord
	var err error

	if req.ResearcherAccountID != "" {
		records, err = h.store.ListByAccount(ctx, req.ResearcherAccountID)
	} else {
		records, err = h.store.ListAll(ctx)
	}

	if err != nil {
		return nil, &ErrorResponse{
			Error:   "failed to list share records",
			Code:    ErrCodeInternal,
			Details: err.Error(),
		}, http.StatusInternalServerError
	}

	now := time.Now().UTC()
	shares := make([]ShareRecordResponse, 0, len(records))
	for _, r := range records {
		share := ShareRecordResponse{
			ShareID:             r.ShareID,
			ResearcherAccountID: r.ResearcherAccountID,
			TargetVpcArn:        r.TargetVpcArn,
			LibraryIDs:          r.LibraryIDs,
			S3Prefixes:          r.S3Prefixes,
			CreatedAt:           r.CreatedAt.Format(time.RFC3339),
			Status:              string(r.EffectiveStatus(now)),
		}
		if r.ExpiresAt != nil {
			s := r.ExpiresAt.Format(time.RFC3339)
			share.ExpiresAt = &s
		}
		if r.RevokedAt != nil {
			s := r.RevokedAt.Format(time.RFC3339)
			share.RevokedAt = &s
		}
		shares = append(shares, share)
	}

	return &ListResponse{Shares: shares}, nil, http.StatusOK
}

// HandleRequest routes an incoming API Gateway request to the appropriate handler
// based on the HTTP method and path. This serves as the Lambda handler entry point.
func (h *AdminHandler) HandleRequest(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	switch {
	case method == http.MethodPost && path == "/admin/shares":
		return h.handleCreateHTTP(ctx, body)
	case method == http.MethodPost && path == "/admin/shares/revoke":
		return h.handleRevokeHTTP(ctx, body)
	case method == http.MethodPost && path == "/admin/shares/extend":
		return h.handleExtendHTTP(ctx, body)
	case method == http.MethodGet && path == "/admin/shares":
		return h.handleListHTTP(ctx, body)
	default:
		errResp := ErrorResponse{
			Error: fmt.Sprintf("unknown route: %s %s", method, path),
			Code:  "NOT_FOUND",
		}
		respBody, _ := json.Marshal(errResp)
		return http.StatusNotFound, respBody, nil
	}
}

func (h *AdminHandler) handleCreateHTTP(ctx context.Context, body []byte) (int, []byte, error) {
	var req CreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		errResp := ErrorResponse{
			Error:   "invalid request body",
			Code:    ErrCodeInvalidInput,
			Details: err.Error(),
		}
		respBody, _ := json.Marshal(errResp)
		return http.StatusBadRequest, respBody, nil
	}

	resp, errResp, status := h.HandleCreate(ctx, req)
	if errResp != nil {
		respBody, _ := json.Marshal(errResp)
		return status, respBody, nil
	}

	respBody, err := json.Marshal(resp)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("marshaling response: %w", err)
	}
	return status, respBody, nil
}

func (h *AdminHandler) handleRevokeHTTP(ctx context.Context, body []byte) (int, []byte, error) {
	var req RevokeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		errResp := ErrorResponse{
			Error:   "invalid request body",
			Code:    ErrCodeInvalidInput,
			Details: err.Error(),
		}
		respBody, _ := json.Marshal(errResp)
		return http.StatusBadRequest, respBody, nil
	}

	resp, errResp, status := h.HandleRevoke(ctx, req)
	if errResp != nil {
		respBody, _ := json.Marshal(errResp)
		return status, respBody, nil
	}

	respBody, err := json.Marshal(resp)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("marshaling response: %w", err)
	}
	return status, respBody, nil
}

func (h *AdminHandler) handleExtendHTTP(ctx context.Context, body []byte) (int, []byte, error) {
	var req ExtendRequest
	if err := json.Unmarshal(body, &req); err != nil {
		errResp := ErrorResponse{
			Error:   "invalid request body",
			Code:    ErrCodeInvalidInput,
			Details: err.Error(),
		}
		respBody, _ := json.Marshal(errResp)
		return http.StatusBadRequest, respBody, nil
	}

	resp, errResp, status := h.HandleExtend(ctx, req)
	if errResp != nil {
		respBody, _ := json.Marshal(errResp)
		return status, respBody, nil
	}

	respBody, err := json.Marshal(resp)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("marshaling response: %w", err)
	}
	return status, respBody, nil
}

func (h *AdminHandler) handleListHTTP(ctx context.Context, body []byte) (int, []byte, error) {
	var req ListRequest
	// Body may be empty for GET requests; only parse if non-empty
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			errResp := ErrorResponse{
				Error:   "invalid request body",
				Code:    ErrCodeInvalidInput,
				Details: err.Error(),
			}
			respBody, _ := json.Marshal(errResp)
			return http.StatusBadRequest, respBody, nil
		}
	}

	resp, errResp, status := h.HandleList(ctx, req)
	if errResp != nil {
		respBody, _ := json.Marshal(errResp)
		return status, respBody, nil
	}

	respBody, err := json.Marshal(resp)
	if err != nil {
		return http.StatusInternalServerError, nil, fmt.Errorf("marshaling response: %w", err)
	}
	return status, respBody, nil
}

// mapStateError converts a domain error from the ShareStore into an ErrorResponse
// with appropriate error code.
func mapStateError(err error) *ErrorResponse {
	switch err {
	case ErrShareNotFound:
		return &ErrorResponse{
			Error: "share not found",
			Code:  ErrCodeNotFound,
		}
	case ErrShareAlreadyRevoked:
		return &ErrorResponse{
			Error: "share is already revoked",
			Code:  ErrCodeAlreadyRevoked,
		}
	case ErrShareExpired:
		return &ErrorResponse{
			Error: "share has already expired",
			Code:  ErrCodeExpired,
		}
	default:
		return &ErrorResponse{
			Error:   "internal error",
			Code:    ErrCodeInternal,
			Details: err.Error(),
		}
	}
}

// mapStateHTTPStatus converts a domain error into the appropriate HTTP status code.
func mapStateHTTPStatus(err error) int {
	switch err {
	case ErrShareNotFound:
		return http.StatusNotFound
	case ErrShareAlreadyRevoked:
		return http.StatusConflict
	case ErrShareExpired:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
