package tapsdk

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/internal/assetproof"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// auditCoAnchoredProofs builds the proofs of two assets that share one anchor
// output, as a request with a disclosed passive asset supplies them.
func auditCoAnchoredProofs(t *testing.T) (*proof.Proof, *proof.Proof) {
	t.Helper()
	genesis := bindingGenesis()
	group := &asset.GroupKey{GroupPubKey: *bindingKey(11).PubKey()}
	active, err := asset.New(genesis, 1000, 0, 0,
		asset.NewScriptKey(bindingKey(2).PubKey()), group,
		asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)
	passiveGenesis := genesis
	passiveGenesis.Tag += "-passive"
	passive, err := asset.New(passiveGenesis, 1, 0, 0,
		asset.NewScriptKey(bindingKey(9).PubKey()), nil,
		asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)

	version := commitment.TapCommitmentV2
	tree, err := commitment.FromAssets(&version, active, passive)
	require.NoError(t, err)
	internal := bindingKey(3).PubKey()
	pkScript, _, _, err := tapsend.AnchorOutputScript(internal, nil, tree)
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&genesis.FirstPrevOut, nil, nil))
	tx.AddTxOut(wire.NewTxOut(10_000, pkScript))

	build := func(a *asset.Asset, g asset.Genesis) *proof.Proof {
		_, inclusion, err := tree.Proof(a.TapCommitmentKey(),
			a.AssetCommitmentKey())
		require.NoError(t, err)
		return &proof.Proof{
			PrevOut: genesis.FirstPrevOut, AnchorTx: *tx, Asset: *a,
			BlockHeight: 100,
			BlockHeader: wire.BlockHeader{MerkleRoot: tx.TxHash()},
			InclusionProof: proof.TaprootProof{
				InternalKey: internal,
				CommitmentProof: &proof.CommitmentProof{
					Proof: *inclusion,
				},
			},
			GenesisReveal: &g,
		}
	}
	return build(active, genesis), build(passive, passiveGenesis)
}

// auditAltLeafWithoutScriptKey encodes the proof with one alt leaf and then
// retypes the alt leaf's script key record (16) to an unknown odd type, which
// tapd's decoder preserves instead of rejecting.
func auditAltLeafWithoutScriptKey(t *testing.T, p *proof.Proof) []byte {
	t.Helper()
	leaf, err := asset.NewAltLeaf(
		asset.NewScriptKey(bindingKey(7).PubKey()), asset.ScriptV0,
	)
	require.NoError(t, err)
	p.AltLeaves = []asset.AltLeaf[asset.Asset]{leaf}
	var encoded bytes.Buffer
	require.NoError(t, p.Encode(&encoded))
	raw := encoded.Bytes()
	magic := bytes.Clone(raw[:proof.PrefixMagicBytesLength])
	records := auditParseTLV(t, raw[proof.PrefixMagicBytesLength:])

	var found bool
	for i := range records {
		// Proof record 27 holds a count followed by one length-prefixed
		// TLV stream per alt leaf.
		if records[i].typ != 27 {
			continue
		}
		var buf [8]byte
		r := bytes.NewReader(records[i].value)
		count, err := tlv.ReadVarInt(r, &buf)
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		length, err := tlv.ReadVarInt(r, &buf)
		require.NoError(t, err)
		stream := make([]byte, length)
		_, err = r.Read(stream)
		require.NoError(t, err)

		inner := auditParseTLV(t, stream)
		for j := range inner {
			if inner[j].typ == 16 {
				inner[j].typ = 19
				found = true
			}
		}
		stream = auditEncodeTLV(t, inner)
		var value bytes.Buffer
		require.NoError(t, tlv.WriteVarInt(&value, 1, &buf))
		require.NoError(t, tlv.WriteVarInt(
			&value, uint64(len(stream)), &buf,
		))
		value.Write(stream)
		records[i].value = value.Bytes()
	}
	require.True(t, found)
	return auditProofFile(t, append(magic, auditEncodeTLV(t, records)...))
}

// TestAuditBAltLeafWithoutScriptKey shows that the co-anchored inventory
// check dereferences the script key of a disclosed alt leaf before any alt
// leaf validation. The proof file decodes and passes the provenance walk.
func TestInventoryRejectsInvalidAltLeaf(t *testing.T) {
	active, passive := auditCoAnchoredProofs(t)

	// The honest pair opens the complete anchor.
	require.NoError(t, assetproof.VerifyCompleteAnchors(
		[]*proof.Proof{active, passive},
	))

	raw := auditAltLeafWithoutScriptKey(t, active)
	crafted, err := LastProof(raw)
	require.NoError(t, err)
	require.Len(t, crafted.AltLeaves, 1)
	params := address.RegressionNetTap
	require.NoError(t, checkProvenanceWithParams(t.Context(), raw, &params))

	var checkErr error
	require.NotPanics(t, func() {
		checkErr = assetproof.VerifyCompleteAnchors(
			[]*proof.Proof{crafted, passive},
		)
	})
	require.Error(t, checkErr)
}
