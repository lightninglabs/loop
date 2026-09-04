package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/stretchr/testify/require"
)

func validNonOpTrueProof(t *testing.T) (*proof.Proof, *address.Tap) {
	t.Helper()

	_, scriptKey := btcec.PrivKeyFromBytes([]byte{1})
	_, internalKey := btcec.PrivKeyFromBytes([]byte{2})
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash: chainhash.Hash{1}, Index: 1,
		},
		Tag: "non OP_TRUE asset", OutputIndex: 0, Type: asset.Normal,
	}
	proofAsset, err := asset.New(
		genesis, 1, 0, 0, asset.NewScriptKey(scriptKey), nil,
		asset.WithAssetVersion(asset.V1),
	)
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(&version, proofAsset)
	require.NoError(t, err)
	_, commitmentProof, err := tapCommitment.Proof(
		proofAsset.TapCommitmentKey(), proofAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	anchorScript, err := tapscript.PayToAddrScript(
		*internalKey, nil, *tapCommitment,
	)
	require.NoError(t, err)
	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: genesis.FirstPrevOut,
	})
	anchorTx.AddTxOut(&wire.TxOut{
		Value: 1_000, PkScript: anchorScript,
	})
	assetProof := &proof.Proof{
		AnchorTx: *anchorTx, Asset: *proofAsset,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 0, InternalKey: internalKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof: *commitmentProof,
			},
		},
	}
	_, err = assetProof.VerifyProofs()
	require.NoError(t, err)

	return assetProof, &address.Tap{
		AssetID: genesis.ID(), Amount: 1,
		ChainParams: &address.RegressionNetTap,
	}
}

func validOpTrueProof(t *testing.T, genesis asset.Genesis,
	amount uint64, keyScalar byte) *proof.Proof {

	t.Helper()

	_, internalKey := btcec.PrivKeyFromBytes([]byte{keyScalar})
	opTrueScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	require.NoError(t, err)
	proofAsset, err := asset.New(
		genesis, amount, 0, 0,
		asset.NewScriptKey(opTrueScriptKey.PubKey), nil,
		asset.WithAssetVersion(asset.V1),
	)
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(&version, proofAsset)
	require.NoError(t, err)
	_, commitmentProof, err := tapCommitment.Proof(
		proofAsset.TapCommitmentKey(),
		proofAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	anchorScript, err := tapscript.PayToAddrScript(
		*internalKey, nil, *tapCommitment,
	)
	require.NoError(t, err)
	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: genesis.FirstPrevOut,
	})
	anchorTx.AddTxOut(&wire.TxOut{
		Value: 1_000, PkScript: anchorScript,
	})
	assetProof := &proof.Proof{
		AnchorTx: *anchorTx, Asset: *proofAsset,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 0, InternalKey: internalKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof: *commitmentProof,
			},
		},
	}
	_, err = assetProof.VerifyProofs()
	require.NoError(t, err)

	return assetProof
}

// TestCreateOpTrueSweepVpktValidation verifies malformed proof and destination
// inputs fail before virtual-packet construction can index into them.
func TestCreateOpTrueSweepVpktValidation(t *testing.T) {
	_, err := CreateOpTrueSweepVpkt(t.Context(), nil, nil)
	require.Error(t, err)

	proofs := []*proof.Proof{{}}
	_, err = CreateOpTrueSweepVpkt(t.Context(), proofs, nil)
	require.Error(t, err)

	addr := &address.Tap{Amount: 1, AssetID: asset.ID{1}}
	_, err = CreateOpTrueSweepVpkt(t.Context(), proofs, addr)
	require.Error(t, err)

	addr.ChainParams = &address.RegressionNetTap
	addr.Amount = 0
	_, err = CreateOpTrueSweepVpkt(t.Context(), proofs, addr)
	require.Error(t, err)

	addr.Amount = 1
	addr.AssetID = asset.ZeroID
	_, err = CreateOpTrueSweepVpkt(t.Context(), proofs, addr)
	require.Error(t, err)

	addr.AssetID = asset.ID{1}
	_, err = CreateOpTrueSweepVpkt(
		t.Context(), []*proof.Proof{nil}, addr,
	)
	require.Error(t, err)

	_, err = CreateOpTrueSweepVpkt(t.Context(), proofs, addr)
	require.Error(t, err)

	nonOpTrueProof, matchingAddr := validNonOpTrueProof(t)
	_, err = CreateOpTrueSweepVpkt(
		t.Context(), []*proof.Proof{nonOpTrueProof}, matchingAddr,
	)
	require.ErrorContains(t, err, "is not an OP_TRUE asset")

	matchingAddr.Version = address.V2
	_, err = CreateOpTrueSweepVpkt(
		t.Context(), []*proof.Proof{nonOpTrueProof}, matchingAddr,
	)
	require.ErrorContains(t, err, "version 2")
}

// TestCreateOpTrueSweepVpktMultipleInputs verifies every asset input receives
// its OP_TRUE virtual witness and duplicate anchor inputs are rejected.
func TestCreateOpTrueSweepVpktMultipleInputs(t *testing.T) {
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash: chainhash.Hash{9}, Index: 1,
		},
		Tag: "multi-input OP_TRUE asset", OutputIndex: 0,
		Type: asset.Normal,
	}
	proofs := []*proof.Proof{
		validOpTrueProof(t, genesis, 2, 11),
		validOpTrueProof(t, genesis, 3, 12),
	}
	_, destinationScriptKey := btcec.PrivKeyFromBytes([]byte{21})
	_, destinationInternalKey := btcec.PrivKeyFromBytes([]byte{22})
	addr := &address.Tap{
		Version:      address.V1,
		AssetVersion: asset.V1,
		AssetID:      genesis.ID(),
		ScriptKey:    *destinationScriptKey,
		InternalKey:  *destinationInternalKey,
		Amount:       5,
		ChainParams:  &address.RegressionNetTap,
	}

	packet, err := CreateOpTrueSweepVpkt(t.Context(), proofs, addr)
	require.NoError(t, err)
	require.Len(t, packet.Outputs, 1)
	witnesses, err := packet.Outputs[0].PrevWitnesses()
	require.NoError(t, err)
	require.Len(t, witnesses, len(proofs))
	for _, witness := range witnesses {
		require.Len(t, witness.TxWitness, 2)
	}

	addr.Amount = 4
	_, err = CreateOpTrueSweepVpkt(
		t.Context(), []*proof.Proof{proofs[0], proofs[0]}, addr,
	)
	require.ErrorContains(t, err, "duplicates an input outpoint")
}
