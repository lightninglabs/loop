package deposit

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestNewKitRejectsOppositeKeys ensures the two script roles cannot use the
// same x-only key under different compressed encodings.
func TestNewKitRejectsOppositeKeys(t *testing.T) {
	_, funder := scalarKey(t, 1)
	encoded := funder.SerializeCompressed()
	encoded[0] ^= 1
	opposite, err := btcec.ParsePubKey(encoded)
	require.NoError(t, err)
	require.False(t, funder.IsEqual(opposite))

	_, err = NewKit(
		funder, opposite, keychain.KeyLocator{}, asset.ID{1}, 144,
		&address.RegressionNetTap,
	)
	require.ErrorContains(t, err, "keys must differ")
}

// mutatingAddressClient retains and modifies request IDs to exercise the
// ownership boundary between a kit and its RPC client.
type mutatingAddressClient struct {
	ids [][]byte
}

// NewAddr retains and mutates the request's asset ID.
func (m *mutatingAddressClient) NewAddr(_ context.Context,
	req *taprpc.NewAddrRequest, _ ...grpc.CallOption) (*taprpc.Addr, error) {

	m.ids = append(m.ids, req.AssetId)
	req.AssetId[0] ^= 1

	return &taprpc.Addr{}, nil
}

// ExportProof retains and mutates the request's asset ID.
func (m *mutatingAddressClient) ExportProof(_ context.Context,
	req *taprpc.ExportProofRequest, _ ...grpc.CallOption) (*taprpc.ProofFile,
	error) {

	m.ids = append(m.ids, req.AssetId)
	req.AssetId[0] ^= 1

	return &taprpc.ProofFile{}, nil
}

// TestRPCRequestsPreserveAssetID verifies that immediate and retained request
// mutations cannot change a kit or a derived HTLC contract.
func TestRPCRequestsPreserveAssetID(t *testing.T) {
	fixture := newWitnessFixture(t)
	kit := fixture.kit
	expectedID := kit.assetID
	client := &mutatingAddressClient{}

	_, err := kit.NewAddr(t.Context(), client, 1000)
	require.NoError(t, err)
	require.Equal(t, expectedID, kit.assetID)

	_, swapKit, err := kit.NewHtlcAddr(
		t.Context(), client, 1000, lntypes.Hash{1}, 144,
	)
	require.NoError(t, err)
	require.Equal(t, expectedID, kit.assetID)
	require.Equal(t, expectedID, swapKit.AssetID())

	outpoint := fixture.proof.OutPoint()
	_, err = kit.ExportProof(t.Context(), client, &outpoint)
	require.NoError(t, err)
	require.Equal(t, expectedID, kit.assetID)
	require.Len(t, client.ids, 3)
	for _, id := range client.ids {
		clear(id)
	}
	require.Equal(t, expectedID, kit.assetID)
	_, err = kit.AnchorRootFromProofCommitment(fixture.proof, 1000)
	require.NoError(t, err)
}

// TestVerifyProofFileSnapshot verifies that changes to caller-owned bytes
// during the RPC cannot replace the proof consumed after verification.
func TestVerifyProofFileSnapshot(t *testing.T) {
	fixture := newWitnessFixture(t)
	file, err := proof.NewFile(proof.V0, *fixture.proof)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, file.Encode(&encoded))
	raw := encoded.Bytes()
	expectedBytes := bytes.Clone(raw)
	rpcFile := &taprpc.ProofFile{RawProofFile: raw}
	outpoint := fixture.proof.OutPoint()
	verifier := &proofVerifierMock{
		response: &taprpc.VerifyProofResponse{Valid: true},
		onVerify: func(req *taprpc.ProofFile) {
			clear(raw)
			rpcFile.RawProofFile = []byte{0}
			require.Equal(t, expectedBytes, req.RawProofFile)
		},
	}
	expectedRoot, err := fixture.kit.AnchorRootFromProofCommitment(
		fixture.proof, fixture.proof.Asset.Amount,
	)
	require.NoError(t, err)
	root, err := fixture.kit.VerifyProofFile(
		t.Context(), verifier, rpcFile, &outpoint,
		fixture.proof.Asset.Amount,
	)
	require.NoError(t, err)
	require.Equal(t, expectedRoot, root)
	require.Equal(t, 1, verifier.calls)
}

// TestVerifyProofFileSizeLimit verifies oversized input is rejected before
// crossing the verifier boundary.
func TestVerifyProofFileSizeLimit(t *testing.T) {
	fixture := newWitnessFixture(t)
	verifier := &proofVerifierMock{
		response: &taprpc.VerifyProofResponse{Valid: true},
	}
	_, err := fixture.kit.VerifyProofFile(
		t.Context(), verifier, &taprpc.ProofFile{
			RawProofFile: make([]byte, proof.FileMaxProofSizeBytes+1),
		}, &wire.OutPoint{}, fixture.proof.Asset.Amount,
	)
	require.ErrorContains(t, err, "file exceeds maximum size")
	require.Zero(t, verifier.calls)
}
