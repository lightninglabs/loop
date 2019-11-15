package lsat

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"gopkg.in/macaroon.v2"
)

var (
	// zeroPreimage is an empty, invalid payment preimage that is used to
	// initialize pending tokens with.
	zeroPreimage lntypes.Preimage
)

// Token is the main type to store an LSAT token in.
type Token struct {
	// PaymentHash is the hash of the LSAT invoice that needs to be paid.
	// Knowing the preimage to this hash is seen as proof of payment by the
	// authentication server.
	PaymentHash lntypes.Hash

	// Preimage is the proof of payment indicating that the token has been
	// paid for if set. If the preimage is empty, the payment might still
	// be in transit.
	Preimage lntypes.Preimage

	// AmountPaid is the total amount in msat that the user paid to get the
	// token. This does not include routing fees.
	AmountPaid lnwire.MilliSatoshi

	// RoutingFeePaid is the total amount in msat that the user paid in
	// routing fee to get the token.
	RoutingFeePaid lnwire.MilliSatoshi

	// TimeCreated is the moment when this token was created.
	TimeCreated time.Time

	// baseMac is the base macaroon in its original form as baked by the
	// authentication server. No client side caveats have been added to it
	// yet.
	baseMac *macaroon.Macaroon
}

// tokenFromChallenge parses the parts that are present in the challenge part
// of the LSAT auth protocol which is the macaroon and the payment hash.
func tokenFromChallenge(baseMac []byte, paymentHash *[32]byte) (*Token, error) {
	// First, validate that the macaroon is valid and can be unmarshaled.
	mac := &macaroon.Macaroon{}
	err := mac.UnmarshalBinary(baseMac)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal macaroon: %v", err)
	}

	token := &Token{
		TimeCreated: time.Now(),
		baseMac:     mac,
		Preimage:    zeroPreimage,
	}
	hash, err := lntypes.MakeHash(paymentHash[:])
	if err != nil {
		return nil, err
	}
	token.PaymentHash = hash
	return token, nil
}

// BaseMacaroon returns the base macaroon as received from the authentication
// server.
func (t *Token) BaseMacaroon() *macaroon.Macaroon {
	return t.baseMac.Clone()
}

// PaidMacaroon returns the base macaroon with the proof of payment (preimage)
// added as a first-party-caveat.
func (t *Token) PaidMacaroon() (*macaroon.Macaroon, error) {
	mac := t.BaseMacaroon()
	err := AddFirstPartyCaveats(
		mac, NewCaveat(PreimageKey, t.Preimage.String()),
	)
	if err != nil {
		return nil, err
	}
	return mac, nil
}

// IsValid returns true if the timestamp contained in the base macaroon is not
// yet expired.
func (t *Token) IsValid() bool {
	// TODO(guggero): Extract and validate from caveat once we add an
	//  expiration date to the LSAT.
	return true
}

// isPending returns true if the payment for the LSAT is still in flight and we
// haven't received the preimage yet.
func (t *Token) isPending() bool {
	return t.Preimage == zeroPreimage
}

// serializeToken returns a byte-serialized representation of the token.
func serializeToken(t *Token) ([]byte, error) {
	var b bytes.Buffer

	baseMacBytes, err := t.baseMac.MarshalBinary()
	if err != nil {
		return nil, err
	}

	macLen := uint32(len(baseMacBytes))
	if err := binary.Write(&b, byteOrder, macLen); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, baseMacBytes); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, t.PaymentHash); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, t.Preimage); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, t.AmountPaid); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, t.RoutingFeePaid); err != nil {
		return nil, err
	}

	timeUnix := t.TimeCreated.UnixNano()
	if err := binary.Write(&b, byteOrder, timeUnix); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// deserializeToken constructs a token by reading it from a byte slice.
func deserializeToken(value []byte) (*Token, error) {
	r := bytes.NewReader(value)

	var macLen uint32
	if err := binary.Read(r, byteOrder, &macLen); err != nil {
		return nil, err
	}

	macBytes := make([]byte, macLen)
	if err := binary.Read(r, byteOrder, &macBytes); err != nil {
		return nil, err
	}

	var paymentHash [lntypes.HashSize]byte
	if err := binary.Read(r, byteOrder, &paymentHash); err != nil {
		return nil, err
	}

	token, err := tokenFromChallenge(macBytes, &paymentHash)
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &token.Preimage); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &token.AmountPaid); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &token.RoutingFeePaid); err != nil {
		return nil, err
	}

	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	token.TimeCreated = time.Unix(0, unixNano)

	return token, nil
}
