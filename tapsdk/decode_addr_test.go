package tapsdk

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/stretchr/testify/require"
)

// Test vectors cover the three address versions, fungible (group) vs.
// collectible (asset-id) refs, and the optional amount/tapscript
// sibling fields. Round-tripping through EncodeAddress + DecodeAddress
// is the primary invariant we care about: the SDK builds both halves
// locally, so anything that falls out of the loop is a genuine bug.

// derivePub returns a valid compressed secp256k1 public key derived
// from the given small scalar. Using real scalars keeps the test
// independent of hardcoded fixtures that may not actually lie on the
// curve.
func derivePub(t *testing.T, scalar byte) PubKey {
	t.Helper()

	priv, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	pk, err := ParsePubKey(priv.PubKey().SerializeCompressed())
	require.NoError(t, err)
	return pk
}

// testAssetID and derivePub below share naming with fixtures already
// defined in asset_ref_test.go; we reuse those here.

func TestDecodeAddress_RoundTrip_V0(t *testing.T) {
	t.Parallel()

	original := &Address{
		AddressVersion: AddressVersionV0,
		AssetVersion:   AssetVersionV0,
		AssetRef:       AssetRefFromAssetID(testAssetID()),
		Amount:         1000,
		ScriptKey:      derivePub(t, 7),
		InternalKey:    derivePub(t, 11),
	}

	encoded, err := EncodeAddress(original, NetworkMainnet)
	require.NoError(t, err)
	require.Contains(t, encoded, "tapbc1")

	decoded, err := DecodeAddress(encoded)
	require.NoError(t, err)

	require.Equal(t, original.AddressVersion, decoded.AddressVersion)
	require.Equal(t, original.AssetVersion, decoded.AssetVersion)
	require.Equal(t, original.AssetRef, decoded.AssetRef)
	require.Equal(t, original.Amount, decoded.Amount)
	require.Equal(t, original.ScriptKey, decoded.ScriptKey)
	require.Equal(t, original.InternalKey, decoded.InternalKey)
	require.Equal(t, encoded, decoded.Encoded)
}

func TestDecodeAddress_RoundTrip_V2_GroupKey_NoAmount(t *testing.T) {
	t.Parallel()

	original := &Address{
		AddressVersion: AddressVersionV2,
		AssetVersion:   AssetVersionV1,
		AssetRef:       AssetRefFromGroupKey(derivePub(t, 13)),
		// V2 with no embedded amount (sender-chosen).
		Amount:           0,
		ScriptKey:        derivePub(t, 7),
		InternalKey:      derivePub(t, 11),
		ProofCourierAddr: "authmailbox+universerpc://courier:10029",
	}

	encoded, err := EncodeAddress(original, NetworkRegtest)
	require.NoError(t, err)
	require.Contains(t, encoded, "taprt1")

	decoded, err := DecodeAddress(encoded)
	require.NoError(t, err)

	require.Equal(t, original.AddressVersion, decoded.AddressVersion)
	require.Equal(t, original.AssetRef, decoded.AssetRef)
	require.Equal(t, uint64(0), decoded.Amount)
	require.Equal(
		t, original.ProofCourierAddr, decoded.ProofCourierAddr,
	)
}

func TestDecodeAddress_CollectionItemPrefersAssetID(t *testing.T) {
	t.Parallel()

	assetID := testAssetID()
	encoded := encodeAddressWithAssetIDAndGroupKey(
		t, assetID, testGroupKey(t),
	)

	decoded, err := DecodeAddress(encoded)
	require.NoError(t, err)
	require.Equal(t, AssetRefFromAssetID(assetID), decoded.AssetRef)
}

func TestDecodeAddress_RejectsOversizedTLVLength(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	scratch := make([]byte, 9)
	require.NoError(t, writeBigSize(&buf, addrTLVVersion, scratch))
	require.NoError(t, writeBigSize(&buf, math.MaxUint64, scratch))

	data5, err := bech32.ConvertBits(buf.Bytes(), 8, 5, true)
	require.NoError(t, err)

	encoded, err := bech32.EncodeM(tapHRPRegtest, data5)
	require.NoError(t, err)

	_, err = DecodeAddress(encoded)
	require.ErrorContains(t, err, "tlv length")
}

