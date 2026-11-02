package tapsdk

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// TaprootAssetsKeyFamily is the key family used to generate internal keys and
// script keys for Taproot Assets. This value must match the constant defined
// in the taproot-assets repository.
const TaprootAssetsKeyFamily = 212

// PubKey is a 33-byte compressed secp256k1 public key.
type PubKey [33]byte

// XOnlyPubKey is a 32-byte x-only (Schnorr/Taproot) public key.
type XOnlyPubKey [32]byte

// KeyLocator identifies a key in the key derivation path.
type KeyLocator struct {
	// Family is the key family (derivation purpose).
	Family uint32

	// Index is the specific key index within the family.
	Index uint32
}

// KeyDescriptor contains key bytes and locator information.
type KeyDescriptor struct {
	// RawKeyBytes is the raw public key bytes (33 bytes compressed).
	RawKeyBytes PubKey

	// KeyLocator identifies the key's derivation path.
	KeyLocator KeyLocator
}

// ScriptKey represents a Taproot Asset script key for receiving assets.
// A script key is composed of an internal key that is optionally tweaked
// with a tap tweak to produce the final public key.
type ScriptKey struct {
	// PubKey is the full Taproot output key the asset is locked to.
	// This is either a BIP-86 key if TapTweak is empty, or a key with
	// the tap tweak applied.
	PubKey PubKey

	// KeyDesc describes the internal key used to derive PubKey.
	KeyDesc KeyDescriptor

	// TapTweak is the optional Taproot tweak applied to the internal key.
	// If empty, a BIP-86 style tweak is applied.
	TapTweak []byte
}

// InternalKey represents an anchor output internal key.
// This key is used as the internal key for the Taproot output that anchors
// the asset commitment on-chain.
type InternalKey struct {
	// PubKey is the raw public key bytes (33 bytes compressed).
	PubKey PubKey

	// KeyLocator identifies the key's derivation path.
	KeyLocator KeyLocator
}

// DerivedKeys contains both keys needed for receiving assets in an
// interactive transfer. The receiver derives these keys and shares them
// with the sender.
type DerivedKeys struct {
	// ScriptKey is used to lock the asset to the receiver.
	ScriptKey ScriptKey

	// InternalKey is used as the internal key for the anchor output.
	InternalKey InternalKey
}

// ParsePubKey parses a 33-byte compressed secp256k1 public key.
func ParsePubKey(b []byte) (PubKey, error) {
	var k PubKey
	if len(b) != btcec.PubKeyBytesLenCompressed {
		return k, fmt.Errorf("public key must be %d bytes, was %d",
			btcec.PubKeyBytesLenCompressed, len(b))
	}

	if _, err := btcec.ParsePubKey(b); err != nil {
		return k, fmt.Errorf("invalid public key: %w", err)
	}

	copy(k[:], b)
	return k, nil
}

// String returns the hex encoding of the public key.
func (k PubKey) String() string {
	return hex.EncodeToString(k[:])
}

// ParseTaprootPubKey parses a Taproot public key from raw bytes. The input can
// be either a 32-byte x-only key or a 33-byte compressed key. The result is
// normalized to 33-byte compressed encoding.
func ParseTaprootPubKey(b []byte) (PubKey, error) {
	var (
		key PubKey
		pk  *btcec.PublicKey
		err error
	)

	switch len(b) {
	case schnorr.PubKeyBytesLen:
		pk, err = schnorr.ParsePubKey(b)
		if err != nil {
			return key, fmt.Errorf("invalid x-only public key: %w",
				err)
		}

	case btcec.PubKeyBytesLenCompressed:
		pk, err = btcec.ParsePubKey(b)
		if err != nil {
			return key, fmt.Errorf("invalid public key: %w", err)
		}

		pk, err = schnorr.ParsePubKey(schnorr.SerializePubKey(pk))
		if err != nil {
			return key, fmt.Errorf("invalid public key: %w", err)
		}

	default:
		return key, fmt.Errorf("taproot public key must be %d or %d bytes, was %d",
			schnorr.PubKeyBytesLen, btcec.PubKeyBytesLenCompressed,
			len(b))
	}

	copy(key[:], pk.SerializeCompressed())
	return key, nil
}

// ParseScriptKey is an alias for ParseTaprootPubKey.
func ParseScriptKey(b []byte) (PubKey, error) {
	return ParseTaprootPubKey(b)
}

// Bytes returns the 33-byte compressed encoding of the public key.
func (k PubKey) Bytes() []byte {
	return k[:]
}

// XOnly returns the 32-byte x-only encoding of the public key.
func (k PubKey) XOnly() XOnlyPubKey {
	var x XOnlyPubKey
	copy(x[:], k[1:])
	return x
}

// Bytes returns the 32-byte x-only encoding of the public key.
func (k XOnlyPubKey) Bytes() []byte {
	return k[:]
}

// ParsePubKeyHex parses a hex-encoded compressed secp256k1 public key.
func ParsePubKeyHex(s string) (PubKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return PubKey{}, fmt.Errorf("invalid hex: %w", err)
	}

	return ParsePubKey(b)
}

// ParseGroupRefKey decodes a hex-encoded group key in either 33-byte
// compressed or 32-byte x-only form. tapd surfaces both encodings for the
// same group depending on which internal code path produced the key; the
// 33-byte form preserves the y-parity byte, the 32-byte form drops it.
// Using schnorr.ParsePubKey on a 33-byte input would silently discard the
// parity and flip odd-y keys, so callers must branch on length. This
// helper absorbs that ambiguity and returns a normalized compressed key.
func ParseGroupRefKey(s string) (PubKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return PubKey{}, fmt.Errorf("invalid group key hex %q: %w",
			s, err)
	}

	switch len(b) {
	case btcec.PubKeyBytesLenCompressed:
		return ParsePubKey(b)

	case schnorr.PubKeyBytesLen:
		return ParseTaprootPubKey(b)

	default:
		return PubKey{}, fmt.Errorf("group key hex %q: expected %d "+
			"or %d bytes, got %d", s, schnorr.PubKeyBytesLen,
			btcec.PubKeyBytesLenCompressed, len(b))
	}
}

// ParseXOnlyPubKey parses a 32-byte x-only (Schnorr/Taproot) public key.
func ParseXOnlyPubKey(b []byte) (XOnlyPubKey, error) {
	var k XOnlyPubKey
	if len(b) != schnorr.PubKeyBytesLen {
		return k, fmt.Errorf("x-only public key must be %d bytes, was %d",
			schnorr.PubKeyBytesLen, len(b))
	}

	if _, err := schnorr.ParsePubKey(b); err != nil {
		return k, fmt.Errorf("invalid x-only public key: %w", err)
	}

	copy(k[:], b)
	return k, nil
}
