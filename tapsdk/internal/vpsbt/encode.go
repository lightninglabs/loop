package vpsbt

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/tapsdk/internal/codec"
)

// KeyLocator identifies the derivation path for a BIP32 key.
type KeyLocator struct {
	// Family is the hardened key family.
	Family uint32

	// Index is the non-hardened key index.
	Index uint32
}

// InteractiveVPacket represents a minimal virtual packet for interactive sends.
// This contains only the fields needed to create a vPacket that can be funded
// by the tapd daemon.
type InteractiveVPacket struct {
	// AssetID is the 32-byte asset identifier.
	AssetID [32]byte

	// Amount is the number of asset units to send.
	Amount uint64

	// ScriptKey is the receiver's script key (tweaked Taproot output key).
	ScriptKey [33]byte

	// AnchorInternalKey is the receiver's anchor output internal key.
	AnchorInternalKey [33]byte

	// AnchorKeyLocator identifies the anchor key's derivation path.
	AnchorKeyLocator KeyLocator

	// AltLeaves optionally carries auxiliary Taproot leaves that should be
	// committed to the output's Taproot Asset tree.
	AltLeaves [][]byte

	// LockTime is the optional lock time for the output.
	LockTime uint64

	// RelativeLockTime is the optional relative lock time.
	RelativeLockTime uint64

	// AnchorOutputIndex is the index of the anchor output in the BTC tx.
	AnchorOutputIndex uint32

	// AssetVersion is the version of the asset (V0 or V1).
	AssetVersion uint8

	// NetworkHRP is the chain params HRP.
	NetworkHRP string

	// CoinType is the BIP-44 coin type for derivation paths.
	CoinType uint32
}

// Encode serializes the interactive vPacket to bytes that can be passed to
// FundVirtualPsbt.
func (v *InteractiveVPacket) Encode() ([]byte, error) {
	// Build the underlying PSBT structure.
	packet, err := v.toPsbt()
	if err != nil {
		return nil, fmt.Errorf("failed to build PSBT: %w", err)
	}

	// Serialize the PSBT.
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("failed to serialize PSBT: %w", err)
	}

	return buf.Bytes(), nil
}

// toPsbt converts the interactive vPacket to a PSBT with custom fields.
func (v *InteractiveVPacket) toPsbt() (*psbt.Packet, error) {
	// Create the placeholder unsigned transaction.
	// For a vPacket with 1 input and 1 output.
	unsignedTx := &wire.MsgTx{
		Version: 2,
		TxIn:    []*wire.TxIn{{}},
		TxOut:   make([]*wire.TxOut, 1),
	}

	// Create the script for the output (pay-to-taproot using the script key).
	// We need to convert the 33-byte compressed key to a 32-byte x-only key.
	pkScript, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_1).
		AddData(v.ScriptKey[1:]).
		Script()
	if err != nil {
		return nil, fmt.Errorf("failed to create pk script: %w", err)
	}

	unsignedTx.TxOut[0] = &wire.TxOut{
		Value:    int64(v.Amount),
		PkScript: pkScript,
	}

	// Create the PSBT packet.
	packet := &psbt.Packet{
		UnsignedTx: unsignedTx,
		Inputs:     make([]psbt.PInput, 1),
		Outputs:    make([]psbt.POutput, 1),
		Unknowns:   v.encodeGlobalFields(),
	}

	// Encode the input.
	packet.Inputs[0].Unknowns = v.encodeInputFields()

	// Encode the output.
	outputUnknowns, pOut, err := v.encodeOutputFields()
	if err != nil {
		return nil, fmt.Errorf("failed to encode output: %w", err)
	}

	packet.Outputs[0] = pOut
	packet.Outputs[0].Unknowns = outputUnknowns

	return packet, nil
}

// encodeGlobalFields encodes the global vPacket fields.
func (v *InteractiveVPacket) encodeGlobalFields() []*psbt.Unknown {
	return []*psbt.Unknown{
		{
			Key:   keyGlobalIsVirtualTx,
			Value: []byte{0x01}, // true
		},
		{
			Key:   keyGlobalChainParamsHRP,
			Value: []byte(v.NetworkHRP),
		},
		{
			Key:   keyGlobalPsbtVersion,
			Value: []byte{VPacketVersionV1},
		},
	}
}

// encodeInputFields encodes the input fields.
// For ForInteractiveSend, only the asset ID is set in PrevID.
func (v *InteractiveVPacket) encodeInputFields() []*psbt.Unknown {
	// Encode PrevID with just the asset ID (outpoint and script key are zero).
	prevIDBytes := encodePrevID(v.AssetID)

	return []*psbt.Unknown{
		{
			Key:   keyInputPrevID,
			Value: prevIDBytes,
		},
	}
}

