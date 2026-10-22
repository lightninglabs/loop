package htlc_test

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightningnetwork/lnd/lntypes"
)

func ExampleNewSwapKit_cltv() {
	// For illustration, generate two parties' keys locally. In a swap the
	// parties exchange public keys and persist their negotiated terms.
	sender, err := btcec.NewPrivateKey()
	if err != nil {
		panic(err)
	}
	receiver, err := btcec.NewPrivateKey()
	if err != nil {
		panic(err)
	}

	// Sender is the client for Loop In and the server for Loop Out. This
	// example reuses each party's internal key for its script role; callers
	// may instead supply four distinct keys. AssetID and SwapHash below
	// stand in for the agreed asset and the Lightning invoice payment hash.
	kit, err := htlc.NewSwapKit(htlc.AssetHTLCCLTVV1, htlc.Params{
		SenderInternalPubKey:   sender.PubKey(),
		ReceiverInternalPubKey: receiver.PubKey(),
		SenderScriptKey:        sender.PubKey(),
		ReceiverScriptKey:      receiver.PubKey(),
		AssetID:                asset.ID{1},
		Amount:                 1000,
		SwapHash:               lntypes.Hash{2},
		RefundHeight:           1_000_000,
		AddressParams:          &address.RegressionNetTap,
	})
	if err != nil {
		panic(err)
	}

	// Set these on the proof-selected input and the Bitcoin transaction
	// before signing any inputs. The flow must enforce a safe deadline and
	// confirmation policy against the current chain height separately.
	claim := kit.SuccessSpendConstraints()
	refund := kit.TimeoutSpendConstraints()
	fmt.Printf("claim: sequence=%d locktime=%d\n", claim.Sequence, claim.LockTime)
	fmt.Printf("refund: sequence=%d locktime=%d\n", refund.Sequence, refund.LockTime)
	// Output:
	// claim: sequence=1 locktime=0
	// refund: sequence=4294967294 locktime=1000000
}
