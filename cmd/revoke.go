package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/spf13/cobra"

	"github.com/umccr/data-releaser/internal/auth"
	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/manifest"
	"github.com/umccr/data-releaser/internal/model"
)

var revokeCmd = &cobra.Command{
	Use:   "revoke [share-id]",
	Short: "Revoke an active data share",
	Long: `Mark a Share_Record as revoked in the Authorization Service,
optionally flushing the API Gateway cache for immediate propagation.`,
	Args: cobra.ExactArgs(1),
	RunE: runRevoke,
}

func init() {
	revokeCmd.Flags().Bool("flush-cache", false, "Flush the API Gateway authorization cache for the affected researcher")

	rootCmd.AddCommand(revokeCmd)
}

func runRevoke(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shareID := args[0]

	flushCache, err := cmd.Flags().GetBool("flush-cache")
	if err != nil {
		return fmt.Errorf("reading --flush-cache flag: %w", err)
	}

	// --- Step 1: Load config and create AWS client factory ---
	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("configuration not loaded")
	}

	factory, err := internalaws.NewClientFactory(ctx, cfg.Profile, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return err
	}

	// --- Step 2: Revoke the share via auth service (Req 3.1) ---
	store := auth.NewShareStore(factory.DynamoDBClient(), ShareRecordsTableName)

	record, err := store.Revoke(ctx, shareID)
	if err != nil {
		// Handle specific state-conflict errors (Req 3.3, 3.5, 3.6, 3.7)
		switch {
		case errors.Is(err, auth.ErrShareNotFound):
			fmt.Fprintf(os.Stderr, "Error: share %q not found\n", shareID)
		case errors.Is(err, auth.ErrShareAlreadyRevoked):
			fmt.Fprintf(os.Stderr, "Error: share %q is already revoked\n", shareID)
		case errors.Is(err, auth.ErrShareExpired):
			fmt.Fprintf(os.Stderr, "Error: share %q has already expired\n", shareID)
		default:
			fmt.Fprintf(os.Stderr, "Error: failed to revoke share: %v\n", err)
		}
		return err
	}

	// --- Step 3: Best-effort cache flush (Req 3.8) ---
	if flushCache {
		flushErr := flushAPIGatewayCache(ctx, factory.APIGatewayClient(), cfg.Endpoints.APIGatewayRestAPIID, cfg.Endpoints.APIGatewayStageName)
		if flushErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: cache flush failed (revocation still applied): %v\n", flushErr)
			fmt.Fprintf(os.Stderr, "  Propagation is bounded by the Cache-TTL ceiling.\n")
		} else {
			fmt.Fprintf(os.Stderr, "Cache flushed for stage %q.\n", cfg.Endpoints.APIGatewayStageName)
		}
	}

	// --- Step 4: Regenerate manifest (Req 7.3, 7.4) ---
	allShares, err := store.ListByAccount(ctx, record.ResearcherAccountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to list shares for manifest regeneration: %v\n", err)
		// Don't fail the revoke command — the record was already revoked
	} else {
		now := time.Now().UTC()
		var activeShares []model.ShareRecord
		for _, s := range allShares {
			if s.EffectiveStatus(now) == model.ShareStatusActive {
				activeShares = append(activeShares, *s)
			}
		}

		generator := manifest.NewGenerator(cfg.Endpoints.HtsgetBaseURL)
		manifestDoc := generator.Generate(activeShares, now)

		bucket := researcherBucketName(record.ResearcherAccountID)
		writer := manifest.NewWriter(factory.S3Client())
		if syncErr := writer.Sync(ctx, bucket, manifestDoc); syncErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: manifest sync failed (revocation still applied): %v\n", syncErr)
		}
	}

	// --- Step 5: Output confirmation (Req 3.4) ---
	revokedAt := record.RevokedAt.Format(time.RFC3339)
	fmt.Printf("Share revoked successfully:\n")
	fmt.Printf("  Share ID:    %s\n", record.ShareID)
	fmt.Printf("  Revoked at:  %s\n", revokedAt)

	if !flushCache {
		fmt.Fprintf(os.Stderr, "Note: propagation is bounded by the Cache-TTL ceiling. Use --flush-cache for faster propagation.\n")
	}

	return nil
}

// flushAPIGatewayCache calls the API Gateway FlushStageCache API for the authorization
// service. This is a best-effort operation — failures are reported but do not cause the
// revoke command to fail.
// Requirements: 3.8
func flushAPIGatewayCache(ctx context.Context, client internalaws.APIGatewayClient, restAPIID, stageName string) error {
	if restAPIID == "" {
		return fmt.Errorf("apiGatewayRestApiId is not configured; cannot flush cache")
	}
	if stageName == "" {
		return fmt.Errorf("apiGatewayStageName is not configured; cannot flush cache")
	}

	_, err := client.FlushStageCache(ctx, &apigateway.FlushStageCacheInput{
		RestApiId: aws.String(restAPIID),
		StageName: aws.String(stageName),
	})
	if err != nil {
		return fmt.Errorf("FlushStageCache failed: %w", err)
	}

	return nil
}
