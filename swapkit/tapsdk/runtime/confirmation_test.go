package runtime

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

func confirmationPackage(t *testing.T, version asset.Version) transaction {
	t.Helper()
	// Reuse the public SDK fixture used to test the production wallet
	// signer. Only test code knows the deterministic private keys.
	raw, err := os.ReadFile("../walletsigner/testdata/script.package")
	require.NoError(t, err)
	var pkg sdk.CustomAnchorTransferPackage
	require.NoError(t, pkg.UnmarshalBinary(raw))
	base, err := lastProof(pkg.Inputs[0].ProofSource.Blob)
	require.NoError(t, err)
	base.Asset.Version = version
	pkg.Inputs[0].ProofSource.Blob = bindingRaw(t, base)
	// Reseal the public fixture after changing only the base version.
	// The assembler test does not replace full lineage verification.
	domain := []byte("tap-sdk/custom-anchor/proof-file/v1\x00")
	pkg.Inputs[0].ProofSource.ContentID = sha256.Sum256(
		append(domain, pkg.Inputs[0].ProofSource.Blob...),
	)
	sealed, err := pkg.Seal()
	require.NoError(t, err)
	requests, err := sealed.SigningRequests()
	require.NoError(t, err)
	packet, err := decodePSBT(sealed.AnchorPsbt)
	require.NoError(t, err)
	setWitness := func(index uint32, witness wire.TxWitness) {
		var encoded bytes.Buffer
		require.NoError(t, psbt.WriteTxWitness(&encoded, witness))
		packet.Inputs[index].FinalScriptWitness = encoded.Bytes()
	}
	for _, request := range requests.KeyPath {
		var scalar byte
		for _, candidate := range []byte{2, 9} {
			key := bindingKey(candidate)
			if bytes.Equal(schnorr.SerializePubKey(key.PubKey()),
				request.Signer[:]) {
				scalar = candidate
			}
		}
		require.NotZero(t, scalar)
		key := txscript.TweakTaprootPrivKey(*bindingKey(scalar),
			request.TaprootMerkleRoot)
		signature, err := schnorr.Sign(key, request.Sighash[:])
		require.NoError(t, err)
		encoded := signature.Serialize()
		if request.SighashType != 0 {
			encoded = append(encoded, byte(request.SighashType))
		}
		setWitness(request.InputIndex, wire.TxWitness{encoded})
	}
	for _, request := range requests.ScriptPath {
		signature, err := schnorr.Sign(
			bindingKey(2), request.Sighash[:],
		)
		require.NoError(t, err)
		encoded := signature.Serialize()
		if request.SighashType != 0 {
			encoded = append(encoded, byte(request.SighashType))
		}
		setWitness(request.InputIndex, wire.TxWitness{
			encoded, request.LeafScript, request.ControlBlock,
		})
	}
	final, err := encodePSBT(packet)
	require.NoError(t, err)
	result := transaction{Package: sealed, Final: final}
	_, err = finalTx(result)
	require.NoError(t, err)
	return result
}

// TestConfirmOrdinaryProofVersions checks the confirmed-lineage assembler,
// including historical v0 assets. Full lineage cryptography remains checked
// by publish's authenticated SDK verifier after this step.
func TestConfirmOrdinaryProofVersions(t *testing.T) {
	for _, version := range []asset.Version{asset.V0, asset.V1} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			tx := confirmationPackage(t, version)
			anchor, err := finalTx(tx)
			require.NoError(t, err)
			block := &wire.MsgBlock{
				Header: wire.BlockHeader{
					MerkleRoot: anchor.TxHash(),
				},
				Transactions: []*wire.MsgTx{anchor},
			}
			raw, err := confirmedProof(tx, block, 123)
			require.NoError(t, err)
			file, err := proof.DecodeFile(raw)
			require.NoError(t, err)
			require.EqualValues(t, 2, file.NumProofs())
			first, err := file.ProofAt(0)
			require.NoError(t, err)
			require.Equal(t, version, first.Asset.Version)
			last, err := file.LastProof()
			require.NoError(t, err)
			require.EqualValues(t, 123, last.BlockHeight)
			require.Equal(t, block.BlockHash(),
				last.BlockHeader.BlockHash())
			require.Equal(t, anchor.TxHash(),
				last.AnchorTx.TxHash())
			require.True(t, last.TxMerkleProof.Verify(
				anchor, block.Header.MerkleRoot))

			block.Transactions = nil
			_, err = confirmedProof(tx, block, 123)
			require.ErrorIs(t, err, bridge.ErrPending)
		})
	}
}