func TestDecodeAddress_RoundTrip_WithTapscriptSibling(t *testing.T) {
	t.Parallel()

	sibling := []byte{0x01, 0x20, 0xde, 0xad, 0xbe, 0xef}

	original := &Address{
		AddressVersion:   AddressVersionV1,
		AssetVersion:     AssetVersionV0,
		AssetRef:         AssetRefFromAssetID(testAssetID()),
		Amount:           250,
		ScriptKey:        derivePub(t, 7),
		InternalKey:      derivePub(t, 11),
		TapscriptSibling: sibling,
	}

	encoded, err := EncodeAddress(original, NetworkTestnet)
	require.NoError(t, err)

	decoded, err := DecodeAddress(encoded)
	require.NoError(t, err)
	require.Equal(t, sibling, decoded.TapscriptSibling)
	require.Equal(t, uint64(250), decoded.Amount)
}

func encodeAddressWithAssetIDAndGroupKey(t *testing.T, assetID AssetID,
	groupKey PubKey) string {

	t.Helper()

	var buf bytes.Buffer
	scratch := make([]byte, 9)

	writeTLV := func(tlvType uint64, value []byte) {
		require.NoError(t, writeBigSize(&buf, tlvType, scratch))
		require.NoError(
			t, writeBigSize(&buf, uint64(len(value)), scratch),
		)
		_, err := buf.Write(value)
		require.NoError(t, err)
	}

	writeTLV(addrTLVVersion, []byte{byte(AddressVersionV2 - 1)})
	writeTLV(addrTLVAssetVersion, []byte{byte(AssetVersionV1)})
	writeTLV(addrTLVAssetID, assetID[:])
	writeTLV(addrTLVGroupKey, groupKey[:])
	scriptKey := derivePub(t, 7)
	internalKey := derivePub(t, 11)
	writeTLV(addrTLVScriptKey, scriptKey[:])
	writeTLV(addrTLVInternalKey, internalKey[:])

	data5, err := bech32.ConvertBits(buf.Bytes(), 8, 5, true)
	require.NoError(t, err)

	encoded, err := bech32.EncodeM(tapHRPRegtest, data5)
	require.NoError(t, err)

	return encoded
}

func TestDecodeAddress_LargeAmount(t *testing.T) {
	t.Parallel()

	original := &Address{
		AddressVersion: AddressVersionV2,
		AssetRef:       AssetRefFromGroupKey(derivePub(t, 13)),
		Amount:         uint64(1) << 40, // forces 4-byte BigSize path
		ScriptKey:      derivePub(t, 7),
		InternalKey:    derivePub(t, 11),
	}

	encoded, err := EncodeAddress(original, NetworkMainnet)
	require.NoError(t, err)

	decoded, err := DecodeAddress(encoded)
	require.NoError(t, err)
	require.Equal(t, original.Amount, decoded.Amount)
}

func TestDecodeAddress_RejectsUnknownHRP(t *testing.T) {
	t.Parallel()

	// Encode against a known HRP, then rewrite the HRP to a
	// syntactically valid but unknown prefix.
	addr := &Address{
		AddressVersion: AddressVersionV0,
		AssetRef:       AssetRefFromAssetID(testAssetID()),
		Amount:         1,
		ScriptKey:      derivePub(t, 7),
		InternalKey:    derivePub(t, 11),
	}
	encoded, err := EncodeAddress(addr, NetworkMainnet)
	require.NoError(t, err)

	mutilated := "bogus" + encoded[len("tapbc"):]
	_, err = DecodeAddress(mutilated)
	require.ErrorContains(t, err, "unknown tap address HRP")
}

func TestDecodeAddress_RejectsMissingSeparator(t *testing.T) {
	t.Parallel()

	_, err := DecodeAddress("nothascensep")
	require.ErrorContains(t, err, "missing HRP separator")
}

func TestDecodeAddress_RejectsGarbageBech32(t *testing.T) {
	t.Parallel()

	_, err := DecodeAddress("tapbc1xxxxxxxx")
	require.Error(t, err)
}

func TestDecodeAddress_TestnetHRPRoundTrips(t *testing.T) {
	t.Parallel()

	// Testnet/Testnet4/Signet all share the "taptb" HRP; exercising
	// any of them through the encode/decode pair confirms the HRP
	// table.
	addr := &Address{
		AddressVersion: AddressVersionV0,
		AssetRef:       AssetRefFromAssetID(testAssetID()),
		Amount:         7,
		ScriptKey:      derivePub(t, 7),
		InternalKey:    derivePub(t, 11),
	}

	for _, net := range []Network{
		NetworkTestnet, NetworkTestnet4, NetworkSignet,
	} {
		encoded, err := EncodeAddress(addr, net)
		require.NoErrorf(t, err, "encode %s", net)
		require.Contains(t, encoded, "taptb1")

		decoded, err := DecodeAddress(encoded)
		require.NoErrorf(t, err, "decode %s", net)
		require.Equal(t, addr.AssetRef, decoded.AssetRef)
	}
}
