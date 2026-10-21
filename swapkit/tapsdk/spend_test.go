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
			tx.Final = nil
			requests, err := tx.Package.SigningRequests()
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
			r := &Driver{cfg: DriverConfig{Signer: wallet}}
			err = r.signScriptSpend(t.Context(), nil, &tx,
				keychain.KeyLocator{}, nil)
			if lostReply {
				require.ErrorIs(t, err, lost)
				require.Empty(t, tx.Final)
				r = &Driver{cfg: DriverConfig{Signer: wallet}}
				err = r.signScriptSpend(t.Context(), nil, &tx,
					keychain.KeyLocator{}, nil)
			}
			require.NoError(t, err)
			final := tx.Final
			require.NotEmpty(t, final)
			stored, err := json.Marshal(tx)
			require.NoError(t, err)
			tx = transaction{}
			require.NoError(t, json.Unmarshal(stored, &tx))
			require.Equal(t, final, tx.Final)
			r = &Driver{cfg: DriverConfig{Signer: wallet}}
			calls := wallet.calls
			err = r.signScriptSpend(t.Context(), nil, &tx,
				keychain.KeyLocator{}, nil)
			require.NoError(t, err)
			require.Equal(t, calls, wallet.calls)
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
	driver, swap, _, _ := bindingOutcome(t, true)
	var contract depositContract
	require.NoError(t, json.Unmarshal(swap.Offer.Terms.Contract, &contract))
	output := swap.Fallback.Package.Outputs[0]
	output.OPTrueSpend = contract.Output.OPTrueSpend
	policy, err := htlc(swap.Offer, contract)
	require.NoError(t, err)
	anchor := policy.AnchorPlan()
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
		&sibling, sdkPoint(contract.Output.AnchorOutpoint), 140, false)
	previous.AnchorTx.TxOut[0].Value = output.AnchorValueSat
	previous.BlockHeader.MerkleRoot = previous.AnchorTx.TxHash()
	output.TaprootAssetRoot, _, err = isolatedRoots(previous)
	require.NoError(t, err)
	spend, err := policy.TimeoutSpend(output.TaprootAssetRoot)
	require.NoError(t, err)
	proof := bindingRaw(t, previous)
	fee := uint64(contract.Output.AnchorValueSat - output.AnchorValueSat)
	driver.cfg.SDK = declaringRecoveryWallet{}
	before, err := driver.recoveryPlan(t.Context(), output, proof,
		contract.ClientKey, keychain.KeyLocator{}, spend, fee)
	require.NoError(t, err)
	// Recovery takes its fee from the stored fallback, including after a
	// fresh driver is built under a lower cap and a higher target fee.
	restarted := &Driver{cfg: driver.cfg}
	restarted.cfg.FeeSat, restarted.cfg.MaxFeeSat = 5000, 1
	after, err := restarted.recoveryPlan(t.Context(), output, proof,
		contract.ClientKey, keychain.KeyLocator{}, spend, fee)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.EqualValues(t, 8000, after.Outputs[0].AnchorValueSat)
}
