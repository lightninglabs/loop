package rest

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/internal/codec"
	"github.com/stretchr/testify/require"
)

const (
	restDecodedProofOutpoint = "000000000000000000000000000000000000000000" +
		"0000000000000000000001:7"
	restPrevOutpointOne = "000000000000000000000000000000000000000000" +
		"0000000000000000000002:1"
	restPrevOutpointTwo = "000000000000000000000000000000000000000000" +
		"0000000000000000000003:2"
	restZeroPrevOutpoint = "000000000000000000000000000000000000000000" +
		"0000000000000000000000:0"
)

func TestUnmarshalDecodedProofStrict(t *testing.T) {
	t.Parallel()

	t.Run("issuance", func(t *testing.T) {
		jsonProof, expected := newRESTDecodedIssuanceProof(t)
		decoded, err := unmarshalDecodedProof(jsonProof)
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
		jsonProof, expected := newRESTDecodedTransferProof(t)
		decoded, err := unmarshalDecodedProof(jsonProof)
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
		jsonProof, expected := newRESTDecodedTransferProof(t)
		jsonProof.Asset.AssetGenesis.AssetType = assetTypeCollectibleJSON
		decoded, err := unmarshalDecodedProof(jsonProof)
		require.NoError(t, err)
		require.Equal(
			t, tapsdk.AssetRefFromAssetID(expected.issuanceID),
			decoded.AssetRef,
		)
	})

	t.Run("split zero previous ID", func(t *testing.T) {
		jsonProof, expected := newRESTDecodedTransferProof(t)
		jsonProof.Asset.PrevWitnesses = append(
			jsonProof.Asset.PrevWitnesses, &jsonPrevWitness{
				PrevID:          restDecodedProofZeroPrevID(),
				SplitCommitment: &jsonSplitCommitment{},
			},
		)

		decoded, err := unmarshalDecodedProof(jsonProof)
		require.NoError(t, err)
		require.False(t, decoded.IsIssuance)
		require.Equal(t, expected.prevIDs, decoded.PrevIDs)
	})
}

