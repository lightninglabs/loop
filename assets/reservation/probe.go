package reservation

import (
	"crypto/sha256"

	"github.com/lightningnetwork/lnd/lntypes"
)

// ProbeHash binds the sole probe invoice to a client-generated purchase ID.
// As with conventional Loop In probes, flipping a bit of the digest ensures
// the known input is not its preimage. Neither party can settle this invoice.
func ProbeHash(id ID) lntypes.Hash {
	hash := sha256.Sum256(id[:])
	hash[0] ^= 1
	return hash
}
