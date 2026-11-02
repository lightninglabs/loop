package rest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/stretchr/testify/require"
)

func TestParseHexBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{
			name:    testEmptyString,
			input:   "",
			wantLen: 0,
		},
		{
			name:    "valid hex",
			input:   "abcdef0123456789",
			wantLen: 8,
		},
		{
			name:    "invalid hex",
			input:   "not-hex",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHexBytes(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Len(t, got, tc.wantLen)
		})
	}
}

func TestParseBase64Bytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{
			name:    testEmptyString,
			input:   "",
			wantLen: 0,
		},
		{
			name:    "valid base64",
			input:   "SGVsbG8=",
			wantLen: 5,
		},
		{
			name:    "invalid base64",
			input:   "not-valid!!!",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBase64Bytes(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Len(t, got, tc.wantLen)
		})
	}
}

func TestParseUint64(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{
			name:  testEmptyString,
			input: "",
			want:  0,
		},
		{
			name:  "zero",
			input: "0",
			want:  0,
		},
		{
			name:  "positive value",
			input: "42",
			want:  42,
		},
		{
			name:  "large value",
			input: "18446744073709551615",
			want:  18446744073709551615,
		},
		{
			name:    "negative value",
			input:   "-1",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUint64(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnmarshalVerifyProofResponseIncludesDecodedProof(t *testing.T) {
	var (
		assetID = "0102030405060708090a0b0c0d0e0f101112131415161718" +
			"191a1b1c1d1e1f20"
		scriptKey = hex.EncodeToString(restTestPubKey)
		outpoint  = "000000000000000000000000000000000000000000000000" +
			"0000000000000001:7"
	)

	result, err := unmarshalVerifyProofResponse(&jsonVerifyProofResponse{
		Valid: true,
		DecodedProof: &jsonDecodedProof{
			ProofAtDepth:   2,
			NumberOfProofs: 3,
			Asset: &jsonDecodedAsset{
				AssetGenesis: &jsonGenesisInfo{AssetID: assetID},
				Amount:       "42",
				ScriptKey:    scriptKey,
				ChainAnchor: &jsonChainAnchor{
					AnchorOutpoint: outpoint,
				},
			},
		},
	})
	require.NoError(t, err)
	require.True(t, result.Valid)
	require.NotNil(t, result.DecodedProof)
	require.Equal(t, uint64(42), result.DecodedProof.Amount)
	require.Equal(t, uint32(2), result.DecodedProof.ProofAtDepth)
	require.Equal(t, uint32(3), result.DecodedProof.NumberOfProofs)
	require.Equal(t, outpoint, result.DecodedProof.Outpoint.String())
	require.Equal(t, assetID, hex.EncodeToString(
		result.DecodedProof.IssuanceID[:],
	))
}

func TestParseAssetTypes(t *testing.T) {
	tests := []struct {
		name    string
		parse   func(string) (tapsdk.AssetType, error)
		input   string
		want    tapsdk.AssetType
		wantErr string
	}{
		{
			name:  "asset normal",
			parse: parseAssetType,
			input: assetTypeNormalJSON,
			want:  tapsdk.AssetTypeFungible,
		},
		{
			name:  "asset collectible",
			parse: parseAssetType,
			input: assetTypeCollectibleJSON,
			want:  tapsdk.AssetTypeNFT,
		},
		{
			name:    "asset unknown",
			parse:   parseAssetType,
			input:   testUnknown,
			wantErr: "unknown asset_type",
		},
		{
			name:    "asset empty",
			parse:   parseAssetType,
			input:   "",
			wantErr: "unknown asset_type",
		},
		{
			name:  "burn normal",
			parse: parseBurnAssetType,
			input: assetTypeNormalJSON,
			want:  tapsdk.AssetTypeFungible,
		},
		{
			name:  "burn collectible",
			parse: parseBurnAssetType,
			input: assetTypeCollectibleJSON,
			want:  tapsdk.AssetTypeNFT,
		},
		{
			name:    "burn empty",
			parse:   parseBurnAssetType,
			input:   "",
			wantErr: testUnknownBurnAssetType,
		},
		{
			name:    "burn unknown",
			parse:   parseBurnAssetType,
			input:   testUnknown,
			wantErr: testUnknownBurnAssetType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.parse(tc.input)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnmarshalBurnRecord(t *testing.T) {
	var (
		assetID  tapsdk.AssetID
		groupKey tapsdk.PubKey
	)
	copy(assetID[:], restTestAssetID)
	copy(groupKey[:], restTestPubKey)
	collectionRef := tapsdk.AssetRefFromGroupKey(groupKey)

	tests := []struct {
		name      string
		groupKey  string
		assetType string
		wantRef   tapsdk.AssetRef
		wantColl  *tapsdk.AssetRef
		wantType  tapsdk.AssetType
	}{
		{
			name:      "fungible group burn",
			groupKey:  hex.EncodeToString(restTestPubKey),
			assetType: assetTypeNormalJSON,
			wantRef:   tapsdk.AssetRefFromGroupKey(groupKey),
			wantType:  tapsdk.AssetTypeFungible,
		},
		{
			name:      "standalone NFT burn",
			assetType: assetTypeCollectibleJSON,
			wantRef:   tapsdk.AssetRefFromAssetID(assetID),
			wantType:  tapsdk.AssetTypeNFT,
		},
		{
			name:      "collection item burn",
			groupKey:  hex.EncodeToString(restTestPubKey),
			assetType: assetTypeCollectibleJSON,
			wantRef:   tapsdk.AssetRefFromAssetID(assetID),
			wantColl:  &collectionRef,
			wantType:  tapsdk.AssetTypeNFT,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]string{
				"asset_id":          hex.EncodeToString(restTestAssetID),
				"tweaked_group_key": tc.groupKey,
				"amount":            "1",
				"anchor_txid":       hex.EncodeToString(restTestAssetID),
				"asset_type":        tc.assetType,
			}

			raw, err := json.Marshal(body)
			require.NoError(t, err)

			var burn jsonAssetBurn
			require.NoError(t, json.Unmarshal(raw, &burn))

			result, err := unmarshalBurnRecord(&burn)
			require.NoError(t, err)
			require.Equal(t, tc.wantRef, result.AssetRef)
			require.Equal(t, tc.wantColl, result.CollectionRef)
			require.Equal(t, tc.wantType, result.Type)
			require.Equal(t, assetID, result.IssuanceID)
			require.Equal(t, uint64(1), result.Amount)
		})
	}
}

func TestUnmarshalBurnRecordRejectsMalformedFields(t *testing.T) {
	validBurn := func() jsonAssetBurn {
		return jsonAssetBurn{
			AssetID:         hex.EncodeToString(restTestAssetID),
			TweakedGroupKey: hex.EncodeToString(restTestPubKey),
			Amount:          "1",
			AnchorTxid:      hex.EncodeToString(restTestAssetID),
			AssetType:       assetTypeNormalJSON,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*jsonAssetBurn)
		wantErr string
	}{
		{
			name: "invalid asset ID length",
			mutate: func(b *jsonAssetBurn) {
				b.AssetID = "01"
			},
			wantErr: "invalid burn asset_id",
		},
		{
			name: "invalid group key length",
			mutate: func(b *jsonAssetBurn) {
				b.TweakedGroupKey = "02"
			},
			wantErr: "invalid burn tweaked_group_key",
		},
		{
			name: "invalid anchor txid length",
			mutate: func(b *jsonAssetBurn) {
				b.AnchorTxid = "01"
			},
			wantErr: "invalid burn txid",
		},
		{
			name: "invalid collection item amount",
			mutate: func(b *jsonAssetBurn) {
				b.AssetType = assetTypeCollectibleJSON
				b.Amount = "2"
			},
			wantErr: "invalid collectible burn amount",
		},
		{
			name: "unknown asset type",
			mutate: func(b *jsonAssetBurn) {
				b.AssetType = testUnknown
			},
			wantErr: testUnknownBurnAssetType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			burn := validBurn()
			tc.mutate(&burn)

			_, err := unmarshalBurnRecord(&burn)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestUnmarshalVerifyOwnershipResponse(t *testing.T) {
	var (
		txid      [32]byte
		blockHash [32]byte
	)
	for i := range txid {
		txid[i] = byte(i + 1)
		blockHash[i] = byte(32 - i)
	}

	resp, err := unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			ValidProof: true,
			Outpoint: &jsonOutpoint{
				Txid:        hex.EncodeToString(txid[:]),
				OutputIndex: 7,
			},
			BlockHash:   hex.EncodeToString(blockHash[:]),
			BlockHeight: 800000,
		},
	)
	require.NoError(t, err)
	require.True(t, resp.Valid)
	require.Equal(t, uint32(7), resp.Outpoint.Index)
	require.Equal(t, txid, resp.Outpoint.Txid)
	require.Equal(t, tapsdk.Hash(blockHash), resp.BlockHash)
	require.Equal(t, uint32(800000), resp.BlockHeight)

	displayHash := chainhash.Hash(blockHash)
	resp, err = unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			ValidProof: false,
			OutpointStr: fmt.Sprintf(
				"%s:%d", hex.EncodeToString(txid[:]), 3,
			),
			BlockHashStr: displayHash.String(),
		},
	)
	require.NoError(t, err)
	require.False(t, resp.Valid)
	require.Equal(t, uint32(3), resp.Outpoint.Index)
	require.Equal(t, tapsdk.Hash(blockHash), resp.BlockHash)

	resp, err = unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			OutpointStr: fmt.Sprintf(
				"%s:%d", hex.EncodeToString(txid[:]), 4,
			),
			BlockHashStr: displayHash.String(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, tapsdk.Hash(blockHash), resp.BlockHash)

	var fallbackHash [32]byte
	for i := range fallbackHash {
		fallbackHash[i] = 0xff - byte(i)
	}
	resp, err = unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			OutpointStr: fmt.Sprintf(
				"%s:%d", hex.EncodeToString(txid[:]), 5,
			),
			BlockHash: base64.StdEncoding.EncodeToString(
				fallbackHash[:],
			),
		},
	)
	require.NoError(t, err)
	require.Equal(t, tapsdk.Hash(fallbackHash), resp.BlockHash)

	_, err = unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			BlockHash: "01",
		},
	)
	require.ErrorContains(t, err, "invalid block hash length")

	_, err = unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			BlockHash: "not hex or base64",
		},
	)
	require.ErrorContains(t, err, "invalid block_hash")

	_, err = unmarshalVerifyOwnershipResponse(
		&jsonVerifyOwnershipResponse{
			ValidProof: true,
		},
	)
	require.ErrorContains(t, err, "missing outpoint")
}

func TestParseAssetVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "v0",
			input: "ASSET_VERSION_V0",
			want:  0,
		},
		{
			name:  "v1",
			input: testAssetVersionV1,
			want:  1,
		},
		{
			name:  "unknown defaults to v0",
			input: "SOMETHING_ELSE",
			want:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAssetVersion(tc.input)
			require.Equal(t, tc.want, int(got))
		})
	}
}

func TestParseAddressVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  tapsdk.AddressVersion
	}{
		{
			name:  "v0",
			input: "ADDR_VERSION_V0",
			want:  tapsdk.AddressVersionV0,
		},
		{
			name:  "v1",
			input: "ADDR_VERSION_V1",
			want:  tapsdk.AddressVersionV1,
		},
		{
			name:  "v2",
			input: testAddrVersionV2,
			want:  tapsdk.AddressVersionV2,
		},
		{
			name:  "unknown defaults to v0",
			input: "ADDR_VERSION_UNKNOWN",
			want:  tapsdk.AddressVersionV0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAddressVersion(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseBackupMode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  tapsdk.BackupMode
	}{
		{
			name:  "raw",
			input: testRaw,
			want:  tapsdk.BackupModeRaw,
		},
		{
			name:  "compact",
			input: "COMPACT",
			want:  tapsdk.BackupModeCompact,
		},
		{
			name:  "optimistic",
			input: "OPTIMISTIC",
			want:  tapsdk.BackupModeOptimistic,
		},
		{
			name:  "unknown defaults to raw",
			input: "SOMETHING_ELSE",
			want:  tapsdk.BackupModeRaw,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parseBackupMode(tc.input))
		})
	}
}

