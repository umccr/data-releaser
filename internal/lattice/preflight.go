// Package lattice implements VPC Lattice and AWS RAM connectivity verification
// for researcher accounts.

package lattice

import (
	"context"
	"fmt"
)

// Preflight runs the connectivity preflight check that MUST be called as the first
// step of the share flow, before any Athena query or share record creation.
//
// It verifies that the researcher account has an active VPC Lattice service network
// association via AWS RAM. If this check fails, the share command must refuse to
// proceed and report the reason to the operator.
//
// Error conditions:
//   - No active RAM association: share refused with descriptive message about missing connectivity
//   - RAM API unavailable or permission denied: share refused with error indicating verification failure
//
// Returns nil if the preflight check passes (active association found).
func Preflight(ctx context.Context, checker *Checker, researcherAccountID string) error {
	if err := checker.VerifyAssociation(ctx, researcherAccountID); err != nil {
		return fmt.Errorf("connectivity preflight failed for account %s: %w", researcherAccountID, err)
	}
	return nil
}
