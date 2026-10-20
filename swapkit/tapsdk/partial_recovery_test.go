package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

type partialRecoveryChain struct {
	*bindingChain

	spend func(wire.OutPoint) (*wire.MsgBlock, uint32, error)
}

func (c *partialRecoveryChain) Spend(_ context.Context, point wire.OutPoint,
	_ []byte, _ uint32) (*wire.MsgBlock, uint32, error) {

	return c.spend(point)
}

type partialRecoverySDK struct {
	*bindingSDK

	declared error
}

func (s *partialRecoverySDK) DeclareScriptKey(context.Context,
	*sdk.DeclareScriptKeyRequest) (*sdk.ScriptKey, error) {

	return nil, s.declared
}

// TestPartialFallbackRecoveryDiscovery checks the server can discover either
// refund path with only the unsigned fallback saved. The marker stops at the
// proof reconstruction boundary; native tests cover the actual tapd archive.
func TestPartialFallbackRecoveryDiscovery(t *testing.T) {
	for _, original := range []bool{false, true} {
		name := "htlc refund"
		if original {
			name = "original deposit refund"
		}
		t.Run(name, func(t *testing.T) {
			driver, swap, outcome, chain := bindingOutcome(t, true)
			if original {
				driver, swap, outcome, chain =
					originalRefundFixture(t)
			}
			deposit, err := driver.localDeposit(t.Context(),
				swap.Offer.Terms.DepositOutpoints[0])
			require.NoError(t, err)
			base, err := lastProof(deposit.Deposit.Proof)
			require.NoError(t, err)

			// The persisted public package fixes the anchor even
			// though no completed Bitcoin witness was received.
			anchor := wire.NewMsgTx(2)
			point := base.OutPoint()
			anchor.AddTxIn(wire.NewTxIn(&point, nil, nil))
			anchor.AddTxOut(wire.NewTxOut(9000, []byte{0x51}))
			packet, err := psbt.NewFromUnsignedTx(anchor)
			require.NoError(t, err)
			raw, err := encodePSBT(packet)
			require.NoError(t, err)
			if original {
				swap.Fallback.Package =
					&sdk.CustomAnchorTransferPackage{
						Outputs: []sdk.CustomAnchorAssetOutputSummary{{
							AnchorOutpoint: sdk.Outpoint{
								Txid: sdk.Hash(anchor.TxHash()),
							},
						}},
					}
			}
			swap.Fallback.Package.AnchorPsbt = raw
			swap.Fallback.Final = nil
			prior := &swap.Fallback.Package.Outputs[0]
			prior.AnchorOutpoint.Txid = sdk.Hash(anchor.TxHash())
			htlcPoint := sdkPoint(prior.AnchorOutpoint)
			if !original {
				outcome.AnchorTx.TxIn[0].PreviousOutPoint = htlcPoint
			}
			marker := errors.New("reached proof reconstruction")
			driver.cfg.SDK = &partialRecoverySDK{
				bindingSDK: &bindingSDK{valid: true},
				declared:   marker,
			}
			var watched []wire.OutPoint
			driver.cfg.Chain = &partialRecoveryChain{
				bindingChain: chain,
				spend: func(point wire.OutPoint) (
					*wire.MsgBlock, uint32, error) {

					watched = append(watched, point)
					if original && point == htlcPoint {
						return nil, 0, swapkit.ErrPending
					}
					return bindingBlock(outcome),
						outcome.BlockHeight, nil
				},
			}
			// Each invocation starts with a fresh, stateless driver.
			driver = &Driver{cfg: driver.cfg}
			_, err = driver.discoverOutcome(
				t.Context(), swap, true,
			)
			if original {
				require.ErrorIs(t, err, swapkit.ErrPending)
				_, err = driver.discoverOriginalRefund(
					t.Context(), swap,
				)
				require.Equal(t, []wire.OutPoint{
					htlcPoint, base.OutPoint(),
				}, watched)
			} else {
				require.Equal(t, []wire.OutPoint{
					htlcPoint,
				}, watched)
			}
			require.ErrorIs(t, err, marker)
		})
	}
}
