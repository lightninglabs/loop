package tapsdk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strconv"
	"testing"

	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestAssetProofPathMalformedRecordLength rejects truncated TLV records without
// allocating their declared length. The proof file itself fits comfortably
// within the proof-path budget, including its valid checksum.
func TestAssetProofPathMalformedRecordLength(t *testing.T) {
	for _, length := range []uint64{math.MaxInt64, math.MaxUint64} {
		t.Run(strconv.FormatUint(length, 10), func(t *testing.T) {
			var raw bytes.Buffer
			raw.Write(proof.PrefixMagicBytes[:])
			var scratch [8]byte
			require.NoError(t, tlv.WriteVarInt(&raw, 65535, &scratch))
			require.NoError(t, tlv.WriteVarInt(&raw, length, &scratch))

			var file bytes.Buffer
			file.Write(proof.FilePrefixMagicBytes[:])
			require.NoError(t, binary.Write(
				&file, binary.BigEndian, uint32(proof.V0),
			))
			require.NoError(t, tlv.WriteVarInt(&file, 1, &scratch))
			require.NoError(t, tlv.WriteVarInt(
				&file, uint64(raw.Len()), &scratch,
			))
			file.Write(raw.Bytes())
			var previous [sha256.Size]byte
			hash := sha256.New()
			hash.Write(previous[:])
			hash.Write(raw.Bytes())
			file.Write(hash.Sum(nil))

			path := &sdk.AssetProofPath{ConfirmedBaseProof: file.Bytes()}
			var err error
			require.NotPanics(t, func() {
				err = path.Validate()
			})
			require.ErrorIs(t, err, sdk.ErrAssetProofPathInvalid)
		})
	}
}
