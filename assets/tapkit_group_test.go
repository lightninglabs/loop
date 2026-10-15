package assets

import (
	"bytes"
	"fmt"
	"net/url"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// sweepGroupProof creates a proof with a real issuance group signature when
// groupSigner is supplied. Local group metadata must not affect matching.
func sweepGroupProof(t *testing.T, genesis asset.Genesis, amount uint64,
	anchorScalar byte, groupSigner *btcec.PrivateKey) *proof.Proof {

	t.Helper()

	p := validOpTrueProof(t, genesis, amount, anchorScalar)
	if groupSigner == nil {
		return p
	}
	p.Asset.GroupKey, _ = asset.RandGroupKeyWithSigner(
		t, groupSigner, genesis, &p.Asset,
	)
	p.Asset.PrevWitnesses[0].TxWitness = p.Asset.GroupKey.Witness
	version := commitment.TapCommitmentV2
	c, err := commitment.FromAssets(&version, &p.Asset)
	require.NoError(t, err)
	_, cp, err := c.Proof(
		p.Asset.TapCommitmentKey(), p.Asset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	p.InclusionProof.CommitmentProof = &proof.CommitmentProof{Proof: *cp}
	p.AnchorTx.TxOut[0].PkScript, err = tapscript.PayToAddrScript(
		*p.InclusionProof.InternalKey, nil, *c,
	)
	require.NoError(t, err)
	_, err = p.VerifyProofs()
	require.NoError(t, err)

	return p
}

// TestSweepDestinationGroup checks each input against the encoded address's
// group, then verifies valid sweeps anchor to the address's predicted script.
func TestSweepDestinationGroup(t *testing.T) {
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{1}},
		Tag:          "sweep destination group", Type: asset.Normal,
	}
	groupSigner, _ := btcec.PrivKeyFromBytes([]byte{40})
	otherSigner, _ := btcec.PrivKeyFromBytes([]byte{41})
	grouped := sweepGroupProof(t, genesis, 2, 11, groupSigner)
	secondGrouped := sweepGroupProof(t, genesis, 3, 12, groupSigner)
	ungrouped := sweepGroupProof(t, genesis, 2, 13, nil)
	secondUngrouped := sweepGroupProof(t, genesis, 3, 14, nil)
	otherGrouped := sweepGroupProof(t, genesis, 3, 15, otherSigner)
	groupKey := &grouped.Asset.GroupKey.GroupPubKey
	otherKey := &otherGrouped.Asset.GroupKey.GroupPubKey
	negated := groupKey.SerializeCompressed()
	negated[0] ^= 1
	oppositeKey, err := btcec.ParsePubKey(negated)
	require.NoError(t, err)
	_, scriptKey := btcec.PrivKeyFromBytes([]byte{21})
	_, internalKey := btcec.PrivKeyFromBytes([]byte{22})
	tests := []struct {
		name     string
		proofs   []*proof.Proof
		groupKey *btcec.PublicKey
		badIndex int
	}{
		{
			name: "matching ungrouped", badIndex: -1,
			proofs: []*proof.Proof{ungrouped, secondUngrouped},
		},
		{
			name: "matching grouped", badIndex: -1, groupKey: groupKey,
			proofs: []*proof.Proof{grouped, secondGrouped},
		},
		{
			name: "ungrouped input to grouped address", groupKey: groupKey,
			proofs: []*proof.Proof{ungrouped},
		},
		{
			name:   "grouped input to ungrouped address",
			proofs: []*proof.Proof{grouped},
		},
		{
			name: "different group", groupKey: otherKey,
			proofs: []*proof.Proof{grouped},
		},
		{
			name: "opposite parity", groupKey: oppositeKey,
			proofs: []*proof.Proof{grouped},
		},
		{
			name: "second input ungrouped", groupKey: groupKey, badIndex: 1,
			proofs: []*proof.Proof{grouped, secondUngrouped},
		},
		{
			name: "second input different group", groupKey: groupKey,
			badIndex: 1, proofs: []*proof.Proof{grouped, otherGrouped},
		},
	}
	for _, version := range []address.Version{address.V0, address.V1} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("v%d/%s", version, test.name), func(t *testing.T) {
				addr := &address.Tap{
					Version: version, AssetVersion: asset.V1,
					AssetID: genesis.ID(), GroupKey: test.groupKey,
					ScriptKey: *scriptKey, InternalKey: *internalKey,
					ProofCourierAddr: url.URL{
						Scheme: "hashmail", Host: "localhost:10001",
					},
					ChainParams: &address.RegressionNetTap,
				}
				for _, p := range test.proofs {
					addr.Amount += p.Asset.Amount
				}
				encoded, err := addr.EncodeAddress()
				require.NoError(t, err)
				addr, err = address.DecodeAddress(
					encoded, &address.RegressionNetTap,
				)
				require.NoError(t, err)
				addr.AttachGenesis(genesis)
				encodeProofs := func() []byte {
					var encoded bytes.Buffer
					for _, p := range test.proofs {
						require.NoError(t, p.Encode(&encoded))
					}
					return encoded.Bytes()
				}
				before := encodeProofs()
				packet, err := CreateOpTrueSweepVpkt(
					t.Context(), test.proofs, addr,
				)
				require.Equal(t, before, encodeProofs())
				afterAddress, encodeErr := addr.EncodeAddress()
				require.NoError(t, encodeErr)
				require.Equal(t, encoded, afterAddress)
				if test.badIndex >= 0 {
					require.ErrorContains(t, err, fmt.Sprintf(
						"asset proof %d group key does not match "+
							"sweep address", test.badIndex,
					))
					require.Nil(t, packet)
					return
				}
				require.NoError(t, err)
				outputKey, err := addr.TaprootOutputKey()
				require.NoError(t, err)
				expectedScript, err := txscript.PayToTaprootScript(outputKey)
				require.NoError(t, err)
				packet.Outputs[0].AnchorOutputInternalKey = internalKey
				packets := []*tappsbt.VPacket{packet}
				commitments, err := tapsend.CreateOutputCommitments(
					packets, tapsend.WithSpenderLeaves(),
				)
				require.NoError(t, err)
				actualScript, _, _, err := tapsend.AnchorOutputScript(
					packet.Outputs[1].AnchorOutputInternalKey,
					packet.Outputs[1].AnchorOutputTapscriptSibling,
					commitments[packet.Outputs[1].AnchorOutputIndex],
				)
				require.NoError(t, err)
				require.Equal(t, expectedScript, actualScript)
			})
		}
	}
}
