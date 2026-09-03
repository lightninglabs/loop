package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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
}
