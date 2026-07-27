package lattice

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ram"
	ramtypes "github.com/aws/aws-sdk-go-v2/service/ram/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// mockRAMClient implements internalaws.RAMClient for testing.
type mockRAMClient struct {
	output *ram.GetResourceShareAssociationsOutput
	err    error
}

// Ensure mockRAMClient satisfies the interface.
var _ internalaws.RAMClient = (*mockRAMClient)(nil)

func (m *mockRAMClient) GetResourceShareAssociations(ctx context.Context, params *ram.GetResourceShareAssociationsInput, optFns ...func(*ram.Options)) (*ram.GetResourceShareAssociationsOutput, error) {
	return m.output, m.err
}

func TestPreflight_ActiveAssociation(t *testing.T) {
	client := &mockRAMClient{
		output: &ram.GetResourceShareAssociationsOutput{
			ResourceShareAssociations: []ramtypes.ResourceShareAssociation{
				{Status: ramtypes.ResourceShareAssociationStatusAssociated},
			},
		},
	}
	checker := NewChecker(client)

	err := Preflight(context.Background(), checker, "123456789012")
	if err != nil {
		t.Fatalf("expected nil error for active association, got: %v", err)
	}
}

func TestPreflight_NoAssociation(t *testing.T) {
	client := &mockRAMClient{
		output: &ram.GetResourceShareAssociationsOutput{
			ResourceShareAssociations: []ramtypes.ResourceShareAssociation{},
		},
	}
	checker := NewChecker(client)

	err := Preflight(context.Background(), checker, "123456789012")
	if err == nil {
		t.Fatal("expected error for missing association, got nil")
	}
	if !errors.Is(err, err) {
		t.Fatalf("unexpected error type: %v", err)
	}
	// Verify it wraps with the preflight context.
	expected := "connectivity preflight failed"
	if got := err.Error(); !contains(got, expected) {
		t.Errorf("error message %q does not contain %q", got, expected)
	}
	// Verify the underlying reason is about missing association.
	if got := err.Error(); !contains(got, "no VPC Lattice service network association found") {
		t.Errorf("error message %q does not describe missing association", got)
	}
}

func TestPreflight_InactiveAssociation(t *testing.T) {
	client := &mockRAMClient{
		output: &ram.GetResourceShareAssociationsOutput{
			ResourceShareAssociations: []ramtypes.ResourceShareAssociation{
				{Status: ramtypes.ResourceShareAssociationStatusAssociating},
			},
		},
	}
	checker := NewChecker(client)

	err := Preflight(context.Background(), checker, "123456789012")
	if err == nil {
		t.Fatal("expected error for inactive association, got nil")
	}
	if got := err.Error(); !contains(got, "connectivity preflight failed") {
		t.Errorf("error message %q does not contain preflight context", got)
	}
	if got := err.Error(); !contains(got, "no active VPC Lattice service network association found") {
		t.Errorf("error message %q does not describe inactive association", got)
	}
}

func TestPreflight_APIUnavailable(t *testing.T) {
	client := &mockRAMClient{
		err: errors.New("connection refused"),
	}
	checker := NewChecker(client)

	err := Preflight(context.Background(), checker, "123456789012")
	if err == nil {
		t.Fatal("expected error for API unavailability, got nil")
	}
	if got := err.Error(); !contains(got, "connectivity preflight failed") {
		t.Errorf("error message %q does not contain preflight context", got)
	}
	if got := err.Error(); !contains(got, "failed to query AWS RAM") {
		t.Errorf("error message %q does not describe RAM query failure", got)
	}
}

func TestPreflight_PermissionDenied(t *testing.T) {
	client := &mockRAMClient{
		err: errors.New("AccessDeniedException: User is not authorized to perform ram:GetResourceShareAssociations"),
	}
	checker := NewChecker(client)

	err := Preflight(context.Background(), checker, "123456789012")
	if err == nil {
		t.Fatal("expected error for permission denial, got nil")
	}
	if got := err.Error(); !contains(got, "connectivity preflight failed") {
		t.Errorf("error message %q does not contain preflight context", got)
	}
	if got := err.Error(); !contains(got, "AccessDeniedException") {
		t.Errorf("error message %q does not include permission denial detail", got)
	}
}

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
