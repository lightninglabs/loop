package assets

import (
	"context"

	"github.com/lightninglabs/loop/assets/internal/policy"
)

// MinOnchainSwapTapdVersion is the oldest tapd release that may create new
// shared-kit swap addresses or verify an untrusted HTLC/deposit history. The
// kit checks the running tapd at those RPC boundaries. It does not gate
// ordinary asset payments or local claim/refund witness construction. Recovery
// still needs valid proofs and compliant new transfers; no recovery manager is
// implemented by this version check.
//
// The floor requires v0.8.5's proof activation and spender commitments plus
// v0.8.6's rejection of invalid internal keys in inclusion/exclusion proofs.
// Root locator proofs already existed in v0.8.4. See the policy definition for
// the dependency that must be updated when this minimum is raised.
const MinOnchainSwapTapdVersion = policy.MinOnchainSwapTapdVersion

// ErrTapdVersionUnsupported is returned when tapd is older than
// MinOnchainSwapTapdVersion or reports a version that cannot be parsed.
var ErrTapdVersionUnsupported = policy.ErrTapdVersionUnsupported

// CheckOnchainSwapTapdVersion checks a tapd GetInfo version against the shared
// kit's floor. Applications must also check it before offering or funding new
// swaps through APIs outside the kit. This function does not establish that an
// older backend can safely recover an existing swap.
func CheckOnchainSwapTapdVersion(version string) error {
	return policy.CheckOnchainSwapTapdVersion(version)
}

// TapdInfoClient is the narrow runtime version boundary shared with on-chain
// swap integrations that perform address creation, offering or funding outside
// the kit helpers.
type TapdInfoClient = policy.TapdInfoClient

// RequireOnchainSwapTapd checks the connected tapd immediately before starting
// an on-chain asset swap operation outside the kit. It leaves ordinary asset
// payments and local recovery signing independent of the version floor.
func RequireOnchainSwapTapd(ctx context.Context, client TapdInfoClient) error {
	return policy.RequireOnchainSwapTapd(ctx, client)
}