func TestUnmarshalDecodedProofRejectsMalformedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		proof   func(*testing.T) *jsonDecodedProof
		wantErr string
	}{
		{
			name: "nil proof",
			proof: func(*testing.T) *jsonDecodedProof {
				return nil
			},
			wantErr: "nil decoded proof",
		},
		{
			name: "nil asset",
			proof: func(*testing.T) *jsonDecodedProof {
				return &jsonDecodedProof{}
			},
			wantErr: "nil decoded asset",
		},
		{
			name: "nil genesis",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.AssetGenesis = nil
				return proof
			},
			wantErr: "nil proof asset genesis",
		},
		{
			name: "31 byte primary asset ID",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.AssetGenesis.AssetID = hex.EncodeToString(
					bytes.Repeat([]byte{1}, 31),
				)
				return proof
			},
			wantErr: "invalid asset ID",
		},
		{
			name: "32 byte primary script key",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.ScriptKey = hex.EncodeToString(
					restDecodedProofKey(t, 1)[1:],
				)
				return proof
			},
			wantErr: "invalid script key",
		},
		{
			name: "invalid group key",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.AssetGroup.TweakedGroupKey =
					hex.EncodeToString(make([]byte, 33))
				return proof
			},
			wantErr: "invalid group key",
		},
		{
			name: "malformed alt leaves hex",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.AltLeaves = "zz"
				return proof
			},
			wantErr: "invalid alt leaves",
		},
		{
			name: "malformed alt leaves payload",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.AltLeaves = hex.EncodeToString([]byte{1, 2})
				return proof
			},
			wantErr: "decode alt leaves",
		},
		{
			name: "nil previous witness",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0] = nil
				return proof
			},
			wantErr: "missing prev_id for witness 0",
		},
		{
			name: "nil previous ID",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[1].PrevID = nil
				return proof
			},
			wantErr: "missing prev_id for witness 1",
		},
		{
			name: "invalid previous outpoint",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0].PrevID.AnchorPoint =
					"not-an-outpoint"
				return proof
			},
			wantErr: "invalid prev_id outpoint for witness 0",
		},
		{
			name: "ordinary zero previous ID",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0].PrevID =
					restDecodedProofZeroPrevID()
				return proof
			},
			wantErr: "zero prev_id is only valid for issuance or split " +
				"witness 0",
		},
		{
			name: "malformed issuance zero previous ID",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedIssuanceProof(t)
				proof.Asset.PrevWitnesses[0].PrevID.ScriptKey =
					hex.EncodeToString(make([]byte, 32))
				return proof
			},
			wantErr: "invalid prev_id script_key for witness 0",
		},
		{
			name: "malformed split zero previous ID",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0] = &jsonPrevWitness{
					PrevID:          restDecodedProofZeroPrevID(),
					SplitCommitment: &jsonSplitCommitment{},
				}
				proof.Asset.PrevWitnesses[0].PrevID.AssetID =
					hex.EncodeToString(make([]byte, 31))
				return proof
			},
			wantErr: "invalid prev_id asset_id for witness 0",
		},
		{
			name: "31 byte previous asset ID",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[0].PrevID.AssetID =
					hex.EncodeToString(bytes.Repeat([]byte{2}, 31))
				return proof
			},
			wantErr: "invalid prev_id asset_id for witness 0",
		},
		{
			name: "32 byte previous script key",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[1].PrevID.ScriptKey =
					hex.EncodeToString(restDecodedProofKey(t, 4)[1:])
				return proof
			},
			wantErr: "invalid prev_id script_key for witness 1",
		},
		{
			name: "invalid previous script key",
			proof: func(t *testing.T) *jsonDecodedProof {
				proof, _ := newRESTDecodedTransferProof(t)
				proof.Asset.PrevWitnesses[1].PrevID.ScriptKey =
					hex.EncodeToString(make([]byte, 33))
				return proof
			},
			wantErr: "invalid prev_id script_key for witness 1",
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

type restDecodedProofExpected struct {
	assetRef   tapsdk.AssetRef
	issuanceID tapsdk.AssetID
	scriptKey  tapsdk.PubKey
	outpoint   tapsdk.Outpoint
	altLeaves  [][]byte
	prevIDs    []tapsdk.PrevID
}

func newRESTDecodedIssuanceProof(t *testing.T) (*jsonDecodedProof,
	restDecodedProofExpected) {

	t.Helper()

	proof, expected := newRESTDecodedProofBase(t)
	proof.ProofAtDepth = 2
	proof.NumberOfProofs = 3
	proof.Asset.Amount = "42"
	proof.GenesisReveal = &jsonGenesisReveal{
		GenesisBaseReveal: struct{}{},
	}
	proof.Asset.PrevWitnesses = []*jsonPrevWitness{{
		PrevID: restDecodedProofZeroPrevID(),
	}}

	return proof, expected
}

func newRESTDecodedTransferProof(t *testing.T) (*jsonDecodedProof,
	restDecodedProofExpected) {

	t.Helper()

	proof, expected := newRESTDecodedProofBase(t)
	proof.Asset.Amount = "37"
	groupKeyBytes := restDecodedProofKey(t, 2)
	proof.Asset.AssetGroup = &jsonAssetGroup{
		TweakedGroupKey: hex.EncodeToString(groupKeyBytes),
	}
	groupKey, err := tapsdk.ParsePubKey(groupKeyBytes)
	require.NoError(t, err)
	expected.assetRef = tapsdk.AssetRefFromGroupKey(groupKey)

	expected.altLeaves = restDecodedProofAltLeaves(t)
	encodedAltLeaves, err := codec.EncodeAltLeaves(expected.altLeaves)
	require.NoError(t, err)
	proof.AltLeaves = hex.EncodeToString(encodedAltLeaves)

	prevOne := restDecodedProofPrevID(
		t, restPrevOutpointOne, 0x21, 3,
	)
	prevTwo := restDecodedProofPrevID(
		t, restPrevOutpointTwo, 0x22, 4,
	)
	proof.Asset.PrevWitnesses = []*jsonPrevWitness{
		{PrevID: prevOne},
		{PrevID: prevTwo},
	}
	expected.prevIDs = []tapsdk.PrevID{
		restDecodedProofExpectedPrevID(t, prevOne),
		restDecodedProofExpectedPrevID(t, prevTwo),
	}

	return proof, expected
}

func newRESTDecodedProofBase(t *testing.T) (*jsonDecodedProof,
	restDecodedProofExpected) {

	t.Helper()

	assetIDBytes := bytes.Repeat([]byte{0x11}, 32)
	assetID, err := tapsdk.ParseAssetID(assetIDBytes)
	require.NoError(t, err)
	scriptKeyBytes := restDecodedProofKey(t, 1)
	scriptKey, err := tapsdk.ParsePubKey(scriptKeyBytes)
	require.NoError(t, err)
	outpoint, err := tapsdk.NewOutpointFromStr(restDecodedProofOutpoint)
	require.NoError(t, err)

	return &jsonDecodedProof{
		Asset: &jsonDecodedAsset{
			AssetGenesis: &jsonGenesisInfo{
				AssetID:   hex.EncodeToString(assetIDBytes),
				AssetType: assetTypeNormalJSON,
			},
			ScriptKey: hex.EncodeToString(scriptKeyBytes),
			ChainAnchor: &jsonChainAnchor{
				AnchorOutpoint: restDecodedProofOutpoint,
			},
		},
	}, restDecodedProofExpected{
		assetRef:   tapsdk.AssetRefFromAssetID(assetID),
		issuanceID: assetID,
		scriptKey:  scriptKey,
		outpoint:   outpoint,
	}
}

func restDecodedProofPrevID(t *testing.T, outpoint string, idByte,
	keyByte byte) *jsonPrevID {

	t.Helper()

	return &jsonPrevID{
		AnchorPoint: outpoint,
		AssetID:     hex.EncodeToString(bytes.Repeat([]byte{idByte}, 32)),
		ScriptKey:   hex.EncodeToString(restDecodedProofKey(t, keyByte)),
	}
}

func restDecodedProofZeroPrevID() *jsonPrevID {
	return &jsonPrevID{
		AnchorPoint: restZeroPrevOutpoint,
		AssetID:     hex.EncodeToString(make([]byte, 32)),
		ScriptKey:   hex.EncodeToString(make([]byte, 33)),
	}
}

func restDecodedProofExpectedPrevID(t *testing.T,
	prev *jsonPrevID) tapsdk.PrevID {

	t.Helper()

	outpoint, err := tapsdk.NewOutpointFromStr(prev.AnchorPoint)
	require.NoError(t, err)
	assetIDBytes, err := hex.DecodeString(prev.AssetID)
	require.NoError(t, err)
	assetID, err := tapsdk.ParseAssetID(assetIDBytes)
	require.NoError(t, err)
	scriptKeyBytes, err := hex.DecodeString(prev.ScriptKey)
	require.NoError(t, err)
	scriptKey, err := tapsdk.ParsePubKey(scriptKeyBytes)
	require.NoError(t, err)

	return tapsdk.PrevID{
		Outpoint:   outpoint,
		IssuanceID: assetID,
		ScriptKey:  scriptKey,
	}
}

func restDecodedProofAltLeaves(t *testing.T) [][]byte {
	t.Helper()

	var firstKey, secondKey [33]byte
	copy(firstKey[:], restDecodedProofKey(t, 5))
	copy(secondKey[:], restDecodedProofKey(t, 6))
	first, err := codec.EncodeAltLeaf(0, firstKey)
	require.NoError(t, err)
	second, err := codec.EncodeAltLeaf(1, secondKey)
	require.NoError(t, err)

	return [][]byte{first, second}
}

func restDecodedProofKey(t *testing.T, seed byte) []byte {
	t.Helper()

	keyBytes := make([]byte, 32)
	keyBytes[31] = seed
	_, publicKey := btcec.PrivKeyFromBytes(keyBytes)

	return publicKey.SerializeCompressed()
}
