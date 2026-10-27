package assetproof

import (
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

func inventoryProofs(t *testing.T, seed byte, count int) []*proof.Proof {
	t.Helper()
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{seed}, Index: 1},
		Tag:          "inventory", Type: asset.Normal,
	}
	assets := make([]*asset.Asset, count)
	for idx := range assets {
		_, key := btcec.PrivKeyFromBytes([]byte{byte(idx + 1)})
		a, err := asset.New(genesis, 500, 0, 0, asset.NewScriptKey(key),
			nil, asset.WithAssetVersion(asset.V1))
		require.NoError(t, err)
		assets[idx] = a
	}
	version := commitment.TapCommitmentV2
	tree, err := commitment.FromAssets(&version, assets...)
	require.NoError(t, err)
	_, internal := btcec.PrivKeyFromBytes([]byte{20})
	root := tree.TapscriptRoot(nil)
	key := txscript.ComputeTaprootOutputKey(internal, root[:])
	script, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)
	anchor := wire.NewMsgTx(2)
	anchor.AddTxIn(wire.NewTxIn(&genesis.FirstPrevOut, nil, nil))
	anchor.AddTxOut(wire.NewTxOut(1_000, script))
	block := &wire.MsgBlock{Header: wire.BlockHeader{
		MerkleRoot: anchor.TxHash(), Nonce: uint32(seed),
	}, Transactions: []*wire.MsgTx{anchor}}
	blobs, err := proof.NewMintingBlobs(&proof.MintParams{
		Block: block, BlockHeight: 101, Tx: anchor,
		TxIndex: 0, OutputIndex: 0, InternalKey: internal,
		TaprootAssetRoot: tree, GenesisPoint: genesis.FirstPrevOut,
	}, proof.MockVerifierCtx,
		proof.WithGenOption(proof.WithVersion(proof.TransitionV1)))
	require.NoError(t, err)
	proofs := make([]*proof.Proof, count)
	for idx, a := range assets {
		proofs[idx] = blobs[asset.ToSerialized(a.ScriptKey.PubKey)]
		require.NotNil(t, proofs[idx])
	}
	return proofs
}

// TestDisclosedInventory proves that a known passive asset can be preserved,
// while a hidden co-anchor cannot authorize a supposedly complete transfer.
func TestDisclosedInventory(t *testing.T) {
	single := inventoryProofs(t, 1, 1)
	require.NoError(t, VerifyCompleteAnchors(single))
	_, err := VerifyCompleteAnchor(single[0])
	require.NoError(t, err)
	coanchored := inventoryProofs(t, 2, 2)
	require.Error(t, VerifyCompleteAnchors(coanchored[:1]))
	_, err = VerifyCompleteAnchor(coanchored[0])
	require.Error(t, err)
	require.NoError(t, VerifyCompleteAnchors(coanchored))
	combined := slices.Concat(single, coanchored)
	require.NoError(t, VerifyCompleteAnchors(combined))
	wrong := inventoryProofs(t, 3, 1)
	require.Error(t, VerifyCompleteAnchors([]*proof.Proof{
		coanchored[0], wrong[0],
	}))
}

// TestMalformedInventory verifies rejection before commitment code can
// dereference omitted untrusted proof fields.
func TestMalformedInventory(t *testing.T) {
	require.Error(t, VerifyCompleteAnchors(nil))
	malformedSplit := inventoryProofs(t, 1, 1)[0]
	malformedSplit.Asset.PrevWitnesses = []asset.Witness{{
		SplitCommitment: &asset.SplitCommitment{},
	}}
	for _, p := range []*proof.Proof{nil, {}, malformedSplit} {
		require.NotPanics(t, func() {
			_, err := VerifyCompleteAnchor(p)
			require.Error(t, err)
			require.Error(t, VerifyCompleteAnchors([]*proof.Proof{p}))
		})
	}
}
