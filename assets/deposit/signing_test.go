package deposit

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestControlBlockKeyIsolation prevents a caller from changing the deposit's
// contract by mutating a returned control block.
func TestControlBlockKeyIsolation(t *testing.T) {
	f := newWitnessFixture(t)
	original := f.kit.muSig2Key.PreTweakedKey.SerializeCompressed()
	root := make([]byte, 32)
	block, err := f.kit.GenTimeoutBtcControlBlock(root)
	require.NoError(t, err)
	_, other := scalarKey(t, 42)
	*block.InternalKey = *other
	next, err := f.kit.GenTimeoutBtcControlBlock(root)
	require.NoError(t, err)
	require.Equal(t, original, next.InternalKey.SerializeCompressed())
	require.NotEqual(t, other.SerializeCompressed(),
		next.InternalKey.SerializeCompressed())
}

// TestTimeoutPreservesFeeSignature exercises refund signing after a wallet has
// already signed a fee input. The transaction ID and fee signature stay valid.
func TestTimeoutPreservesFeeSignature(t *testing.T) {
	f := newWitnessFixture(t)
	feePrivateKey, feePubKey := scalarKey(t, 42)
	feeOutputKey := txscript.ComputeTaprootKeyNoScript(feePubKey)
	feeScript, err := txscript.PayToTaprootScript(feeOutputKey)
	require.NoError(t, err)
	f.prevOutputs[0].PkScript = feeScript
	tx := f.packet.UnsignedTx
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, input := range tx.TxIn {
		fetcher.AddPrevOut(input.PreviousOutPoint, f.prevOutputs[i])
	}
	sig, err := txscript.RawTxInTaprootSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0,
		f.prevOutputs[0].Value, feeScript, nil, txscript.SigHashDefault,
		feePrivateKey,
	)
	require.NoError(t, err)
	f.packet.Inputs[0].TaprootKeySpendSig = sig
	parsed, err := schnorr.ParseSignature(sig)
	require.NoError(t, err)
	signatureValid := func() bool {
		digest, err := txscript.CalcTaprootSignatureHash(
			txscript.NewTxSigHashes(tx, fetcher), txscript.SigHashDefault,
			tx, 0, fetcher,
		)
		require.NoError(t, err)
		return parsed.Verify(digest, feeOutputKey)
	}
	require.True(t, signatureValid())
	originalTxid := tx.TxHash()
	signer := &localSigner{
		privateKey: f.funderKey, expectedInput: f.assetInIndex,
	}
	_, err = f.kit.CreateTimeoutWitness(
		t.Context(), signer, f.proof, f.packet, f.transfer,
	)
	require.NoError(t, err)
	require.True(t, signatureValid())
	require.Equal(t, originalTxid, tx.TxHash())
}

type versionAddressClient struct {
	AddressProofClient

	request *taprpc.NewAddrRequest
}

func (m *versionAddressClient) NewAddr(_ context.Context,
	req *taprpc.NewAddrRequest, _ ...grpc.CallOption) (*taprpc.Addr, error) {

	m.request = req
	return &taprpc.Addr{}, nil
}

// TestAddressAndPacketVersionsMatch freezes the RPC versions so addresses and
// funding packets do not drift when tapd defaults change.
func TestAddressAndPacketVersionsMatch(t *testing.T) {
	f := newWitnessFixture(t)
	client := &versionAddressClient{}
	_, kit, err := f.kit.NewHtlcAddr(
		t.Context(), client, 1000, lntypes.Hash{1}, 144,
	)
	require.NoError(t, err)
	version, err := rpcutils.UnmarshalAssetVersion(client.request.AssetVersion)
	require.NoError(t, err)
	require.Equal(t, taprpc.AddrVersion_ADDR_VERSION_V1,
		client.request.AddressVersion)
	packet, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	require.Equal(t, asset.V1, version)
	require.Equal(t, asset.V1, packet.Outputs[1].AssetVersion)
	_, err = f.kit.NewAddr(t.Context(), client, 1000)
	require.NoError(t, err)
	require.Equal(t, taprpc.AssetVersion_ASSET_VERSION_V1,
		client.request.AssetVersion)
	require.Equal(t, taprpc.AddrVersion_ADDR_VERSION_V1,
		client.request.AddressVersion)
}

// TestTimeoutRejectsUnsetSequence prevents signing a transaction that would need
// to change after other inputs have already been signed.
func TestTimeoutRejectsUnsetSequence(t *testing.T) {
	f := newWitnessFixture(t)
	index, err := f.kit.AssetInputIndex(f.proof, f.packet)
	require.NoError(t, err)
	require.Equal(t, uint32(f.assetInIndex), index)
	f.packet.UnsignedTx.TxIn[index].Sequence = 0
	originalTxid := f.packet.UnsignedTx.TxHash()
	signer := &localSigner{privateKey: f.funderKey, expectedInput: f.assetInIndex}
	_, err = f.kit.CreateTimeoutWitness(
		t.Context(), signer, f.proof, f.packet, f.transfer,
	)
	require.ErrorContains(t, err, "asset input sequence must be")
	require.Zero(t, signer.calls)
	require.Equal(t, originalTxid, f.packet.UnsignedTx.TxHash())
}

// TestTimeoutRejectsInvalidTransfer ensures the deposit signing entry point
// also enforces asset preservation before requesting a signature.
func TestTimeoutRejectsInvalidTransfer(t *testing.T) {
	for _, missing := range []bool{false, true} {
		f := newWitnessFixture(t)
		if missing {
			f.transfer = nil
		} else {
			f.packet.UnsignedTx.TxOut[1].PkScript =
				[]byte{txscript.OP_TRUE}
		}
		signer := &localSigner{
			privateKey: f.funderKey, expectedInput: f.assetInIndex,
		}
		_, err := f.kit.CreateTimeoutWitness(
			t.Context(), signer, f.proof, f.packet, f.transfer,
		)
		require.Error(t, err)
		require.Zero(t, signer.calls)
	}
}
