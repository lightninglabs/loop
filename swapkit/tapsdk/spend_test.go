package tapsdk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

type scriptWallet struct {
	lndclient.SignerClient

	signature []byte
	failure   error
	calls     int
}

func (w *scriptWallet) SignOutputRawKeyLocator(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor, []*wire.TxOut) ([][]byte, error) {

	w.calls++
	if w.failure != nil {
		err := w.failure
		w.failure = nil
		return nil, err
	}
	return [][]byte{w.signature}, nil
}

// TestScriptSpendRecovery checks retry after a lost signer reply and reuse of
// the completed transaction after the owning FSM saves it.
func TestScriptSpendRecovery(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "completed package"
		if lostReply {
			name = "lost signer reply"
		}
		t.Run(name, func(t *testing.T) {
			tx := scriptPackage(t)
			requests, err := tx.SigningRequests()
			require.NoError(t, err)
			require.Len(t, requests.ScriptPath, 1)
			request := requests.ScriptPath[0]
			signature, err := schnorr.Sign(
				bindingKey(2), request.Sighash[:],
			)
			require.NoError(t, err)
			lost := errors.New("simulated lost signer reply")
			wallet := &scriptWallet{signature: signature.Serialize()}
			if lostReply {
				wallet.failure = lost
			}
			signed, err := FinalizeScriptSpend(t.Context(), wallet, keychain.KeyLocator{}, tx, nil)
			if lostReply {
				require.ErrorIs(t, err, lost)
				require.Nil(t, signed)
				signed, err = FinalizeScriptSpend(t.Context(), wallet, keychain.KeyLocator{}, tx, nil)
			}
			require.NoError(t, err)
			final := signed.AnchorPsbt
			require.NotEmpty(t, final)
			stored, err := signed.MarshalBinary()
			require.NoError(t, err)
			restored, err := DecodeFinalPackage(stored)
			require.NoError(t, err)
			require.Equal(t, final, restored.AnchorPsbt)
			require.Equal(t, signed.CommittedPackageDigest,
				restored.CommittedPackageDigest)
		})
	}
}

type declaringRecoveryWallet struct{ sdk.Client }

func (declaringRecoveryWallet) DeclareScriptKey(context.Context,
	*sdk.DeclareScriptKeyRequest) (*sdk.ScriptKey, error) {

	return nil, nil
}

// TestRecoveryPlanIgnoresChangedFeeConfiguration verifies the transaction that
// is rebuilt after restart stays identical when operator fee limits change.
func TestRecoveryPlanIgnoresChangedFeeConfiguration(t *testing.T) {
	node, swap, _, _ := bindingOutcome(t, true)
	var contract DepositContract
	require.NoError(t, json.Unmarshal(swap.Offer.Terms.Contract, &contract))
	output := swap.Fallback.Outputs[0]
	output.OPTrueSpend = contract.Output.OPTrueSpend
	policy, err := HTLC(swap.Offer, contract)
	require.NoError(t, err)
	anchor := AnchorPlan(policy.AnchorPlan())
	require.Len(t, anchor.Tapscript.TapLeaves, 2)
	branch := txscript.NewTapBranch(
		txscript.NewBaseTapLeaf(anchor.Tapscript.TapLeaves[0].Script),
		txscript.NewBaseTapLeaf(anchor.Tapscript.TapLeaves[1].Script),
	)
	sibling := commitment.NewPreimageFromBranch(branch)
	internal, err := btcec.ParsePubKey(anchor.InternalKey.PubKey[:])
	require.NoError(t, err)
	script, err := btcec.ParsePubKey(contract.Output.ScriptKey[:])
	require.NoError(t, err)
	previous := bindingProof(t, bindingGenesis(), script, internal,
		&sibling, SDKPoint(contract.Output.AnchorOutpoint), 140, false)
	previous.AnchorTx.TxOut[0].Value = output.AnchorValueSat
	previous.BlockHeader.MerkleRoot = previous.AnchorTx.TxHash()
	output.TaprootAssetRoot, _, err = isolatedRoots(previous)
	require.NoError(t, err)
	spend, err := policy.TimeoutSpend(output.TaprootAssetRoot)
	require.NoError(t, err)
	proof := bindingRaw(t, previous)
	fee := uint64(contract.Output.AnchorValueSat - output.AnchorValueSat)
	node.client = declaringRecoveryWallet{}
	before, err := RecoveryRequest(t.Context(), node.wallet(), output, proof,
		contract.ClientKey, keychain.KeyLocator{}, spend, fee)
	require.NoError(t, err)
	// Recovery only accepts the agreed fee. No daemon fee configuration
	// crosses this API when the same unsigned transfer is rebuilt.
	after, err := RecoveryRequest(t.Context(), node.wallet(), output, proof,
		contract.ClientKey, keychain.KeyLocator{}, spend, fee)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.EqualValues(t, 8000, after.Outputs[0].AnchorValueSat)
}
