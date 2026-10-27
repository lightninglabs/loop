package tapsdk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
		name := "HTLC refund"
		if original {
			name = "original deposit refund"
		}
		t.Run(name, func(t *testing.T) {
			node, swap, outcome, chain := bindingOutcome(t, true)
			if original {
				node, swap, outcome, chain =
					originalRefundFixture(t)
			}
			deposit := swap.Deposit
			base, err := LastProof(deposit.Deposit.Proof)
			require.NoError(t, err)

			// The persisted public package fixes the anchor even
			// though no completed Bitcoin witness was received.
			anchor := wire.NewMsgTx(2)
			point := base.OutPoint()
			anchor.AddTxIn(wire.NewTxIn(&point, nil, nil))
			anchor.AddTxOut(wire.NewTxOut(9000, []byte{0x51}))
			packet, err := psbt.NewFromUnsignedTx(anchor)
			require.NoError(t, err)
			raw, err := EncodePSBT(packet)
			require.NoError(t, err)
			if original {
				swap.Fallback =
					&sdk.CustomAnchorTransferPackage{
						Outputs: []sdk.CustomAnchorAssetOutputSummary{
							{
								AnchorOutpoint: sdk.Outpoint{
									Txid: sdk.Hash(
										anchor.TxHash(),
									),
								},
							},
						},
					}
			}
			swap.Fallback.AnchorPsbt = raw
			prior := &swap.Fallback.Outputs[0]
			prior.AnchorOutpoint.Txid = sdk.Hash(anchor.TxHash())
			htlcPoint := SDKPoint(prior.AnchorOutpoint)
			if !original {
				outcome.AnchorTx.TxIn[0].PreviousOutPoint = htlcPoint
			}
			marker := errors.New("reached proof reconstruction")
			node.client = &partialRecoverySDK{
				bindingSDK: &bindingSDK{valid: true},
				declared:   marker,
			}
			var watched []wire.OutPoint
			node.chain = &partialRecoveryChain{
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
			// Each invocation reuses only the current operation's data.
			_, err = DiscoverOutcome(
				t.Context(),
				node.wallet(),
				node.chain,
				node.minConfs,
				swap,
				true,
			)
			if original {
				require.ErrorIs(t, err, swapkit.ErrPending)
				_, err = DiscoverOriginalRefund(
					t.Context(),
					node.wallet(),
					node.chain,
					node.minConfs,
					swap,
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

// observedRecoverySDK records the exact tapd logging request, then stops before
// proof reconstruction, which is covered by the real-node tests.
type observedRecoverySDK struct {
	*partialRecoverySDK
	sdk.CustomAnchorWalletKitClient

	input   *sdk.ProofFile
	point   sdk.Outpoint
	logged  *sdk.PublishAndLogTransferRequest
	stopErr error
}

func (s *observedRecoverySDK) ExportProof(_ context.Context, _ sdk.AssetRef,
	_ sdk.PubKey, point *sdk.Outpoint) (*sdk.ProofFile, error) {

	if point != nil && *point == s.point {
		return s.input, nil
	}
	return nil, errors.New("proof not yet logged")
}

func (*observedRecoverySDK) CustomAnchorCapabilities(context.Context) (
	*sdk.CustomAnchorCapabilities, error) {

	capabilities := sdk.DefaultTapdCustomAnchorCapabilities()
	return &capabilities, nil
}

func (s *observedRecoverySDK) PublishAndLogTransferWithRequest(
	_ context.Context, request *sdk.PublishAndLogTransferRequest) (
	*sdk.AssetPacket, error) {

	s.logged = request
	return nil, s.stopErr
}

// TestObservedSignedFallbackDoesNotRebroadcast exercises the discovery path
// used by InspectRefund when a server restarts after its peer's refund mined.
// Even a complete signed fallback must use observer logging for its lineage.
func TestObservedSignedFallbackDoesNotRebroadcast(t *testing.T) {
	node, swap, outcome, chain := bindingOutcome(t, true)
	pkg := signingPackage(t)
	pkg.Publish.SkipAnchorTxBroadcast = false
	pkg.Publish.ExternalBroadcast = false
	pkg, err := pkg.Seal()
	require.NoError(t, err)
	signed, err := signTestPackage(
		t.Context(),
		signingWallet(t, 2),
		signingWallet(t, 3),
		pkg,
	)
	require.NoError(t, err)
	fallback, err := pkg.WithFinalAnchorPSBT(
		finishSigningFixture(t, signed),
	)
	require.NoError(t, err)
	swap.Fallback = fallback

	// Reuse the existing package's fully signed anchor. The chain stub
	// reports the agreed timeout leaf spending that anchor's asset output.
	prior := fallback.Outputs[0]
	var contract DepositContract
	require.NoError(t, json.Unmarshal(swap.Offer.Terms.Contract, &contract))
	policy, err := HTLC(swap.Offer, contract)
	require.NoError(t, err)
	spend, err := policy.TimeoutSpend(prior.TaprootAssetRoot)
	require.NoError(t, err)
	point := SDKPoint(prior.AnchorOutpoint)
	outcome.AnchorTx.TxIn[0].PreviousOutPoint = point
	outcome.AnchorTx.TxIn[0].Witness = [][]byte{
		make([]byte, 64), spend.LeafScript, spend.ControlBlock,
	}
	node.chain = &partialRecoveryChain{
		bindingChain: chain,
		spend: func(actual wire.OutPoint) (*wire.MsgBlock, uint32, error) {
			require.Equal(t, point, actual)
			return bindingBlock(outcome), outcome.BlockHeight, nil
		},
	}

	inputRaw := fallback.Inputs[0].ProofSource.Blob
	inputProof, err := LastProof(inputRaw)
	require.NoError(t, err)
	client := &observedRecoverySDK{
		partialRecoverySDK: &partialRecoverySDK{
			bindingSDK: &bindingSDK{valid: true},
		},
		input: &sdk.ProofFile{RawProofFile: inputRaw},
		point: sdk.Outpoint{Txid: sdk.Hash(inputProof.OutPoint().Hash),
			Index: inputProof.OutPoint().Index},
		stopErr: errors.New("reached observer log"),
	}
	node.client = client
	_, err = DiscoverOutcome(
		t.Context(),
		node.wallet(),
		node.chain,
		node.minConfs,
		swap,
		true,
	)
	require.ErrorIs(t, err, client.stopErr)
	require.NotNil(t, client.logged)
	require.True(t, client.logged.SkipAnchorTxBroadcast)
	// Logging flags belong to this observation, not the persisted package.
	require.False(t, fallback.Publish.SkipAnchorTxBroadcast)
}
