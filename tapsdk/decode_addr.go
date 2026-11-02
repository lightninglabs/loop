package tapsdk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

// Tap address HRPs per Bitcoin network. Kept private because
// Address already exposes the address version; callers do not
// need to pick an HRP — the decoder infers it from the string.
const (
	tapHRPMainnet  = "tapbc"
	tapHRPTestnet  = "taptb"
	tapHRPTestnet4 = "taptb"
	tapHRPRegtest  = "taprt"
	tapHRPSignet   = "taptb"
	tapHRPSimnet   = "tapsb"
)

// Tap address TLV types defined by taproot-assets/address/records.go.
// Kept in sync with the upstream constants.
const (
	addrTLVVersion      uint64 = 0
	addrTLVAssetVersion uint64 = 2
	addrTLVAssetID      uint64 = 4
	addrTLVGroupKey     uint64 = 5
	addrTLVScriptKey    uint64 = 6
	addrTLVInternalKey  uint64 = 8
	addrTLVTapscriptSib uint64 = 9
	addrTLVAmount       uint64 = 10
	addrTLVProofCourier uint64 = 12
)

// DecodeAddress decodes a bech32m-encoded Taproot Asset address
// locally, without contacting tapd. The returned Address populates the
// fields that are recoverable from the address string alone:
//
//   - AssetRef (from group key for grouped assets unless a concrete
//     non-zero asset ID is present, otherwise asset ID)
//   - AddressVersion, AssetVersion
//   - Amount
//   - ScriptKey, InternalKey
//   - TapscriptSibling, ProofCourierAddr
//
// TaprootOutputKey and AssetType are left unset — deriving them
// requires the asset commitment and the asset genesis, both of which
// live server-side.
//
// Unknown odd TLV types are skipped for forward compatibility with
// future protocol extensions.
func DecodeAddress(addr string) (*Address, error) {
	normalized := strings.ToLower(strings.TrimSpace(addr))

	oneIdx := strings.LastIndexByte(normalized, '1')
	if oneIdx <= 0 {
		return nil, fmt.Errorf("invalid tap address: missing HRP " +
			"separator")
	}

	hrp := normalized[:oneIdx]
	if !isKnownTapHRP(hrp) {
		return nil, fmt.Errorf("unknown tap address HRP %q", hrp)
	}

	decHRP, data5, err := bech32.DecodeNoLimit(normalized)
	if err != nil {
		return nil, fmt.Errorf("invalid bech32m: %w", err)
	}
	if decHRP != hrp {
		return nil, fmt.Errorf("bech32m HRP mismatch")
	}

	payload, err := bech32.ConvertBits(data5, 5, 8, false)
	if err != nil {
		return nil, fmt.Errorf("invalid address payload: %w", err)
	}

	result := &Address{Encoded: normalized}
	r := bytes.NewReader(payload)
	scratch := make([]byte, 9)

	var (
		haveAssetID   bool
		haveGroupKey  bool
		assetID       AssetID
		assetIDRef    AssetRef
		groupKeyRef   AssetRef
		prevType      uint64
		seenAnyRecord bool
	)

	for r.Len() > 0 {
		t, err := readBigSize(r, scratch)
		if err != nil {
			return nil, fmt.Errorf("read tlv type: %w", err)
		}

		if seenAnyRecord && t <= prevType {
			return nil, fmt.Errorf(
				"tlv records not sorted: type %d after %d",
				t, prevType,
			)
		}
		prevType = t
		seenAnyRecord = true

		l, err := readBigSize(r, scratch)
		if err != nil {
			return nil, fmt.Errorf("read tlv length: %w", err)
		}

		if l > uint64(r.Len()) {
			return nil, fmt.Errorf("tlv length %d exceeds "+
				"remaining payload %d", l, r.Len())
		}

		value := make([]byte, int(l))
		if _, err := io.ReadFull(r, value); err != nil {
			return nil, fmt.Errorf(
				"read tlv value (type %d): %w", t, err,
			)
		}

		switch t {
		case addrTLVVersion:
			v, err := decodeAddressVersion(value)
			if err != nil {
				return nil, err
			}
			result.AddressVersion = v

		case addrTLVAssetVersion:
			v, err := decodeAssetVersion(value)
			if err != nil {
				return nil, err
			}
			result.AssetVersion = v

		case addrTLVAssetID:
			if len(value) != 32 {
				return nil, fmt.Errorf(
					"bad asset id length %d", len(value),
				)
			}
			var id AssetID
			copy(id[:], value)
			assetID = id
			assetIDRef = AssetRefFromAssetID(id)
			haveAssetID = true

		case addrTLVGroupKey:
			if len(value) != 33 {
				return nil, fmt.Errorf(
					"bad group key length %d", len(value),
				)
			}
			gk, err := ParsePubKey(value)
			if err != nil {
				return nil, fmt.Errorf(
					"bad group key: %w", err,
				)
			}
			groupKeyRef = AssetRefFromGroupKey(gk)
			haveGroupKey = true

		case addrTLVScriptKey:
			if len(value) != 33 {
				return nil, fmt.Errorf(
					"bad script key length %d", len(value),
				)
			}
			pk, err := ParsePubKey(value)
			if err != nil {
				return nil, fmt.Errorf(
					"bad script key: %w", err,
				)
			}
			result.ScriptKey = pk

		case addrTLVInternalKey:
			if len(value) != 33 {
				return nil, fmt.Errorf(
					"bad internal key length %d", len(value),
				)
			}
			pk, err := ParsePubKey(value)
			if err != nil {
				return nil, fmt.Errorf(
					"bad internal key: %w", err,
				)
			}
			result.InternalKey = pk

		case addrTLVTapscriptSib:
			// Preserve the serialised bytes; the SDK does not
			// parse the tapscript preimage structure here.
			result.TapscriptSibling = value

		case addrTLVAmount:
			amt, err := readBigSize(
				bytes.NewReader(value), scratch,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"bad amount: %w", err,
				)
			}
			result.Amount = amt

		case addrTLVProofCourier:
			result.ProofCourierAddr = string(value)

		default:
			// Skip unknown TLVs for forward compatibility. We do
			// not distinguish even/odd here because the decoder
			// is read-only: an even type we don't understand
			// would still fail downstream if the caller tried to
			// use the address for anything meaningful.
		}
	}

	switch {
	case haveGroupKey && haveAssetID && !assetID.IsZero():
		// V2 grouped asset addresses carry a group key and a zero
		// asset ID. If an address also carries a non-zero asset ID,
		// keep the concrete item/tranche as the semantic ref.
		result.AssetRef = assetIDRef

	case haveGroupKey:
		result.AssetRef = groupKeyRef

	case haveAssetID:
		result.AssetRef = assetIDRef
	}

	return result, nil
}

