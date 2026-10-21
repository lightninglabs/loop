package script

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CreateMuSig2Session starts a normal LND session for the complete Taproot
// root. Asset deposits include the asset commitment alongside the CSV leaf.
// The caller owns this ephemeral session and retires it after any ambiguity.
func CreateMuSig2Session(ctx context.Context, signer lndclient.SignerClient,
	key keychain.KeyLocator, participants [][]byte, root []byte) (
	*input.MuSig2SessionInfo, error) {

	keys := make([]*btcec.PublicKey, len(participants))
	for i, raw := range participants {
		var err error
		keys[i], err = btcec.ParsePubKey(raw)
		if err != nil {
			return nil, err
		}
	}
	expected, err := input.MuSig2CombineKeys(input.MuSig2Version100RC2,
		keys, true, &input.MuSig2Tweaks{
			TaprootTweak: root, TaprootBIP0086Tweak: len(root) == 0,
		})
	if err != nil {
		return nil, err
	}
	session, err := signer.MuSig2CreateSession(ctx, input.MuSig2Version100RC2,
		&key, participants, lndclient.MuSig2TaprootTweakOpt(root, len(root) == 0))
	if err != nil {
		return nil, err
	}
	if session != nil && session.CombinedKey != nil &&
		bytes.Equal(schnorr.SerializePubKey(session.CombinedKey),
			schnorr.SerializePubKey(expected.FinalKey)) {

		return session, nil
	}
	if session != nil {
		cleanup, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		_ = signer.MuSig2Cleanup(cleanup, session.SessionID)
	}
	return nil, errors.New("LND returned an unexpected MuSig2 output key")
}
