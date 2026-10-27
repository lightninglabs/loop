package reservation

import (
	"context"

	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

// CreateOpTrueSweepVpkt preserves the existing tapd packet layout for a
// reservation spend while its Bitcoin anchor uses the shared swap kit.
func CreateOpTrueSweepVpkt(ctx context.Context, proofs []*proof.Proof,
	addr *address.Tap) (*tappsbt.VPacket, error) {

	return assets.CreateOpTrueSweepVpkt(ctx, proofs, addr)
}