// encodeOutputFields encodes the output fields.
func (v *InteractiveVPacket) encodeOutputFields() ([]*psbt.Unknown,
	psbt.POutput, error) {

	unknowns := []*psbt.Unknown{
		// Type = Simple (0)
		{
			Key:   keyOutputType,
			Value: encodeTLVUint8(VOutputTypeSimple),
		},
		// Interactive = true
		{
			Key:   keyOutputIsInteractive,
			Value: []byte{0x01},
		},
		// Anchor output index
		{
			Key:   keyOutputAnchorOutputIndex,
			Value: encodeTLVUint64(uint64(v.AnchorOutputIndex)),
		},
		// Anchor output internal key
		{
			Key:   keyOutputAnchorOutputInternalKey,
			Value: v.AnchorInternalKey[:],
		},
		// Asset version
		{
			Key:   keyOutputAssetVersion,
			Value: encodeTLVUint8(v.AssetVersion),
		},
	}

	// Add BIP32 derivation info for anchor key.
	bip32Deriv, trBip32Deriv := makeBip32Derivation(
		v.AnchorInternalKey,
		v.AnchorKeyLocator,
		v.CoinType,
	)

	// For BIP32 derivation, the key must contain the pubkey.
	bip32Key := append([]byte(nil), keyOutputAnchorOutputBip32Derivation...)
	bip32Key = append(bip32Key, bip32Deriv.PubKey...)

	// For Taproot BIP32 derivation, the key must contain the x-only pubkey.
	trBip32Key := append([]byte(nil), keyOutputAnchorOutputTrBip32Derivation...)
	trBip32Key = append(trBip32Key, trBip32Deriv.XOnlyPubKey...)

	unknowns = append(unknowns,
		&psbt.Unknown{
			Key:   bip32Key,
			Value: encodeBip32Derivation(bip32Deriv),
		},
		&psbt.Unknown{
			Key:   trBip32Key,
			Value: encodeTaprootBip32Derivation(trBip32Deriv),
		},
	)

	// Add lock time fields if set.
	if v.LockTime > 0 {
		unknowns = append(unknowns, &psbt.Unknown{
			Key:   keyOutputLockTime,
			Value: encodeTLVUint64(v.LockTime),
		})
	}
	if v.RelativeLockTime > 0 {
		unknowns = append(unknowns, &psbt.Unknown{
			Key:   keyOutputRelativeLockTime,
			Value: encodeTLVUint64(v.RelativeLockTime),
		})
	}

	// Encode alt leaves if provided.
	if len(v.AltLeaves) > 0 {
		encodedAltLeaves, err := codec.EncodeAltLeaves(v.AltLeaves)
		if err != nil {
			return nil, psbt.POutput{}, err
		}

		unknowns = append(unknowns, &psbt.Unknown{
			Key:   keyOutputAltLeaves,
			Value: encodedAltLeaves,
		})
	}

	// Create the PSBT output with script key derivation info.
	pOut := psbt.POutput{
		TaprootInternalKey: v.ScriptKey[1:],
	}

	return unknowns, pOut, nil
}

// encodePrevID encodes a PrevID with only the asset ID set.
// Format: OutPoint (36 bytes) + ID (32 bytes) + ScriptKey (33 bytes).
func encodePrevID(assetID [32]byte) []byte {
	var buf bytes.Buffer

	// OutPoint: 32-byte txid (zeros) + 4-byte index (zeros)
	buf.Write(make([]byte, 36))

	// Asset ID (32 bytes)
	buf.Write(assetID[:])

	// ScriptKey: 33-byte compressed pubkey (zeros)
	buf.Write(make([]byte, 33))

	return buf.Bytes()
}

// encodeTLVUint64 encodes a uint64 in TLV format.
func encodeTLVUint64(val uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, val)
	return buf
}

// encodeTLVUint8 encodes a uint8 in TLV format.
func encodeTLVUint8(val uint8) []byte {
	return []byte{val}
}

// makeBip32Derivation creates BIP32 derivation info for a key.
func makeBip32Derivation(pubKey [33]byte, loc KeyLocator,
	coinType uint32) (*psbt.Bip32Derivation, *psbt.TaprootBip32Derivation) {

	bip32Path := []uint32{
		BIP0043Purpose + HardenedKeyStart,
		coinType + HardenedKeyStart,
		loc.Family + HardenedKeyStart,
		0,
		loc.Index,
	}

	bip32Deriv := &psbt.Bip32Derivation{
		PubKey:    pubKey[:],
		Bip32Path: bip32Path,
	}

	trBip32Deriv := &psbt.TaprootBip32Derivation{
		XOnlyPubKey: pubKey[1:],
		Bip32Path:   bip32Path,
		LeafHashes:  make([][]byte, 0),
	}

	return bip32Deriv, trBip32Deriv
}

// encodeBip32Derivation serializes a Bip32Derivation.
// Format: master fingerprint (4 bytes) + path elements.
func encodeBip32Derivation(d *psbt.Bip32Derivation) []byte {
	var buf bytes.Buffer

	// Master key fingerprint (4 bytes, big-endian)
	fingerprint := make([]byte, 4)
	binary.BigEndian.PutUint32(fingerprint, d.MasterKeyFingerprint)
	buf.Write(fingerprint)

	// Path elements (4 bytes each, little-endian)
	for _, elem := range d.Bip32Path {
		elemBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(elemBytes, elem)
		buf.Write(elemBytes)
	}

	return buf.Bytes()
}

// encodeTaprootBip32Derivation serializes a TaprootBip32Derivation.
func encodeTaprootBip32Derivation(d *psbt.TaprootBip32Derivation) []byte {
	var buf bytes.Buffer

	// Number of leaf hashes (compact size uint)
	buf.WriteByte(byte(len(d.LeafHashes)))

	// Leaf hashes (each 32 bytes)
	for _, hash := range d.LeafHashes {
		buf.Write(hash)
	}

	// Master key fingerprint (4 bytes, big-endian)
	fingerprint := make([]byte, 4)
	binary.BigEndian.PutUint32(fingerprint, d.MasterKeyFingerprint)
	buf.Write(fingerprint)

	// Path elements (4 bytes each, little-endian)
	for _, elem := range d.Bip32Path {
		elemBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(elemBytes, elem)
		buf.Write(elemBytes)
	}

	return buf.Bytes()
}
