package lsat

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// LatestVersion is the latest version used for minting new LSATs.
	LatestVersion = 0

	// SecretSize is the size in bytes of a LSAT's secret, also known as
	// the root key of the macaroon.
	SecretSize = 32

	// TokenIDSize is the size in bytes of an LSAT's ID encoded in its
	// macaroon identifier.
	TokenIDSize = 32
)

var (
	// byteOrder is the byte order used to encode/decode a macaroon's raw
	// identifier.
	byteOrder = binary.BigEndian

	// ErrUnknownVersion is an error returned when attempting to decode an
	// LSAT identifier with an unknown version.
	ErrUnknownVersion = errors.New("unknown LSAT version")
)

// TokenID is the type that stores the token identifier of an LSAT token.
type TokenID [TokenIDSize]byte

// String returns the hex encoded representation of the token ID as a string.
func (t *TokenID) String() string {
	return hex.EncodeToString(t[:])
}

// MakeIDFromString parses the hex encoded string and parses it into a token ID.
func MakeIDFromString(newID string) (TokenID, error) {
	if len(newID) != hex.EncodedLen(TokenIDSize) {
		return TokenID{}, fmt.Errorf("invalid id string length of %v, "+
			"want %v", len(newID), hex.EncodedLen(TokenIDSize))
	}

	idBytes, err := hex.DecodeString(newID)
	if err != nil {
		return TokenID{}, err
	}
	var id TokenID
	copy(id[:], idBytes)

	return id, nil
}

// Identifier contains the static identifying details of an LSAT. This is
// intended to be used as the identifier of the macaroon within an LSAT.
type Identifier struct {
	// Version is the version of an LSAT. Having a version allows us to
	// introduce new fields to the identifier in a backwards-compatible
	// manner.
	Version uint16

	// PaymentHash is the payment hash linked to an LSAT. Verification of
	// an LSAT depends on a valid payment, which is enforced by ensuring a
	// preimage is provided that hashes to our payment hash.
	PaymentHash lntypes.Hash

	// TokenID is the unique identifier of an LSAT.
	TokenID TokenID
}

// EncodeIdentifier encodes an LSAT's identifier according to its version.
func EncodeIdentifier(w io.Writer, id *Identifier) error {
	if err := binary.Write(w, byteOrder, id.Version); err != nil {
		return err
	}

	switch id.Version {
	// A version 0 identifier consists of its linked payment hash, followed
	// by the token ID.
	case 0:
		if _, err := w.Write(id.PaymentHash[:]); err != nil {
			return err
		}
		_, err := w.Write(id.TokenID[:])
		return err

	default:
		return fmt.Errorf("%w: %v", ErrUnknownVersion, id.Version)
	}
}

// DecodeIdentifier decodes an LSAT's identifier according to its version.
func DecodeIdentifier(r io.Reader) (*Identifier, error) {
	var version uint16
	if err := binary.Read(r, byteOrder, &version); err != nil {
		return nil, err
	}

	switch version {
	// A version 0 identifier consists of its linked payment hash, followed
	// by the token ID.
	case 0:
		var paymentHash lntypes.Hash
		if _, err := r.Read(paymentHash[:]); err != nil {
			return nil, err
		}
		var tokenID TokenID
		if _, err := r.Read(tokenID[:]); err != nil {
			return nil, err
		}

		return &Identifier{
			Version:     version,
			PaymentHash: paymentHash,
			TokenID:     tokenID,
		}, nil

	default:
		return nil, fmt.Errorf("%w: %v", ErrUnknownVersion, version)
	}
}
