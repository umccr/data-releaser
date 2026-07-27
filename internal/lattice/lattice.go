// Package lattice implements VPC Lattice and AWS RAM connectivity verification
// for researcher accounts.
package lattice

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ram"
	ramtypes "github.com/aws/aws-sdk-go-v2/service/ram/types"

	internalaws "github.com/umccr/data-releaser/internal/aws"
)

// Checker verifies that a researcher account has an active VPC Lattice service
// network association via AWS RAM. This is a prerequisite for share creation
// (Requirements 5.1, 5.2, 5.3, 5.4).
type Checker struct {
	ramClient internalaws.RAMClient
}

// NewChecker creates a new Checker with the given RAM client.
func NewChecker(ramClient internalaws.RAMClient) *Checker {
	return &Checker{ramClient: ramClient}
}

// VerifyAssociation checks that the researcher account has an active (ASSOCIATED)
// RAM resource share association as a principal. This confirms the VPC Lattice
// service network has been shared to the researcher account.
//
// Returns nil if an active association is found.
// Returns a descriptive error if:
//   - No association exists for the account (Req 5.3)
//   - The association exists but is not in ASSOCIATED status (Req 5.4)
//   - The RAM API is unavailable or permission is denied (Req 5.5)
func (c *Checker) VerifyAssociation(ctx context.Context, researcherAccountID string) error {
	input := &ram.GetResourceShareAssociationsInput{
		AssociationType: ramtypes.ResourceShareAssociationTypePrincipal,
		Principal:       &researcherAccountID,
	}

	output, err := c.ramClient.GetResourceShareAssociations(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to query AWS RAM for account %s: %w", researcherAccountID, err)
	}

	if len(output.ResourceShareAssociations) == 0 {
		return fmt.Errorf(
			"no VPC Lattice service network association found for account %s: "+
				"the RAM resource share must be created and accepted before shares can be granted",
			researcherAccountID,
		)
	}

	// Check if any association is in ASSOCIATED status.
	for _, assoc := range output.ResourceShareAssociations {
		if assoc.Status == ramtypes.ResourceShareAssociationStatusAssociated {
			return nil
		}
	}

	// Associations exist but none are in ASSOCIATED state — report the statuses found.
	statuses := make([]string, 0, len(output.ResourceShareAssociations))
	for _, assoc := range output.ResourceShareAssociations {
		statuses = append(statuses, string(assoc.Status))
	}

	return fmt.Errorf(
		"no active VPC Lattice service network association found for account %s: "+
			"found %d association(s) with status %v; the association must be in ASSOCIATED state",
		researcherAccountID,
		len(output.ResourceShareAssociations),
		statuses,
	)
}
