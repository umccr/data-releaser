package validate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/ram"
	ramtypes "github.com/aws/aws-sdk-go-v2/service/ram/types"
)

func associatedRAM() *stubRAMClient {
	return &stubRAMClient{
		fn: func(ctx context.Context, params *ram.GetResourceShareAssociationsInput) (*ram.GetResourceShareAssociationsOutput, error) {
			return &ram.GetResourceShareAssociationsOutput{
				ResourceShareAssociations: []ramtypes.ResourceShareAssociation{
					{Status: ramtypes.ResourceShareAssociationStatusAssociated},
				},
			}, nil
		},
	}
}

func TestCheckRAMAssociation_Active(t *testing.T) {
	check := checkRAMAssociation(context.Background(), associatedRAM(), "534840902377")
	if !check.Passed {
		t.Fatalf("expected active association to pass, got: %s", check.Error)
	}
}

func TestCheckRAMAssociation_Missing(t *testing.T) {
	client := &stubRAMClient{
		fn: func(ctx context.Context, params *ram.GetResourceShareAssociationsInput) (*ram.GetResourceShareAssociationsOutput, error) {
			return &ram.GetResourceShareAssociationsOutput{}, nil
		},
	}
	check := checkRAMAssociation(context.Background(), client, "534840902377")
	if check.Passed {
		t.Fatal("expected a missing association to fail the check")
	}
	if check.Error == "" {
		t.Error("expected a descriptive error for the missing association")
	}
}

// TestValidateResearcher_ContinuesOnFailure verifies every researcher-side check runs
// even when one fails (Req 15.7).
func TestValidateResearcher_ContinuesOnFailure(t *testing.T) {
	// RAM association is missing (fails), but the other checks must still run.
	ramClient := &stubRAMClient{
		fn: func(ctx context.Context, params *ram.GetResourceShareAssociationsInput) (*ram.GetResourceShareAssociationsOutput, error) {
			return nil, errors.New("AccessDeniedException")
		},
	}

	opts := ValidateResearcherOpts{
		RAMClient:      ramClient,
		LambdaClient:   &stubLambdaClient{}, // no functions -> Test_Lambda unavailable
		S3Client:       &stubS3Client{},     // empty policy -> grant check fails
		AccountID:      "534840902377",
		ManifestBucket: "data-control-manifest-bucket",
		ManifestPrefix: "manifests/534840902377/*",
	}

	checks := ValidateResearcher(context.Background(), opts)

	ramCheck := findCheck(t, checks, "RAM Association")
	if ramCheck.Passed {
		t.Error("expected the RAM association check to fail")
	}
	// The Test_Lambda discovery check must still be present despite the earlier failure.
	discovery := findCheck(t, checks, "Test_Lambda Discovery")
	if !discovery.Passed {
		t.Errorf("expected Test_Lambda discovery to run and report UNAVAILABLE as a pass, got: %s", discovery.Error)
	}
	if !strings.Contains(discovery.Error, "UNAVAILABLE") {
		t.Errorf("expected an UNAVAILABLE note when no Test_Lambda is deployed, got: %q", discovery.Error)
	}
}

func TestResearcherCheckTestLambda_FoundAndInvoked(t *testing.T) {
	lambdaClient := testLambdaLister(func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return &lambda.InvokeOutput{
			Payload: []byte(`{"statusCode":200,"latticeResponse":{"htsgetAuth":[]}}`),
		}, nil
	})

	checks := researcherCheckTestLambda(context.Background(), lambdaClient)
	if len(checks) != 2 {
		t.Fatalf("expected discovery + invocation checks, got %d", len(checks))
	}
	for _, c := range checks {
		if !c.Passed {
			t.Errorf("expected check %q to pass, got: %s", c.Name, c.Error)
		}
	}
}

func TestResearcherCheckTestLambda_InvocationError(t *testing.T) {
	lambdaClient := testLambdaLister(func(ctx context.Context, params *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return nil, errors.New("invoke boom")
	})

	checks := researcherCheckTestLambda(context.Background(), lambdaClient)
	invocation := findCheck(t, checks, "Test_Lambda Invocation")
	if invocation.Passed {
		t.Error("expected the invocation check to fail when Invoke errors")
	}
}
