package tapsdk

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/script"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/stretchr/testify/require"
)

// Public packages generated with the SDK builder and deterministic test keys.
// They contain no private keys or secret nonces.
//
//go:embed testdata/*.package
var signingFixtures embed.FS

var errLostSigningReply = errors.New("signing reply lost")

// sessionWallet uses LND's real session manager. It never imports or exports a
// private nonce; reconstructing this wallet discards all unfinished sessions.
type sessionWallet struct {
	lndclient.SignerClient

	t                     *testing.T
	manager               *input.MusigSessionManager
	loseSign, loseCombine bool
	wrongKey              bool
}

func signingWallet(t *testing.T, value byte) *sessionWallet {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{value})
	return &sessionWallet{t: t, manager: input.NewMusigSessionManager(
		func(*keychain.KeyDescriptor) (*btcec.PrivateKey, error) {
			return key, nil
		})}
}

func (w *sessionWallet) MuSig2CreateSession(_ context.Context,
	version input.MuSig2Version, key *keychain.KeyLocator,
	pubkeys [][]byte, opts ...lndclient.MuSig2SessionOpts) (
	*input.MuSig2SessionInfo, error) {

	request := &signrpc.MuSig2SessionRequest{}
	for _, opt := range opts {
		opt(request)
	}
	require.Empty(w.t, request.PregeneratedLocalNonce)
	keys := make([]*btcec.PublicKey, len(pubkeys))
	for i, raw := range pubkeys {
		var err error
		keys[i], err = btcec.ParsePubKey(raw)
		require.NoError(w.t, err)
	}
	session, err := w.manager.MuSig2CreateSession(version, *key, keys,
		&input.MuSig2Tweaks{
			TaprootBIP0086Tweak: request.TaprootTweak.KeySpendOnly,
			TaprootTweak:        request.TaprootTweak.ScriptRoot,
		}, nil, nil)
	if err != nil {
		return nil, err
	}
	// Match the signrpc/lndclient boundary, which returns an x-only key.
	result := *session
	result.CombinedKey, err = schnorr.ParsePubKey(
		schnorr.SerializePubKey(session.CombinedKey),
	)
	if w.wrongKey {
		key, _ := btcec.PrivKeyFromBytes([]byte{4})
		result.CombinedKey = key.PubKey()
	}
	return &result, err
}

func (w *sessionWallet) MuSig2RegisterNonces(_ context.Context,
	id [32]byte, nonces [][66]byte) (bool, error) {

	return w.manager.MuSig2RegisterNonces(id, nonces)
}

func (w *sessionWallet) MuSig2Sign(_ context.Context,
	id [32]byte, digest [32]byte, cleanup bool) ([]byte, error) {

	partial, err := w.manager.MuSig2Sign(id, digest, cleanup)
	if err != nil {
		return nil, err
	}
	if w.loseSign {
		return nil, errLostSigningReply
	}
	var raw bytes.Buffer
	err = partial.Encode(&raw)
	return raw.Bytes(), err
}

func (w *sessionWallet) MuSig2CombineSig(_ context.Context,
	id [32]byte, raw [][]byte) (bool, []byte, error) {

	partials := make([]*musig2.PartialSignature, len(raw))
	for i := range raw {
		partials[i] = &musig2.PartialSignature{}
		if err := partials[i].Decode(bytes.NewReader(raw[i])); err != nil {
			return false, nil, err
		}
	}
	signature, all, err := w.manager.MuSig2CombineSig(id, partials)
	if err != nil {
		return false, nil, err
	}
	if w.loseCombine {
		return false, nil, errLostSigningReply
	}
	return all, signature.Serialize(), nil
}

func (w *sessionWallet) MuSig2Cleanup(_ context.Context,
	id [32]byte) error {

	return w.manager.MuSig2Cleanup(id)
}

func signingPackage(t *testing.T) *sdk.CustomAnchorTransferPackage {
	t.Helper()
	raw, err := signingFixtures.ReadFile("testdata/musig.package")
	require.NoError(t, err)
	var pkg sdk.CustomAnchorTransferPackage
	require.NoError(t, pkg.UnmarshalBinary(raw))
	// This SDK fixture has two independent inputs in addition to MuSig2.
	// Complete those first so this test exercises the real full anchor.
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	key, _ := btcec.PrivKeyFromBytes([]byte{2})
	keyRequest := requests.KeyPath[0]
	tweaked := txscript.TweakTaprootPrivKey(*key, keyRequest.TaprootMerkleRoot)
	sig, err := schnorr.Sign(tweaked, keyRequest.Sighash[:])
	require.NoError(t, err)
	complete, err := pkg.ApplyKeyPathSignature(keyRequest.ID, sig.Serialize())
	require.NoError(t, err)
	return complete
}