// EncodeAddress is the inverse of DecodeAddress. It accepts an Address
// whose AddressVersion, AssetVersion, AssetRef, key material, amount,
// and proof courier are set, and produces the bech32m-encoded Tap
// address string for the given network.
//
// HRP selection is driven by the network argument so tests and
// application code don't need to know the HRP table. TaprootOutputKey
// and AssetType are ignored — they are not part of the address TLV.
func EncodeAddress(addr *Address, network Network) (string, error) {
	if addr == nil {
		return "", fmt.Errorf("nil address")
	}

	hrp, ok := tapHRPForNetwork(network)
	if !ok {
		return "", fmt.Errorf("unsupported network %q for "+
			"tap address encoding", network)
	}

	var buf bytes.Buffer
	scratch := make([]byte, 9)

	writeTLV := func(t uint64, value []byte) error {
		if err := writeBigSize(&buf, t, scratch); err != nil {
			return err
		}
		if err := writeBigSize(
			&buf, uint64(len(value)), scratch,
		); err != nil {
			return err
		}
		_, err := buf.Write(value)
		return err
	}

	// TLV records must appear in ascending type order.
	if err := writeTLV(
		addrTLVVersion, []byte{byte(addr.AddressVersion - 1)},
	); err != nil {
		return "", err
	}

	if err := writeTLV(
		addrTLVAssetVersion, []byte{byte(addr.AssetVersion)},
	); err != nil {
		return "", err
	}

	if assetID, ok := addr.AssetRef.AssetID(); ok {
		if err := writeTLV(addrTLVAssetID, assetID[:]); err != nil {
			return "", err
		}
	} else {
		// V2 with a group key leaves asset_id all-zero.
		var zero AssetID
		if err := writeTLV(addrTLVAssetID, zero[:]); err != nil {
			return "", err
		}
	}

	if groupKey, ok := addr.AssetRef.GroupKey(); ok {
		if err := writeTLV(
			addrTLVGroupKey, groupKey[:],
		); err != nil {
			return "", err
		}
	}

	if err := writeTLV(
		addrTLVScriptKey, addr.ScriptKey[:],
	); err != nil {
		return "", err
	}

	if err := writeTLV(
		addrTLVInternalKey, addr.InternalKey[:],
	); err != nil {
		return "", err
	}

	if len(addr.TapscriptSibling) > 0 {
		if err := writeTLV(
			addrTLVTapscriptSib, addr.TapscriptSibling,
		); err != nil {
			return "", err
		}
	}

	if addr.Amount > 0 {
		var amountBuf bytes.Buffer
		if err := writeBigSize(
			&amountBuf, addr.Amount, scratch,
		); err != nil {
			return "", err
		}
		if err := writeTLV(
			addrTLVAmount, amountBuf.Bytes(),
		); err != nil {
			return "", err
		}
	}

	if addr.ProofCourierAddr != "" {
		if err := writeTLV(
			addrTLVProofCourier, []byte(addr.ProofCourierAddr),
		); err != nil {
			return "", err
		}
	}

	data5, err := bech32.ConvertBits(buf.Bytes(), 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("pack bits: %w", err)
	}

	encoded, err := bech32.EncodeM(hrp, data5)
	if err != nil {
		return "", fmt.Errorf("bech32m encode: %w", err)
	}

	return encoded, nil
}

