package tapsdk

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// confirmationBlockFor wraps the given transactions into a minimal
// serialized block. Header fields other than structure are irrelevant:
// chain anchoring is validated by the importing backend, not by the
// assembler.
func confirmationBlockFor(t *testing.T, txs ...*wire.MsgTx) []byte {
	t.Helper()

	block := wire.MsgBlock{Header: wire.BlockHeader{Version: 2}}
	for _, tx := range txs {
		require.NoError(t, block.AddTransaction(tx))
	}

	// A single-transaction block's merkle root is that transaction's
	// txid, which is all the assembled proof's Verify needs here.
	if len(txs) == 1 {
		block.Header.MerkleRoot = txs[0].TxHash()
	}

	var encoded bytes.Buffer
	require.NoError(t, block.Serialize(&encoded))

	return encoded.Bytes()
}

// TestConfirmProofFile verifies the confirmed proof file assembly: the base
// file is extended by one proof per step, each carrying the block header,
// height, and merkle inclusion proof of its anchor transaction.
func TestConfirmProofFile(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: fixture.transitionProof,
		}},
	}
	require.NoError(t, path.Validate())

	baseFile, err := proof.DecodeFile(fixture.baseProofFile)
	require.NoError(t, err)

	anchorTx := &fixture.transition.AnchorTx
	anchorTxid := anchorTx.TxHash()
	confirmations := map[Hash]AnchorConfirmation{
		Hash(anchorTxid): {
			BlockHeight: 321,
			Block:       confirmationBlockFor(t, anchorTx),
		},
	}

	encoded, err := path.ConfirmProofFile(confirmations)
	require.NoError(t, err)

	file, err := proof.DecodeFile(encoded)
	require.NoError(t, err)
	require.Equal(t, baseFile.NumProofs()+1, file.NumProofs())

	last, err := file.LastProof()
	require.NoError(t, err)
	require.EqualValues(t, 321, last.BlockHeight)
	require.Equal(t, anchorTxid, last.AnchorTx.TxHash())
	require.True(t, last.TxMerkleProof.Verify(
		anchorTx, last.BlockHeader.MerkleRoot,
	))
}

// TestConfirmProofFileMergeEmbedsCoInputs verifies a merging first step
// permanently embeds its co-input path as an input file.
func TestConfirmProofFileMergeEmbedsCoInputs(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathMergeFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.firstBaseFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: fixture.transitionProof,
			CoInputPaths: []*AssetProofPath{{
				ConfirmedBaseProof: fixture.secondBaseFile,
			}},
		}},
	}
	require.NoError(t, path.Validate())

	transition, err := proof.Decode(fixture.transitionProof)
	require.NoError(t, err)
	anchorTxid := transition.AnchorTx.TxHash()

	encoded, err := path.ConfirmProofFile(map[Hash]AnchorConfirmation{
		Hash(anchorTxid): {
			BlockHeight: 99,
			Block: confirmationBlockFor(
				t, &transition.AnchorTx,
			),
		},
	})
	require.NoError(t, err)

	file, err := proof.DecodeFile(encoded)
	require.NoError(t, err)
	last, err := file.LastProof()
	require.NoError(t, err)
	require.Len(t, last.AdditionalInputs, 1)
	require.EqualValues(t, 99, last.BlockHeight)
}

