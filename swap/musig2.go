package swap

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
)

// NewMuSig2Session creates a new musig session.
func NewMusig2Session(ctx context.Context, lnd *lndclient.LndServices,
	ourKey *keychain.KeyDescriptor, theirKey [33]byte,
	opts ...lndclient.MuSigSessionOpts) (*lndclient.MuSig2Session, error) {

	theirPubkey, err := btcec.ParsePubKey(theirKey[:])
	if err != nil {
		return nil, err
	}

	signers := make([][32]byte, 2)
	copy(signers[0][:], schnorr.SerializePubKey(ourKey.PubKey))
	copy(signers[1][:], schnorr.SerializePubKey(theirPubkey))

	return lnd.Signer.NewMuSig2Session(
		ctx, &ourKey.KeyLocator, signers, opts...,
	)
}
