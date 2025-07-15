package deposit

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
)

var (
	// tmpServerSecNonce is a secret nonce used for testing.
	// TODO(bhandras): remove once package spending works e2e.
	tmpServerSecNonce = [musig2.SecNonceSize]byte{
		// First 32 bytes: scalar k1
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00,
		0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80,
		0x90, 0xa0, 0xb0, 0xc0, 0xd0, 0xe0, 0xf0, 0x01,

		// Second 32 bytes: scalar k2
		0x02, 0x13, 0x24, 0x35, 0x46, 0x57, 0x68, 0x79,
		0x8a, 0x9b, 0xac, 0xbd, 0xce, 0xdf, 0xf1, 0x02,
		0x14, 0x26, 0x38, 0x4a, 0x5c, 0x6e, 0x70, 0x82,
		0x94, 0xa6, 0xb8, 0xca, 0xdc, 0xee, 0xf1, 0x03,
	}
)

// secNonceToPubNonce takes our two secret nonces, and produces their two
// corresponding EC points, serialized in compressed format.
func secNonceToPubNonce(
	secNonce [musig2.SecNonceSize]byte) [musig2.PubNonceSize]byte {

	var k1Mod, k2Mod btcec.ModNScalar
	k1Mod.SetByteSlice(secNonce[:btcec.PrivKeyBytesLen])
	k2Mod.SetByteSlice(secNonce[btcec.PrivKeyBytesLen:])

	var r1, r2 btcec.JacobianPoint
	btcec.ScalarBaseMultNonConst(&k1Mod, &r1)
	btcec.ScalarBaseMultNonConst(&k2Mod, &r2)

	// Next, we'll convert the key in jacobian format to a normal public
	// key expressed in affine coordinates.
	r1.ToAffine()
	r2.ToAffine()
	r1Pub := btcec.NewPublicKey(&r1.X, &r1.Y)
	r2Pub := btcec.NewPublicKey(&r2.X, &r2.Y)

	var pubNonce [musig2.PubNonceSize]byte

	// The public nonces are serialized as: R1 || R2, where both keys are
	// serialized in compressed format.
	copy(pubNonce[:], r1Pub.SerializeCompressed())
	copy(
		pubNonce[btcec.PubKeyBytesLenCompressed:],
		r2Pub.SerializeCompressed(),
	)

	return pubNonce
}