// TestConfirmProofFileConfirmsCoInputPaths verifies recursive assembly: a
// merging step embeds each confirmed co-input lineage at the step that
// consumes it.
func TestConfirmProofFileConfirmsCoInputPaths(t *testing.T) {
	t.Parallel()

	firstFile, firstProof, firstKey := newAssetProofPathBase(t)
	secondFile, secondProof, secondKey := newAssetProofPathSecondBase(t)

	// Spine: A -> A'. Co-input path: B -> B'. Merge: A' + B' -> C.
	spineKey := testPrivateKey(t, 13)
	spineTransition := newAssetProofPathTransition(
		t, firstProof, firstKey, spineKey,
	)
	spineBytes, err := spineTransition.Bytes()
	require.NoError(t, err)

	coKey := testPrivateKey(t, 14)
	coTransition := newAssetProofPathTransition(
		t, secondProof, secondKey, coKey,
	)
	coBytes, err := coTransition.Bytes()
	require.NoError(t, err)

	mergeTransition := newAssetProofPathMergeTransition(
		t, []*proof.Proof{spineTransition, coTransition},
		[]*btcec.PrivateKey{spineKey, coKey}, testPrivateKey(t, 15),
	)
	mergeBytes, err := mergeTransition.Bytes()
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: firstFile,
		Steps: []AssetProofPathStep{
			{TransitionProof: spineBytes},
			{
				TransitionProof: mergeBytes,
				CoInputPaths: []*AssetProofPath{{
					ConfirmedBaseProof: secondFile,
					Steps: []AssetProofPathStep{{
						TransitionProof: coBytes,
					}},
				}},
			},
		},
	}
	require.NoError(t, path.Validate())

	confirmations := map[Hash]AnchorConfirmation{
		Hash(spineTransition.AnchorTx.TxHash()): {
			BlockHeight: 210,
			Block: confirmationBlockFor(
				t, &spineTransition.AnchorTx,
			),
		},
		Hash(coTransition.AnchorTx.TxHash()): {
			BlockHeight: 211,
			Block: confirmationBlockFor(
				t, &coTransition.AnchorTx,
			),
		},
		Hash(mergeTransition.AnchorTx.TxHash()): {
			BlockHeight: 212,
			Block: confirmationBlockFor(
				t, &mergeTransition.AnchorTx,
			),
		},
	}
	encoded, err := path.ConfirmProofFile(confirmations)
	require.NoError(t, err)

	baseFile, err := proof.DecodeFile(firstFile)
	require.NoError(t, err)
	coBaseFile, err := proof.DecodeFile(secondFile)
	require.NoError(t, err)

	file, err := proof.DecodeFile(encoded)
	require.NoError(t, err)
	require.Equal(t, baseFile.NumProofs()+2, file.NumProofs())

	// The merge proof embeds the co-input path as one fully confirmed
	// file: the co-input base extended by its own confirmed transition.
	last, err := file.LastProof()
	require.NoError(t, err)
	require.EqualValues(t, 212, last.BlockHeight)
	require.Len(t, last.AdditionalInputs, 1)
	embedded := last.AdditionalInputs[0]
	require.Equal(t, coBaseFile.NumProofs()+1, embedded.NumProofs())
	embeddedTip, err := embedded.LastProof()
	require.NoError(t, err)
	require.EqualValues(t, 211, embeddedTip.BlockHeight)
	require.True(t, embeddedTip.TxMerkleProof.Verify(
		&coTransition.AnchorTx, embeddedTip.BlockHeader.MerkleRoot,
	))

	// The complete file, including the embedded co-input lineage, passes
	// native verification with real merkle inclusion checks.
	verifierCtx := proof.MockVerifierCtx
	verifierCtx.MerkleVerifier = proof.DefaultMerkleVerifier
	_, err = file.Verify(context.Background(), verifierCtx)
	require.NoError(t, err)
}

// TestConfirmProofFileFailsClosed covers the assembly's rejection paths.
func TestConfirmProofFileFailsClosed(t *testing.T) {
	t.Parallel()

	fixture := newAssetProofPathFixture(t)
	path := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: fixture.transitionProof,
		}},
	}
	anchorTxid := fixture.transition.AnchorTx.TxHash()

	// No confirmation for the step's anchor transaction.
	_, err := path.ConfirmProofFile(nil)
	require.ErrorContains(t, err, "missing block confirmation")

	// A block that does not contain the anchor transaction.
	stranger := wire.NewMsgTx(2)
	stranger.AddTxIn(&wire.TxIn{})
	stranger.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x51}})
	_, err = path.ConfirmProofFile(map[Hash]AnchorConfirmation{
		Hash(anchorTxid): {
			BlockHeight: 1,
			Block:       confirmationBlockFor(t, stranger),
		},
	})
	require.ErrorContains(t, err, "not in the supplied block")

	// A path without steps is already a confirmed file.
	stepless := &AssetProofPath{
		ConfirmedBaseProof: fixture.baseProofFile,
	}
	encoded, err := stepless.ConfirmProofFile(nil)
	require.NoError(t, err)
	require.Equal(t, fixture.baseProofFile, encoded)
}