func TestLNDSessionXOnlyOutputKey(t *testing.T) {
	for name, parity := range map[string]byte{"even": 2, "odd": 3} {
		t.Run(name, func(t *testing.T) {
			pkg := signingPackage(t)
			requests, err := pkg.SigningRequests()
			require.NoError(t, err)
			request := requests.MuSig2[0]
			internal, err := schnorr.ParsePubKey(request.InternalKey[:])
			require.NoError(t, err)
			packet, err := DecodePSBT(pkg.CommittedAnchorPsbt)
			require.NoError(t, err)

			// This fixture's MuSig2 input is an external Bitcoin input.
			// Vary its root to exercise both output parities without
			// changing the participants or asset transfer.
			var root [32]byte
			var output *btcec.PublicKey
			for i := range 256 {
				root[0] = byte(i)
				output = txscript.ComputeTaprootOutputKey(
					internal, root[:],
				)
				if output.SerializeCompressed()[0] == parity {
					break
				}
			}
			require.Equal(t, parity, output.SerializeCompressed()[0])
			input := &packet.Inputs[request.InputIndex]
			input.TaprootMerkleRoot = root[:]
			input.WitnessUtxo.PkScript, err =
				txscript.PayToTaprootScript(output)
			require.NoError(t, err)
			raw, err := EncodePSBT(packet)
			require.NoError(t, err)
			pkg.CommittedAnchorPsbt, pkg.AnchorPsbt = raw, raw
			pkg, err = pkg.Seal()
			require.NoError(t, err)

			wallet := signingWallet(t, 2)
			round, _, err := newTestSession(t.Context(), wallet, pkg)
			require.NoError(t, err)
			require.Equal(t, byte(2),
				round.CombinedKey.SerializeCompressed()[0])
			require.NoError(t, wallet.MuSig2Cleanup(t.Context(), round.SessionID))

			wallet.wrongKey = true
			_, _, err = newTestSession(t.Context(), wallet, pkg)
			require.Error(t, err)
		})
	}
}

// newTestSession uses the same ordinary helper as Bitcoin/asset FSM actions.
func newTestSession(ctx context.Context, signer lndclient.SignerClient,
	pkg *sdk.CustomAnchorTransferPackage) (*input.MuSig2SessionInfo,
	sdk.CustomAnchorMuSig2SigningRequest, error) {

	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.MuSig2) != 1 {
		return nil, sdk.CustomAnchorMuSig2SigningRequest{}, ErrSigningRequest
	}
	request := requests.MuSig2[0]
	participants := make([][]byte, len(request.Participants))
	for i := range participants {
		participants[i] = request.Participants[i][:]
	}
	session, err := script.CreateMuSig2Session(ctx, signer, keychain.KeyLocator{},
		participants, request.TaprootMerkleRoot)
	return session, request, err
}

// signTestPackage supplies native LND signing fixtures, not a production
// session wrapper. Both session objects are discarded on any failed RPC.
func signTestPackage(ctx context.Context, client, server *sessionWallet,
	pkg *sdk.CustomAnchorTransferPackage) (*sdk.CustomAnchorTransferPackage, error) {

	c, request, err := newTestSession(ctx, client, pkg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.MuSig2Cleanup(ctx, c.SessionID) }()
	s, _, err := newTestSession(ctx, server, pkg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = server.MuSig2Cleanup(ctx, s.SessionID) }()
	if _, err := server.MuSig2RegisterNonces(ctx, s.SessionID, [][66]byte{c.PublicNonce}); err != nil {
		return nil, err
	}
	partial, err := server.MuSig2Sign(ctx, s.SessionID, request.Sighash, true)
	if err != nil {
		return nil, err
	}
	if _, err := client.MuSig2RegisterNonces(ctx, c.SessionID, [][66]byte{s.PublicNonce}); err != nil {
		return nil, err
	}
	if _, err := client.MuSig2Sign(ctx, c.SessionID, request.Sighash, false); err != nil {
		return nil, err
	}
	all, signature, err := client.MuSig2CombineSig(ctx, c.SessionID, [][]byte{partial})
	if err != nil {
		return nil, err
	}
	if !all {
		return nil, ErrSigningRequest
	}
	return pkg.ApplyKeyPathSignature(request.ID, signature)
}

func TestFreshLNDSessionAfterLostReplies(t *testing.T) {
	pkg := signingPackage(t)
	for _, failure := range []string{"server partial", "client partial", "combine"} {
		t.Run(failure, func(t *testing.T) {
			client, server := signingWallet(t, 2), signingWallet(t, 3)
			server.loseSign = failure == "server partial"
			client.loseSign = failure == "client partial"
			client.loseCombine = failure == "combine"
			_, err := signTestPackage(t.Context(), client, server, pkg)
			require.ErrorIs(t, err, errLostSigningReply)
			// Recreated normal LND session managers generate fresh secret nonces;
			// the persisted unsigned SDK package still fixes the authorization.
			signed, err := signTestPackage(t.Context(), signingWallet(t, 2), signingWallet(t, 3), pkg)
			require.NoError(t, err)
			require.NoError(t, pkg.VerifyFinalAnchorPSBT(finishSigningFixture(t, signed)))
		})
	}
}

// finishSigningFixture finalizes all inputs together. The remaining unrelated
// script input is OP_TRUE; the caller, rather than the signer, owns witnesses.
func finishSigningFixture(t *testing.T,
	pkg *sdk.CustomAnchorTransferPackage) []byte {

	t.Helper()
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	request := requests.ScriptPath[0]
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(pkg.AnchorPsbt), false)
	require.NoError(t, err)
	var witness bytes.Buffer
	require.NoError(t, psbt.WriteTxWitness(&witness, [][]byte{
		request.LeafScript, request.ControlBlock,
	}))
	packet.Inputs[request.InputIndex].FinalScriptWitness = witness.Bytes()
	require.NoError(t, psbt.MaybeFinalizeAll(packet))
	var raw bytes.Buffer
	require.NoError(t, packet.Serialize(&raw))
	return raw.Bytes()
}
