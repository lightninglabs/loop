package grpc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/internal/codec"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
)

const (
	grpcDecodedProofOutpoint = "000000000000000000000000000000000000000000" +
		"0000000000000000000001:7"
	grpcPrevOutpointOne = "000000000000000000000000000000000000000000" +
		"0000000000000000000002:1"
	grpcPrevOutpointTwo = "000000000000000000000000000000000000000000" +
		"0000000000000000000003:2"
	grpcZeroPrevOutpoint = "000000000000000000000000000000000000000000" +
		"0000000000000000000000:0"
)

func TestUnmarshalDecodedProofStrict(t *testing.T) {
	t.Parallel()

	t.Run("issuance", func(t *testing.T) {
		rpcProof, expected := newGRPCDecodedIssuanceProof(t)
		decoded, err := unmarshalDecodedProof(rpcProof)
		require.NoError(t, err)

		require.True(t, decoded.IsIssuance)
		require.Equal(t, expected.assetRef, decoded.AssetRef)
		require.Equal(t, expected.issuanceID, decoded.IssuanceID)
		require.Equal(t, expected.scriptKey, decoded.ScriptKey)
		require.Equal(t, expected.outpoint, decoded.Outpoint)
		require.Equal(t, uint64(42), decoded.Amount)
		require.Equal(t, uint32(2), decoded.ProofAtDepth)
		require.Equal(t, uint32(3), decoded.NumberOfProofs)
		require.Empty(t, decoded.PrevIDs)
		require.Empty(t, decoded.AltLeaves)
	})

	t.Run("v1 transfer", func(t *testing.T) {
		rpcProof, expected := newGRPCDecodedTransferProof(t)
		decoded, err := unmarshalDecodedProof(rpcProof)
		require.NoError(t, err)

		require.False(t, decoded.IsIssuance)
		require.Equal(t, expected.assetRef, decoded.AssetRef)
		require.Equal(t, expected.issuanceID, decoded.IssuanceID)
		require.Equal(t, expected.scriptKey, decoded.ScriptKey)
		require.Equal(t, expected.outpoint, decoded.Outpoint)
		require.Equal(t, uint64(37), decoded.Amount)
		require.Equal(t, expected.altLeaves, decoded.AltLeaves)
		require.Equal(t, expected.prevIDs, decoded.PrevIDs)
	})

	t.Run("grouped collectible", func(t *testing.T) {
		rpcProof, expected := newGRPCDecodedTransferProof(t)
		rpcProof.Asset.AssetGenesis.AssetType = taprpc.AssetType_COLLECTIBLE
		decoded, err := unmarshalDecodedProof(rpcProof)
		require.NoError(t, err)
		require.Equal(
			t, tapsdk.AssetRefFromAssetID(expected.issuanceID),
			decoded.AssetRef,
		)
	})

	t.Run("split zero previous ID", func(t *testing.T) {
		rpcProof, expected := newGRPCDecodedTransferProof(t)
		rpcProof.Asset.PrevWitnesses = append(
			rpcProof.Asset.PrevWitnesses, &taprpc.PrevWitness{
				PrevId:          grpcDecodedProofZeroPrevID(),
				SplitCommitment: &taprpc.SplitCommitment{},
			},
		)

		decoded, err := unmarshalDecodedProof(rpcProof)
		require.NoError(t, err)
		require.False(t, decoded.IsIssuance)
		require.Equal(t, expected.prevIDs, decoded.PrevIDs)
	})
}

