package utils

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/input"
)

// MuSig2Sign will create a MuSig2 signature for the passed message using the
// passed raw private keys. Raw keys are interpreted with
// btcec.PrivKeyFromBytes semantics, which normalize 32-byte inputs modulo the
// secp256k1 group order instead of rejecting out-of-range values. It expects
// at least two signing keys.
func MuSig2Sign(version input.MuSig2Version, keys [][32]byte,
	tweaks *input.MuSig2Tweaks, msg [32]byte) ([]byte, error) {

	privKeys := make([]*btcec.PrivateKey, len(keys))
	pubKeys := make([]*btcec.PublicKey, len(keys))

	// First parse the raw private keys and also create the corresponding
	// public keys. This preserves the same normalization semantics used
	// when these raw keys are turned into pubkeys elsewhere in the protocol.
	for i, key := range keys {
		privKeys[i], pubKeys[i] = btcec.PrivKeyFromBytes(key[:])

		// MuSig2 v0.4 expects x-only public keys.
		if version == input.MuSig2Version040 {
			pubKey := pubKeys[i].SerializeCompressed()
			xOnlyPubKey, err := schnorr.ParsePubKey(pubKey[1:])
			if err != nil {
				return nil, fmt.Errorf("error parsing x-only "+
					"pubkey: %v", err)
			}

			pubKeys[i] = xOnlyPubKey
		}
	}

	if len(privKeys) < 2 {
		return nil, fmt.Errorf("need at least two signing keys")
	}

	// Next we'll create MuSig2 sessions for each individual private
	// signing key.
	sessions := make([]input.MuSig2Session, len(privKeys))
	for i, signingKey := range privKeys {
		_, muSigSession, err := input.MuSig2CreateContext(
			version, signingKey, pubKeys, tweaks, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating "+
				"signing context: %v", err)
		}

		sessions[i] = muSigSession
	}

	// Next we'll pass around all public nonces to all MuSig2 sessions so
	// that they become usable for creating the partial signatures.
	for i := range len(privKeys) {
		nonce := sessions[i].PublicNonce()

		for j := range len(privKeys) {
			if i == j {
				// Step over if it's the same session.
				continue
			}

			_, err := sessions[j].RegisterPubNonce(nonce)
			if err != nil {
				return nil, fmt.Errorf("error sharing "+
					"MuSig2 nonces: %v", err)
			}
		}
	}

	// Now that the sessions are properly set up, we can generate
	// each partial signature.
	signatures := make([]*musig2.PartialSignature, len(privKeys))
	for i, session := range sessions {
		sig, err := input.MuSig2Sign(session, msg, true)
		if err != nil {
			return nil, err
		}

		signatures[i] = sig
	}

	// Now that we have all partial sigs we can just combine them to
	// get the final signature.
	var haveAllSigs bool
	for i := 1; i < len(signatures); i++ {
		var err error
		haveAllSigs, err = input.MuSig2CombineSig(
			sessions[0], signatures[i],
		)
		if err != nil {
			return nil, err
		}
	}

	if !haveAllSigs {
		return nil, fmt.Errorf("combining MuSig2 signatures " +
			"failed")
	}

	return sessions[0].FinalSig().Serialize(), nil
}
