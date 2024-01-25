package utils

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/input"
)

// MuSig2Sign will create a MuSig2 signature for the passed message using the
// passed private keys.
func MuSig2Sign(version input.MuSig2Version, privKeys []*btcec.PrivateKey,
	pubKeys []*btcec.PublicKey, tweaks *input.MuSig2Tweaks,
	msg [32]byte) ([]byte, error) {

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
	for i := 0; i < len(privKeys); i++ {
		nonce := sessions[i].PublicNonce()

		for j := 0; j < len(privKeys); j++ {
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
		return nil, fmt.Errorf("combinging MuSig2 signatures " +
			"failed")
	}

	return sessions[0].FinalSig().Serialize(), nil
}