func TestUnmarshalDecodedProofRejectsMalformedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		proof   func(*testing.T) *taprpc.DecodedProof
		wantErr string
	}{
		{
			name: "nil proof",
			proof: func(*testing.T) *taprpc.DecodedProof {
				return nil
			},
			wantErr: "nil decoded proof",
		},
		{
			name: "nil asset",
			proof: func(*testing.T) *taprpc.DecodedProof {
				return &taprpc.DecodedProof{}
			},
			wantErr: "nil proof asset",
		},
		{
			name: "nil genesis",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.AssetGenesis = nil
				return proof
			},
			wantErr: "nil proof asset genesis",
		},
		{
			name: "31 byte primary asset ID",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.AssetGenesis.AssetId = bytes.Repeat(
					[]byte{1}, 31,
				)
				return proof
			},
			wantErr: "invalid asset ID",
		},
		{
			name: "32 byte primary script key",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.ScriptKey = grpcDecodedProofKey(t, 1)[1:]
				return proof
			},
			wantErr: "invalid script key",
		},
		{
			name: testInvalidGroupKey,
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.AssetGroup.TweakedGroupKey = make([]byte, 33)
				return proof
			},
			wantErr: testInvalidGroupKey,
		},
		{
			name: "malformed alt leaves",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.AltLeaves = []byte{1, 2}
				return proof
			},
			wantErr: "decode alt leaves",
		},
		{
			name: "nil previous witness",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0] = nil
				return proof
			},
			wantErr: "missing prev_id for witness 0",
		},
		{
			name: "nil previous ID",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[1].PrevId = nil
				return proof
			},
			wantErr: "missing prev_id for witness 1",
		},
		{
			name: "invalid previous outpoint",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0].PrevId.AnchorPoint =
					"not-an-outpoint"
				return proof
			},
			wantErr: "invalid prev_id outpoint for witness 0",
		},
		{
			name: "ordinary zero previous ID",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0].PrevId =
					grpcDecodedProofZeroPrevID()
				return proof
			},
			wantErr: "zero prev_id is only valid for issuance or split " +
				"witness 0",
		},
		{
			name: "malformed issuance zero previous ID",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedIssuanceProof(t)
				proof.Asset.PrevWitnesses[0].PrevId.ScriptKey =
					make([]byte, 32)
				return proof
			},
			wantErr: "invalid prev_id script key for witness 0",
		},
		{
			name: "malformed split zero previous ID",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0] = &taprpc.PrevWitness{
					PrevId:          grpcDecodedProofZeroPrevID(),
					SplitCommitment: &taprpc.SplitCommitment{},
				}
				proof.Asset.PrevWitnesses[0].PrevId.AssetId =
					make([]byte, 31)
				return proof
			},
			wantErr: "invalid prev_id asset ID for witness 0",
		},
		{
			name: "31 byte previous asset ID",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0].PrevId.AssetId =
					bytes.Repeat([]byte{2}, 31)
				return proof
			},
			wantErr: "invalid prev_id asset ID for witness 0",
		},
		{
			name: "32 byte previous script key",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[1].PrevId.ScriptKey =
					grpcDecodedProofKey(t, 4)[1:]
				return proof
			},
			wantErr: "invalid prev_id script key for witness 1",
		},
		{
			name: "invalid previous script key",
			proof: func(t *testing.T) *taprpc.DecodedProof {
				proof, _ := newGRPCDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[1].PrevId.ScriptKey =
					make([]byte, 33)
				return proof
			},
			wantErr: "invalid prev_id script key for witness 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := unmarshalDecodedProof(test.proof(t))
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

type grpcDecodedProofExpected struct {
	assetRef   tapsdk.AssetRef
	issuanceID tapsdk.AssetID
	scriptKey  tapsdk.PubKey
	outpoint   tapsdk.Outpoint
	altLeaves  [][]byte
	prevIDs    []tapsdk.PrevID
}

func newGRPCDecodedIssuanceProof(t *testing.T) (*taprpc.DecodedProof,
	grpcDecodedProofExpected) {

	t.Helper()

	proof, expected := newGRPCDecodedProofBase(t)
	proof.ProofAtDepth = 2
	proof.NumberOfProofs = 3
	proof.Asset.Amount = 42
	proof.GenesisReveal = &taprpc.GenesisReveal{
		GenesisBaseReveal: proof.Asset.AssetGenesis,
	}
	proof.Asset.PrevWitnesses = []*taprpc.PrevWitness{{
		PrevId: grpcDecodedProofZeroPrevID(),
	}}

	return proof, expected
}

func newGRPCDecodedTransferProof(t *testing.T) (*taprpc.DecodedProof,
	grpcDecodedProofExpected) {

	t.Helper()

	proof, expected := newGRPCDecodedProofBase(t)
	proof.Asset.Version = taprpc.AssetVersion_ASSET_VERSION_V1
	proof.Asset.Amount = 37
	groupKey := grpcDecodedProofKey(t, 2)
	proof.Asset.AssetGroup = &taprpc.AssetGroup{
		TweakedGroupKey: groupKey,
	}
	expectedGroupKey, err := tapsdk.ParsePubKey(groupKey)
	require.NoError(t, err)
	expected.assetRef = tapsdk.AssetRefFromGroupKey(expectedGroupKey)

	expected.altLeaves = grpcDecodedProofAltLeaves(t)
	proof.AltLeaves, err = codec.EncodeAltLeaves(expected.altLeaves)
	require.NoError(t, err)

	prevOne := grpcDecodedProofPrevID(
		t, grpcPrevOutpointOne, 0x21, 3,
	)
	prevTwo := grpcDecodedProofPrevID(
		t, grpcPrevOutpointTwo, 0x22, 4,
	)
	proof.Asset.PrevWitnesses = []*taprpc.PrevWitness{
		{PrevId: prevOne},
		{PrevId: prevTwo},
	}
	expected.prevIDs = []tapsdk.PrevID{
		grpcDecodedProofExpectedPrevID(t, prevOne),
		grpcDecodedProofExpectedPrevID(t, prevTwo),
	}

	return proof, expected
}

func newGRPCDecodedProofBase(t *testing.T) (*taprpc.DecodedProof,
	grpcDecodedProofExpected) {

	t.Helper()

	assetIDBytes := bytes.Repeat([]byte{0x11}, 32)
	assetID, err := tapsdk.ParseAssetID(assetIDBytes)
	require.NoError(t, err)
	scriptKeyBytes := grpcDecodedProofKey(t, 1)
	scriptKey, err := tapsdk.ParsePubKey(scriptKeyBytes)
	require.NoError(t, err)
	outpoint, err := tapsdk.NewOutpointFromStr(grpcDecodedProofOutpoint)
	require.NoError(t, err)

	return &taprpc.DecodedProof{
		Asset: &taprpc.Asset{
			AssetGenesis: &taprpc.GenesisInfo{
				AssetId: assetIDBytes,
			},
			ScriptKey: scriptKeyBytes,
			ChainAnchor: &taprpc.AnchorInfo{
				AnchorOutpoint: grpcDecodedProofOutpoint,
			},
		},
	}, grpcDecodedProofExpected{
		assetRef:   tapsdk.AssetRefFromAssetID(assetID),
		issuanceID: assetID,
		scriptKey:  scriptKey,
		outpoint:   outpoint,
	}
}

func grpcDecodedProofPrevID(t *testing.T, outpoint string, idByte,
	keyByte byte) *taprpc.PrevInputAsset {

	t.Helper()

	return &taprpc.PrevInputAsset{
		AnchorPoint: outpoint,
		AssetId:     bytes.Repeat([]byte{idByte}, 32),
		ScriptKey:   grpcDecodedProofKey(t, keyByte),
	}
}

func grpcDecodedProofZeroPrevID() *taprpc.PrevInputAsset {
	return &taprpc.PrevInputAsset{
		AnchorPoint: grpcZeroPrevOutpoint,
		AssetId:     make([]byte, 32),
		ScriptKey:   make([]byte, 33),
	}
}

func grpcDecodedProofExpectedPrevID(t *testing.T,
	prev *taprpc.PrevInputAsset) tapsdk.PrevID {

	t.Helper()

	outpoint, err := tapsdk.NewOutpointFromStr(prev.AnchorPoint)
	require.NoError(t, err)
	assetID, err := tapsdk.ParseAssetID(prev.AssetId)
	require.NoError(t, err)
	scriptKey, err := tapsdk.ParsePubKey(prev.ScriptKey)
	require.NoError(t, err)

	return tapsdk.PrevID{
		Outpoint:   outpoint,
		IssuanceID: assetID,
		ScriptKey:  scriptKey,
	}
}

func grpcDecodedProofAltLeaves(t *testing.T) [][]byte {
	t.Helper()

	var firstKey, secondKey [33]byte
	copy(firstKey[:], grpcDecodedProofKey(t, 5))
	copy(secondKey[:], grpcDecodedProofKey(t, 6))
	first, err := codec.EncodeAltLeaf(0, firstKey)
	require.NoError(t, err)
	second, err := codec.EncodeAltLeaf(1, secondKey)
	require.NoError(t, err)

	return [][]byte{first, second}
}

func grpcDecodedProofKey(t *testing.T, seed byte) []byte {
	t.Helper()

	keyBytes := make([]byte, 32)
	keyBytes[31] = seed
	_, publicKey := btcec.PrivKeyFromBytes(keyBytes)

	return publicKey.SerializeCompressed()
}
