package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
)

const maxAltLeavesSize = 65535

const (
	altLeafTlvTypeScriptVersion uint64 = 14
	altLeafTlvTypeScriptKey     uint64 = 16
)

// EncodeAltLeaf encodes a single alt leaf as a TLV stream. The returned bytes
// are the per-leaf payloads carried inside the alt-leaves container format
// used by tapd.
func EncodeAltLeaf(scriptVersion uint16,
	scriptKey [33]byte) ([]byte, error) {

	if _, err := btcec.ParsePubKey(scriptKey[:]); err != nil {
		return nil, fmt.Errorf("invalid script key: %w", err)
	}

	var (
		buf     bytes.Buffer
		scratch [9]byte
	)

	// type=14 (LeafScriptVersion), len=2, value=uint16 (big-endian).
	err := WriteVarInt(&buf, altLeafTlvTypeScriptVersion, scratch[:])
	if err != nil {
		return nil, err
	}
	if err := WriteVarInt(&buf, 2, scratch[:]); err != nil {
		return nil, err
	}
	var verBytes [2]byte
	binary.BigEndian.PutUint16(verBytes[:], scriptVersion)
	if _, err := buf.Write(verBytes[:]); err != nil {
		return nil, err
	}

	// type=16 (LeafScriptKey), len=33, value=compressed pubkey bytes.
	if err := WriteVarInt(&buf, altLeafTlvTypeScriptKey, scratch[:]); err != nil {
		return nil, err
	}
	if err := WriteVarInt(&buf, 33, scratch[:]); err != nil {
		return nil, err
	}
	if _, err := buf.Write(scriptKey[:]); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// EncodeAltLeaves serializes a list of alt leaves using the minimal format
// expected by tapd: varint leaf count followed by length-prefixed raw leaf
// bytes.
func EncodeAltLeaves(leaves [][]byte) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [9]byte
	)

	if err := WriteVarInt(&buf, uint64(len(leaves)), scratch[:]); err != nil {
		return nil, err
	}

	for _, leaf := range leaves {
		if err := WriteVarInt(&buf, uint64(len(leaf)), scratch[:]); err != nil {
			return nil, err
		}

		if _, err := buf.Write(leaf); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// DecodeAltLeaves decodes a set of alt leaves from the minimal format used by
// tapd: varint leaf count followed by length-prefixed raw leaf bytes.
func DecodeAltLeaves(encoded []byte) ([][]byte, error) {
	if len(encoded) == 0 {
		return nil, nil
	}

	// The total size of all alt leaves is capped by the protocol.
	if len(encoded) > maxAltLeavesSize {
		return nil, fmt.Errorf("alt leaves payload too large: %d", len(encoded))
	}

	r := bytes.NewReader(encoded)
	var scratch [8]byte

	numLeaves, err := ReadVarInt(r, scratch[:])
	if err != nil {
		return nil, fmt.Errorf("read alt leaves count: %w", err)
	}

	if numLeaves == 0 {
		if r.Len() != 0 {
			return nil, fmt.Errorf("alt leaves has trailing bytes")
		}
		return nil, nil
	}

	// Each leaf requires at least a 1-byte length varint, so numLeaves cannot
	// exceed the remaining bytes.
	if numLeaves > uint64(r.Len()) {
		return nil, fmt.Errorf("alt leaves count %d exceeds remaining bytes %d",
			numLeaves, r.Len())
	}

	leaves := make([][]byte, 0, numLeaves)
	for range numLeaves {
		leafLen, err := ReadVarInt(r, scratch[:])
		if err != nil {
			return nil, fmt.Errorf("read alt leaf length: %w", err)
		}

		if leafLen > uint64(r.Len()) {
			return nil, fmt.Errorf("alt leaf length %d exceeds remaining bytes %d",
				leafLen, r.Len())
		}

		leaf := make([]byte, leafLen)
		if _, err := io.ReadFull(r, leaf); err != nil {
			return nil, fmt.Errorf("read alt leaf bytes: %w", err)
		}

		leaves = append(leaves, leaf)
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("alt leaves has trailing bytes")
	}

	return leaves, nil
}
