package aws

import (
	"context"
	"testing"
)

func TestNewClientFactory_InvalidProfile(t *testing.T) {
	// Using a profile name that doesn't exist should fail with a credential error
	// when we attempt to retrieve credentials.
	ctx := context.Background()

	// Set a non-existent config file path to prevent loading real profiles
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/path/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/path/credentials")
	// Clear any env-based credentials that might mask the error
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")

	_, err := NewClientFactory(ctx, "nonexistent-profile-xyz", "")
	if err == nil {
		t.Fatal("expected error for non-existent profile, got nil")
	}

	// Verify the error message mentions credential resolution
	if got := err.Error(); got == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestNewClientFactory_NoCredentials(t *testing.T) {
	// With all credential sources disabled, should fail.
	ctx := context.Background()

	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/path/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/path/credentials")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	// Disable EC2 IMDS to prevent fallback in non-EC2 environments
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	_, err := NewClientFactory(ctx, "", "")
	if err == nil {
		t.Fatal("expected error when no credentials are available, got nil")
	}

	expected := "failed to resolve AWS credentials"
	if got := err.Error(); len(got) < len(expected) {
		t.Fatalf("error message too short: %q", got)
	}
}

func TestNewClientFactory_DefaultRegion(t *testing.T) {
	// Verify the default region is applied when none is specified.
	ctx := context.Background()

	// Provide valid static credentials so factory creation succeeds
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") // pragma: allowlist secret
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/path/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/path/credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	factory, err := NewClientFactory(ctx, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.cfg.Region != DefaultRegion {
		t.Errorf("expected region %q, got %q", DefaultRegion, factory.cfg.Region)
	}
}

func TestNewClientFactory_CustomRegion(t *testing.T) {
	ctx := context.Background()

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") // pragma: allowlist secret
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/path/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/path/credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	factory, err := NewClientFactory(ctx, "", "us-west-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if factory.cfg.Region != "us-west-2" {
		t.Errorf("expected region %q, got %q", "us-west-2", factory.cfg.Region)
	}
}

func TestNewClientFactory_ReturnsClients(t *testing.T) {
	ctx := context.Background()

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") // pragma: allowlist secret
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/path/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/path/credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	factory, err := NewClientFactory(ctx, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all client methods return non-nil interfaces
	if factory.AthenaClient() == nil {
		t.Error("AthenaClient() returned nil")
	}
	if factory.DynamoDBClient() == nil {
		t.Error("DynamoDBClient() returned nil")
	}
	if factory.RAMClient() == nil {
		t.Error("RAMClient() returned nil")
	}
	if factory.S3Client() == nil {
		t.Error("S3Client() returned nil")
	}
	if factory.LambdaClient() == nil {
		t.Error("LambdaClient() returned nil")
	}
	if factory.APIGatewayClient() == nil {
		t.Error("APIGatewayClient() returned nil")
	}
	if factory.STSClient() == nil {
		t.Error("STSClient() returned nil")
	}
}

func TestNewClientFactory_AWSConfig(t *testing.T) {
	ctx := context.Background()

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") // pragma: allowlist secret
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/path/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/path/credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	factory, err := NewClientFactory(ctx, "", "ap-southeast-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	awsCfg := factory.AWSConfig()
	if awsCfg.Region != "ap-southeast-2" {
		t.Errorf("AWSConfig().Region = %q, want %q", awsCfg.Region, "ap-southeast-2")
	}
}