func TestMarshalBackupMode(t *testing.T) {
	tests := []struct {
		name string
		mode tapsdk.BackupMode
		want string
	}{
		{
			name: "raw",
			mode: tapsdk.BackupModeRaw,
			want: testRaw,
		},
		{
			name: "compact",
			mode: tapsdk.BackupModeCompact,
			want: "COMPACT",
		},
		{
			name: "optimistic",
			mode: tapsdk.BackupModeOptimistic,
			want: "OPTIMISTIC",
		},
		{
			name: "unknown defaults to raw",
			mode: tapsdk.BackupMode(99),
			want: testRaw,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, marshalBackupMode(tc.mode))
		})
	}
}

func TestParseBatchState(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  tapsdk.BatchState
	}{
		{
			name:  "pending",
			input: "BATCH_STATE_PENDING",
			want:  tapsdk.BatchStatePending,
		},
		{
			name:  "frozen",
			input: "BATCH_STATE_FROZEN",
			want:  tapsdk.BatchStateFrozen,
		},
		{
			name:  "confirmed",
			input: "BATCH_STATE_CONFIRMED",
			want:  tapsdk.BatchStateConfirmed,
		},
		{
			name:  "finalized",
			input: "BATCH_STATE_FINALIZED",
			want:  tapsdk.BatchStateFinalized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBatchState(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnmarshalInfo(t *testing.T) {
	resp := &jsonGetInfoResponse{
		Version:    "0.4.0-beta",
		LndVersion: "0.18.0",
		Network:    "testnet",
		LndIdentityPubkey: "02" + "ab" + "cd" + "ef" +
			"0123456789abcdef0123456789" +
			"abcdef0123456789abcdef01",
		NodeAlias:   "testnode",
		BlockHeight: 800000,
		SyncToChain: true,
	}

	info, err := unmarshalInfo(resp)
	require.NoError(t, err)
	require.Equal(t, "0.4.0-beta", info.Version)
	require.Equal(t, "0.18.0", info.LndVersion)
	require.Equal(t, "testnet", info.Network)
	require.Equal(t, "testnode", info.NodeAlias)
	require.Equal(t, uint32(800000), info.BlockHeight)
	require.True(t, info.SyncedToChain)
}

func TestMarshalAssetVersionJSON(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  string
	}{
		{
			name:  "v0",
			input: 0,
			want:  "ASSET_VERSION_V0",
		},
		{
			name:  "v1",
			input: 1,
			want:  testAssetVersionV1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalAssetVersionJSON(
				tapsdk.AssetVersion(tc.input),
			)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMarshalAddressVersionJSON(t *testing.T) {
	tests := []struct {
		name  string
		input tapsdk.AddressVersion
		want  string
	}{
		{
			name:  "v0",
			input: tapsdk.AddressVersionV0,
			want:  "ADDR_VERSION_V0",
		},
		{
			name:  "v1",
			input: tapsdk.AddressVersionV1,
			want:  "ADDR_VERSION_V1",
		},
		{
			name:  "v2",
			input: tapsdk.AddressVersionV2,
			want:  testAddrVersionV2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalAddressVersionJSON(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMarshalAssetTypeJSON(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  string
	}{
		{
			name:  "normal",
			input: 0,
			want:  assetTypeNormalJSON,
		},
		{
			name:  "collectible",
			input: 1,
			want:  assetTypeCollectibleJSON,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalAssetTypeJSON(
				tapsdk.AssetType(tc.input),
			)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestSplitOutpoint(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  [2]string
	}{
		{
			name:  "standard outpoint",
			input: "abc123:0",
			want:  [2]string{"abc123", "0"},
		},
		{
			name:  "with index",
			input: "deadbeef:42",
			want:  [2]string{"deadbeef", "42"},
		},
		{
			name:  "no colon",
			input: "nocolon",
			want:  [2]string{"nocolon", "0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitOutpoint(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}
