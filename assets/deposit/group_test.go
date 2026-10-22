package deposit

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	assettest "github.com/lightninglabs/loop/assets/internal/test"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// TestDepositProofGroup checks trusted group metadata through all acceptance
// paths and refund signing, while retaining ID-only proof-kit compatibility.
func TestDepositProofGroup(t *testing.T) {
	tests := []struct {
		name          string
		grouped       bool
		trusted       bool
		expectedGroup string
		valid         bool
	}{
		{name: "matching ungrouped", trusted: true, valid: true},
		{
			name: "matching grouped", grouped: true, trusted: true,
			expectedGroup: testSameGroup, valid: true,
		},
		{
			name: "expected grouped but proof ungrouped", trusted: true,
			expectedGroup: testSameGroup,
		},
		{name: "expected ungrouped but proof grouped", grouped: true, trusted: true},
		{
			name: "different group", grouped: true, trusted: true,
			expectedGroup: "different",
		},
		{
			name: "opposite parity", grouped: true, trusted: true,
			expectedGroup: testOppositeGroup,
		},
		{name: "proof-only ungrouped", valid: true},
		{name: "proof-only grouped", grouped: true, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newWitnessFixture(t)
			p := f.proof
			groupSigner, _ := scalarKey(t, 40)
			group, _ := asset.RandGroupKeyWithSigner(
				t, groupSigner, p.Asset.Genesis, &p.Asset,
			)
			if test.grouped {
				p.Asset.GroupKey = group
				p.Asset.PrevWitnesses[0].TxWitness = group.Witness
				var err error
				p.GroupKeyReveal, err = asset.NewGroupKeyReveal(
					*group, p.Asset.ID(),
				)
				require.NoError(t, err)
				reanchorDepositProof(t, f.kit, p)
			}

			// Verify real issuance signatures and transaction inclusion,
			// so a malformed group history cannot explain rejection.
			p.BlockHeader.MerkleRoot = p.AnchorTx.TxHash()
			merkle, err := proof.NewTxMerkleProof(
				[]*wire.MsgTx{&p.AnchorTx}, 0,
			)
			require.NoError(t, err)
			p.TxMerkleProof = *merkle
			file, err := proof.NewFile(proof.V0, *p)
			require.NoError(t, err)
			verifierCtx := proof.MockVerifierCtx
			verifierCtx.MerkleVerifier = proof.DefaultMerkleVerifier
			_, err = file.Verify(t.Context(), verifierCtx)
			require.NoError(t, err)

			params := Params{
				SenderInternalPubKey:   f.kit.senderInternalPubKey,
				ReceiverInternalPubKey: f.kit.receiverInternalPubKey,
				SenderScriptKey:        f.kit.senderScriptKey,
				ReceiverScriptKey:      f.kit.receiverScriptKey,
				KeyLocator:             f.kit.keyLocator,
				AssetID:                p.Asset.ID(),
				CsvExpiry:              f.kit.csvExpiry,
				ChainParams:            f.kit.chainParams,
			}
			if test.trusted {
				params.Genesis = &p.Asset.Genesis
			}
			switch test.expectedGroup {
			case testSameGroup:
				params.GroupKey = &group.GroupPubKey
			case "different":
				_, params.GroupKey = scalarKey(t, 42)
			case testOppositeGroup:
				encoded := group.GroupPubKey.SerializeCompressed()
				encoded[0] ^= 1
				params.GroupKey, err = btcec.ParsePubKey(encoded)
				require.NoError(t, err)
			}
			kit, err := NewKit(params)
			require.NoError(t, err)
			check := func(err error) {
				t.Helper()
				if test.valid {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err,
						"asset proof group key does not match deposit")
				}
			}
			var original bytes.Buffer
			require.NoError(t, p.Encode(&original))
			_, err = kit.AnchorRootFromProofCommitment(p, p.Asset.Amount)
			check(err)
			outpoint := p.OutPoint()
			_, err = kit.VerifyProofFile(
				t.Context(), localProofVerifier{}, encodeProofFile(t, *p),
				&outpoint, p.Asset.Amount,
			)
			check(err)

			f.prevOutputs[f.assetInIndex].PkScript =
				p.AnchorTx.TxOut[p.InclusionProof.OutputIndex].PkScript
			f.packet.UnsignedTx.TxIn[f.assetInIndex].PreviousOutPoint = outpoint
			f.transfer = assettest.Sweep(
				t, []*proof.Proof{p}, f.packet, opTrueWitness(t),
			)
			signer := &localSigner{
				privateKey: f.funderKey, expectedInput: f.assetInIndex,
			}
			witness, err := kit.CreateTimeoutWitness(
				t.Context(), signer, p, f.packet, f.transfer,
			)
			check(err)
			if test.valid {
				require.NotNil(t, witness)
				require.Equal(t, 1, signer.calls)
			} else {
				require.Nil(t, witness)
				require.Zero(t, signer.calls)
			}
			var after bytes.Buffer
			require.NoError(t, p.Encode(&after))
			require.Equal(t, original.Bytes(), after.Bytes())
		})
	}
}