// tapHRPForNetwork maps an SDK Network onto the bech32m HRP used by
// Tap addresses. Unrecognised networks return false.
func tapHRPForNetwork(net Network) (string, bool) {
	switch net {
	case NetworkMainnet:
		return tapHRPMainnet, true
	case NetworkTestnet:
		return tapHRPTestnet, true
	case NetworkTestnet4:
		return tapHRPTestnet4, true
	case NetworkRegtest:
		return tapHRPRegtest, true
	case NetworkSignet:
		return tapHRPSignet, true
	case NetworkSimnet:
		return tapHRPSimnet, true
	}

	return "", false
}

// writeBigSize encodes a BigSize-encoded uint64 to w.
func writeBigSize(w io.Writer, val uint64, scratch []byte) error {
	if len(scratch) < 9 {
		return fmt.Errorf("scratch buffer too small")
	}

	switch {
	case val < 0xfd:
		scratch[0] = byte(val)
		_, err := w.Write(scratch[:1])
		return err

	case val <= 0xffff:
		scratch[0] = 0xfd
		binary.BigEndian.PutUint16(scratch[1:3], uint16(val))
		_, err := w.Write(scratch[:3])
		return err

	case val <= 0xffffffff:
		scratch[0] = 0xfe
		binary.BigEndian.PutUint32(scratch[1:5], uint32(val))
		_, err := w.Write(scratch[:5])
		return err

	default:
		scratch[0] = 0xff
		binary.BigEndian.PutUint64(scratch[1:9], val)
		_, err := w.Write(scratch[:9])
		return err
	}
}

func decodeAddressVersion(b []byte) (AddressVersion, error) {
	if len(b) != 1 {
		return 0, fmt.Errorf("bad address version length %d", len(b))
	}

	switch b[0] {
	case 0:
		return AddressVersionV0, nil
	case 1:
		return AddressVersionV1, nil
	case 2:
		return AddressVersionV2, nil
	default:
		return 0, fmt.Errorf("unknown address version %d", b[0])
	}
}

func decodeAssetVersion(b []byte) (AssetVersion, error) {
	if len(b) != 1 {
		return 0, fmt.Errorf("bad asset version length %d", len(b))
	}

	switch b[0] {
	case 0:
		return AssetVersionV0, nil
	case 1:
		return AssetVersionV1, nil
	default:
		return 0, fmt.Errorf("unknown asset version %d", b[0])
	}
}

func isKnownTapHRP(hrp string) bool {
	// tapHRPTestnet, tapHRPTestnet4 and tapHRPSignet share the same
	// wire value ("taptb"), so only distinct values are listed here.
	switch hrp {
	case tapHRPMainnet, tapHRPTestnet, tapHRPRegtest, tapHRPSimnet:
		return true
	}

	return false
}

// readBigSize decodes a BigSize-encoded uint64 (BIP-0174 / LND TLV
// varint) from r. codec.ReadVarInt has the identical implementation
// but importing codec would introduce a package cycle, so the helper
// is inlined.
func readBigSize(r io.Reader, scratch []byte) (uint64, error) {
	if len(scratch) < 8 {
		return 0, fmt.Errorf("scratch buffer too small")
	}

	if _, err := io.ReadFull(r, scratch[:1]); err != nil {
		return 0, err
	}

	disc := scratch[0]
	switch {
	case disc < 0xfd:
		return uint64(disc), nil

	case disc == 0xfd:
		if _, err := io.ReadFull(r, scratch[:2]); err != nil {
			return 0, err
		}
		return uint64(binary.BigEndian.Uint16(scratch[:2])), nil

	case disc == 0xfe:
		if _, err := io.ReadFull(r, scratch[:4]); err != nil {
			return 0, err
		}
		return uint64(binary.BigEndian.Uint32(scratch[:4])), nil

	default:
		if _, err := io.ReadFull(r, scratch[:8]); err != nil {
			return 0, err
		}
		return binary.BigEndian.Uint64(scratch[:8]), nil
	}
}
