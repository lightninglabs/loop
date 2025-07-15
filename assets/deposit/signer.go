package deposit

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
)

type MuSig2Signer struct {
	signer lndclient.SignerClient

	scriptKey      *btcec.PublicKey
	otherPubKey    *btcec.PublicKey
	anchorRootHash []byte

	session input.MuSig2Session
}

func NewMuSig2Signer(signer lndclient.SignerClient, deposit *Kit, funder bool,
	anchorRootHash []byte) *MuSig2Signer {

	scriptKey := deposit.FunderScriptKey
	otherPubKey := deposit.CoSignerInternalKey
	if !funder {
		scriptKey = deposit.CoSignerScriptKey
		otherPubKey = deposit.FunderInternalKey
	}

	return &MuSig2Signer{
		signer:         signer,
		scriptKey:      scriptKey,
		otherPubKey:    otherPubKey,
		anchorRootHash: anchorRootHash,
	}
}

func (s *MuSig2Signer) NewSession(ctx context.Context) error {
	// Derive the internal key that will be used to sign the message.
	signingPubKey, signingPrivKey, err := DeriveSharedDepositKey(
		ctx, s.signer, s.scriptKey,
	)
	if err != nil {
		return err
	}

	pubKeys := []*btcec.PublicKey{
		signingPubKey, s.otherPubKey,
	}

	tweaks := &input.MuSig2Tweaks{
		TaprootTweak: s.anchorRootHash,
	}

	_, session, err := input.MuSig2CreateContext(
		input.MuSig2Version100RC2, signingPrivKey, pubKeys, tweaks, nil,
	)
	if err != nil {
		return fmt.Errorf("error creating signing context: %w", err)
	}

	s.session = session

	return nil
}

func (s *MuSig2Signer) Session() input.MuSig2Session {
	return s.session
}

func (s *MuSig2Signer) PubNonce() ([musig2.PubNonceSize]byte, error) {
	// If we don't have a session, we can't return a public nonce.
	if s.session == nil {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf("no session " +
			"available")
	}

	// Return the public nonce of the current session.
	return s.session.PublicNonce(), nil
}

// PartialSignMuSig2 is used to partially sign a message hash with the deposit's
// keys.
func (s *MuSig2Signer) PartialSignMuSig2(otherNonce [musig2.PubNonceSize]byte,
	message [32]byte) ([]byte, error) {

	if s.session == nil {
		return nil, fmt.Errorf("no session available")
	}

	_, err := s.session.RegisterPubNonce(otherNonce)
	if err != nil {
		return nil, err
	}

	partialSig, err := input.MuSig2Sign(s.session, message, true)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	err = partialSig.Encode(&buf)
	if err != nil {
		return nil, fmt.Errorf("error encoding partial sig: %w", err)
	}

	return buf.Bytes(), nil
}
