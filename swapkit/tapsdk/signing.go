package tapsdk

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// ErrSigningRound means a signing request or response failed its fixed binding.
var ErrSigningRound = errors.New("invalid asset signing round")

// SigningRound is one ephemeral LND session for a fixed fallback transaction.
// Abandon it after any signing ambiguity. A retry creates fresh nonces, while
// the caller retains the same unsigned package and payment hash in its store.
// Only the complete fallback is persisted; no private nonce leaves LND.
type SigningRound struct {
	signer  lndclient.SignerClient
	pkg     *sdk.CustomAnchorTransferPackage
	request sdk.CustomAnchorMuSig2SigningRequest
	session *input.MuSig2SessionInfo
	used    atomic.Bool
}

// NewSigningRound starts a fresh wallet session for the package's sole MuSig2
// input. The complete root includes both the Bitcoin policy and asset tree.
func NewSigningRound(ctx context.Context, signer lndclient.SignerClient,
	key keychain.KeyLocator, pkg *sdk.CustomAnchorTransferPackage) (
	*SigningRound, error) {

	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.MuSig2) != 1 {
		return nil, ErrSigningRound
	}
	request := requests.MuSig2[0]
	keys := make([][]byte, len(request.Participants))
	for i := range keys {
		keys[i] = request.Participants[i][:]
	}
	session, err := signer.MuSig2CreateSession(ctx,
		input.MuSig2Version100RC2, &key, keys,
		lndclient.MuSig2TaprootTweakOpt(request.TaprootMerkleRoot,
			len(request.TaprootMerkleRoot) == 0))
	if err != nil {
		return nil, err
	}
	round := &SigningRound{signer: signer, pkg: pkg,
		request: request, session: session}
	internal, err := schnorr.ParsePubKey(request.InternalKey[:])
	if err != nil || session == nil || session.CombinedKey == nil {
		round.Close(ctx)
		return nil, ErrSigningRound
	}
	// LND returns the combined key in x-only form. Its decoded parity may
	// differ from the tweaked point, but the Taproot output key must match.
	expected := txscript.ComputeTaprootOutputKey(
		internal, request.TaprootMerkleRoot,
	)
	if !bytes.Equal(schnorr.SerializePubKey(session.CombinedKey),
		schnorr.SerializePubKey(expected)) {

		round.Close(ctx)
		return nil, ErrSigningRound
	}
	return round, nil
}

// Nonce returns the public nonce identifying this signing attempt.
func (r *SigningRound) Nonce() [66]byte { return r.session.PublicNonce }

// Close retires an unfinished session. Completed sessions have already been
// deleted by LND; cleanup errors cannot authorize another use of this object.
func (r *SigningRound) Close(ctx context.Context) {
	r.used.Store(true)
	if r.session != nil {
		cleanup, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		_ = r.signer.MuSig2Cleanup(cleanup,
			r.session.SessionID)
	}
}

func (r *SigningRound) sign(ctx context.Context, peer [66]byte,
	cleanup bool) ([]byte, error) {

	if !r.used.CompareAndSwap(false, true) {
		return nil, ErrSigningRound
	}
	all, err := r.signer.MuSig2RegisterNonces(ctx, r.session.SessionID,
		[][66]byte{peer})
	if err != nil {
		return nil, err
	}
	if !all {
		return nil, ErrSigningRound
	}
	return r.signer.MuSig2Sign(ctx, r.session.SessionID,
		r.request.Sighash, cleanup)
}

// Partial signs once and lets LND discard the server session immediately.
// The caller durably records exposure before returning this to the client.
func (r *SigningRound) Partial(ctx context.Context, peer [66]byte) (
	[]byte, error) {

	return r.sign(ctx, peer, true)
}

// Finalize signs once, combines the server partial inside LND, and has the SDK
// verify and apply the signature. The caller finalizes the anchor before
// persisting it. Any error retires this round; never retry it
// with another peer nonce or after losing an LND response.
func (r *SigningRound) Finalize(ctx context.Context, peer [66]byte,
	partial []byte) (*sdk.CustomAnchorTransferPackage, error) {

	if _, err := r.sign(ctx, peer, false); err != nil {
		return nil, err
	}
	all, signature, err := r.signer.MuSig2CombineSig(ctx,
		r.session.SessionID, [][]byte{partial})
	if err != nil {
		return nil, err
	}
	if !all {
		return nil, ErrSigningRound
	}
	if r.request.SighashType != uint32(txscript.SigHashDefault) {
		signature = append(signature, byte(r.request.SighashType))
	}
	return r.pkg.ApplyKeyPathSignature(r.request.ID, signature)
}

// SignScriptPath uses the normal LND signer for a unilateral recovery spend.
// The SDK checks the resulting complete witness against its sealed package.
func SignScriptPath(ctx context.Context, signer lndclient.SignerClient,
	key keychain.KeyLocator, pkg *sdk.CustomAnchorTransferPackage,
	preimage *[32]byte) ([]byte, error) {

	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.ScriptPath) != 1 {
		return nil, ErrSigningRound
	}
	request := requests.ScriptPath[0]
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(pkg.AnchorPsbt), false)
	if err != nil {
		return nil, err
	}
	previous := make([]*wire.TxOut, len(packet.Inputs))
	for i := range packet.Inputs {
		previous[i] = packet.Inputs[i].WitnessUtxo
		if previous[i] == nil {
			return nil, ErrSigningRound
		}
	}
	signatures, err := signer.SignOutputRawKeyLocator(ctx,
		packet.UnsignedTx, []*lndclient.SignDescriptor{{
			KeyDesc:       keychain.KeyDescriptor{KeyLocator: key},
			SignMethod:    input.TaprootScriptSpendSignMethod,
			WitnessScript: request.LeafScript,
			HashType:      txscript.SigHashType(request.SighashType),
			Output:        previous[request.InputIndex],
			InputIndex:    int(request.InputIndex),
		}}, previous)
	if err != nil {
		return nil, err
	}
	if len(signatures) != 1 {
		return nil, ErrSigningRound
	}
	signature := signatures[0]
	if request.SighashType != uint32(txscript.SigHashDefault) {
		signature = append(signature, byte(request.SighashType))
	}
	unlock := [][]byte{signature}
	if preimage != nil {
		unlock = append([][]byte{preimage[:]}, unlock...)
	}
	final, err := pkg.ApplyScriptPathWitness(request.ID, unlock)
	if err != nil {
		return nil, err
	}
	return final.AnchorPsbt, nil
}
