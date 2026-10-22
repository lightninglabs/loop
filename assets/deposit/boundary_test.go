package deposit

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/assets/internal/policy"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestNewKitRejectsCrossPartyAliases rejects shared x-only keys between parties,
// across both key roles and including opposite compressed-key parity.
func TestNewKitRejectsCrossPartyAliases(t *testing.T) {
	_, senderInternal := scalarKey(t, 1)
	_, receiverInternal := scalarKey(t, 2)
	_, senderScript := scalarKey(t, 3)
	_, receiverScript := scalarKey(t, 4)

	for _, test := range []struct {
		name  string
		alias func(*Params) **btcec.PublicKey
		key   *btcec.PublicKey
	}{
		{
			name: "internal to internal", key: senderInternal,
			alias: func(p *Params) **btcec.PublicKey {
				return &p.ReceiverInternalPubKey
			},
		},
		{
			name: "script to script", key: senderScript,
			alias: func(p *Params) **btcec.PublicKey {
				return &p.ReceiverScriptKey
			},
		},
		{
			name: "internal to script", key: senderInternal,
			alias: func(p *Params) **btcec.PublicKey {
				return &p.ReceiverScriptKey
			},
		},
		{
			name: "script to internal", key: senderScript,
			alias: func(p *Params) **btcec.PublicKey {
				return &p.ReceiverInternalPubKey
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, parity := range []string{testSameGroup, testOppositeGroup} {
				t.Run(parity, func(t *testing.T) {
					params := Params{
						SenderInternalPubKey:   senderInternal,
						ReceiverInternalPubKey: receiverInternal,
						SenderScriptKey:        senderScript,
						ReceiverScriptKey:      receiverScript,
						KeyLocator:             keychain.KeyLocator{},
						AssetID:                asset.ID{1}, CsvExpiry: 144,
						ChainParams: &address.RegressionNetTap,
					}
					encoded := test.key.SerializeCompressed()
					if parity == testOppositeGroup {
						encoded[0] ^= 1
					}
					alias, err := btcec.ParsePubKey(encoded)
					require.NoError(t, err)
					*test.alias(&params) = alias
					_, err = NewKit(params)
					require.ErrorContains(t, err, "keys must differ")
				})
			}
		})
	}
}

// mutatingAddressClient retains and modifies request IDs to exercise the
// ownership boundary between a kit and its RPC client.
type mutatingAddressClient struct {
	supportedTapdClient

	ids     [][]byte
	genesis asset.Genesis
}

// NewAddr retains and mutates the request's asset ID.
func (m *mutatingAddressClient) NewAddr(_ context.Context,
	req *taprpc.NewAddrRequest, _ ...grpc.CallOption) (*taprpc.Addr, error) {

	response, err := newAddrResponse(req, m.genesis, nil)
	if err != nil {
		return nil, err
	}
	m.ids = append(m.ids, req.AssetId)
	req.AssetId[0] ^= 1

	return response, nil
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
	client := &mutatingAddressClient{genesis: fixture.proof.Asset.Genesis}

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
			RawProofFile: make([]byte, policy.MaxProvenanceBytes+1),
		}, &wire.OutPoint{}, fixture.proof.Asset.Amount,
	)
	require.ErrorContains(t, err, "byte limit")
	require.Zero(t, verifier.calls)
}
