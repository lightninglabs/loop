// Package walletsigner signs SDK anchor transactions with wallet-owned keys.
// Its private journal binds each MuSig2 nonce to one complete signing request
// before disclosure, and to one peer nonce set before any signing RPC.
package walletsigner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"go.etcd.io/bbolt"
)

const muSigVersion = signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2

var (
	// ErrBinding means an existing round was requested with different data.
	ErrBinding = errors.New("signing round binding mismatch")
	// ErrInvalid means signing data or a wallet response failed validation.
	ErrInvalid = errors.New("invalid wallet signing data")
	// ErrNotFound means the caller has not durably allocated this round.
	ErrNotFound  = errors.New("signing round not found")
	roundsBucket = []byte("asset-wallet-signing-v1")
)

// PeerNonce identifies a participant's public nonce. Sign requires exactly
// one entry for every participant other than the local wallet key, in the
// participant order committed by the SDK package.
type PeerNonce struct {
	PublicKey sdk.PubKey
	Nonce     [musig2.PubNonceSize]byte
}

// Partial contains public data only. Repeated Sign calls return identical
// bytes, including after either the application or LND has restarted.
type Partial struct {
	Nonce     [musig2.PubNonceSize]byte
	Signature []byte
}

// Signer owns an exclusive, synchronously flushed private nonce journal.
// Callers must authorize the complete SDK package before calling Nonce; this
// component enforces immutable signing, not swap admission or payout policy.
// One running Signer must own a journal. Join its callers before Close.
type Signer struct {
	db     *bbolt.DB
	rpc    signrpc.SignerClient
	wallet walletrpc.WalletKitClient
	locks  [64]chan struct{}
}

type round struct {
	Version   uint32
	ID        [32]byte
	Key       keychain.KeyLocator
	PublicKey sdk.PubKey
	Package   []byte
	RequestID sdk.Hash
	Secret    [musig2.SecNonceSize]byte
	Public    [musig2.PubNonceSize]byte
	Peers     []PeerNonce
	Signature []byte
}

// Open opens a mode-0600 journal and connects authenticated local wallet RPCs.
// The journal contains secret nonces. Keep it with wallet secrets, and never
// restore an older copy while the same signing key remains in use: rollback
// can erase peer bindings and permit dangerous nonce reuse.
func Open(path string, rpc signrpc.SignerClient,
	wallet walletrpc.WalletKitClient) (*Signer, error) {

	if rpc == nil || wallet == nil {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(path)
	if err == nil && (!info.Mode().IsRegular() ||
		info.Mode().Perm() != 0600) {

		return nil, ErrInvalid
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(roundsBucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Signer{db: db, rpc: rpc, wallet: wallet}
	for i := range s.locks {
		s.locks[i] = make(chan struct{}, 1)
	}
	return s, nil
}

// Close flushes the journal and releases its exclusive process lock.
func (s *Signer) Close() error {
	return s.db.Close()
}

func (s *Signer) lock(ctx context.Context, id [32]byte) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := s.locks[id[0]%64]
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	}
}

// Nonce validates the sealed SDK request and wallet locator, then durably
// allocates a public nonce. The caller supplies a stable, nonzero round ID.
// Every retry must carry the identical package, key locator and request ID.
// No nonce or LND session is exposed until the complete binding is flushed.
func (s *Signer) Nonce(ctx context.Context, id [32]byte,
	key keychain.KeyLocator, pkg *sdk.CustomAnchorTransferPackage,
	requestID sdk.Hash) ([musig2.PubNonceSize]byte, error) {

	var zero [musig2.PubNonceSize]byte
	if id == ([32]byte{}) || key.Family < 0 || key.Index > math.MaxInt32 ||
		pkg == nil {

		return zero, ErrInvalid
	}
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return zero, err
	}
	defer unlock()
	raw, err := pkg.MarshalBinary()
	if err != nil {
		return zero, err
	}
	r, err := s.load(ctx, id)
	if err == nil {
		if r.Key != key || r.RequestID != requestID ||
			!bytes.Equal(r.Package, raw) {

			return zero, ErrBinding
		}
		return r.Public, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return zero, err
	}
	public, err := s.deriveKey(ctx, key)
	if err != nil {
		return zero, err
	}
	r = &round{
		Version: 1, ID: id, Key: key, PublicKey: public,
		Package: raw, RequestID: requestID,
	}
	request, _, err := r.request()
	if err != nil {
		return zero, err
	}
	parsed, err := btcec.ParsePubKey(public[:])
	if err != nil {
		return zero, err
	}
	nonces, err := musig2.GenNonces(
		musig2.WithPublicKey(parsed),
		musig2.WithNonceMessageAux(request.Sighash),
		musig2.WithNonceAuxInput(id[:]),
	)
	if err != nil {
		return zero, err
	}
	r.Secret, r.Public = nonces.SecNonce, nonces.PubNonce
	if err := s.store(ctx, r); err != nil {
		return zero, err
	}
	return r.Public, nil
}

// Sign permanently binds the previously allocated nonce to the exact peer
// nonce set before calling LND. Ambiguous RPC failures can be retried after
// either process restarts: only the identical message and nonces are reused.
// A verified partial is flushed before return and replayed without signing.
func (s *Signer) Sign(ctx context.Context, id [32]byte,
	peers []PeerNonce) (*Partial, error) {

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	request, keys, err := r.request()
	if err != nil {
		return nil, err
	}
	if err := validatePeers(request, r.PublicKey, peers); err != nil {
		return nil, err
	}
	if len(r.Peers) != 0 && !samePeers(r.Peers, peers) {
		return nil, ErrBinding
	}
	if len(r.Signature) != 0 {
		return &Partial{r.Public, bytes.Clone(r.Signature)}, nil
	}
	if len(r.Peers) == 0 {
		r.Peers = append([]PeerNonce(nil), peers...)
		if err := s.store(ctx, r); err != nil {
			return nil, err
		}
	}
	public, err := s.deriveKey(ctx, r.Key)
	if err != nil {
		return nil, err
	}
	if public != r.PublicKey {
		return nil, ErrBinding
	}
	keyBytes := make([][]byte, len(request.Participants))
	for i, key := range request.Participants {
		keyBytes[i] = bytes.Clone(key[:])
	}
	nonces := make([][]byte, len(peers))
	for i := range peers {
		nonces[i] = bytes.Clone(peers[i].Nonce[:])
	}
	keyOnly := len(request.TaprootMerkleRoot) == 0
	session, err := s.rpc.MuSig2CreateSession(ctx,
		&signrpc.MuSig2SessionRequest{
			KeyLoc: rpcLocator(r.Key), AllSignerPubkeys: keyBytes,
			OtherSignerPublicNonces: nonces,
			PregeneratedLocalNonce:  bytes.Clone(r.Secret[:]),
			Version:                 muSigVersion,
			TaprootTweak: &signrpc.TaprootTweakDesc{
				ScriptRoot: bytes.Clone(
					request.TaprootMerkleRoot,
				),
				KeySpendOnly: keyOnly,
			},
		})
	if err != nil {
		return nil, err
	}
	if err := validateSession(r, request, session); err != nil {
		return nil, err
	}
	signed, err := s.rpc.MuSig2Sign(ctx, &signrpc.MuSig2SignRequest{
		SessionId:     bytes.Clone(session.SessionId),
		MessageDigest: bytes.Clone(request.Sighash[:]), Cleanup: true,
	})
	if err != nil {
		return nil, err
	}
	if signed == nil || !verifyPartial(
		r, request, keys, signed.LocalPartialSignature,
	) {
		return nil, ErrInvalid
	}
	r.Signature = bytes.Clone(signed.LocalPartialSignature)
	// This record needs only its public nonce and result after this write.
	clear(r.Secret[:])
	if err := s.store(ctx, r); err != nil {
		return nil, err
	}
	return &Partial{r.Public, bytes.Clone(r.Signature)}, nil
}

func (s *Signer) deriveKey(ctx context.Context,
	key keychain.KeyLocator) (sdk.PubKey, error) {

	response, err := s.wallet.DeriveKey(ctx, rpcLocator(key))
	if err != nil {
		return sdk.PubKey{}, err
	}
	if response == nil || response.KeyLoc == nil ||
		response.KeyLoc.KeyFamily != int32(key.Family) ||
		response.KeyLoc.KeyIndex != int32(key.Index) {

		return sdk.PubKey{}, ErrInvalid
	}
	public, err := sdk.ParsePubKey(response.RawKeyBytes)
	if err != nil || !bytes.Equal(public[:], response.RawKeyBytes) {
		return sdk.PubKey{}, ErrInvalid
	}
	return public, nil
}

func rpcLocator(key keychain.KeyLocator) *signrpc.KeyLocator {
	return &signrpc.KeyLocator{
		KeyFamily: int32(key.Family), KeyIndex: int32(key.Index),
	}
}

func (r *round) request() (*sdk.CustomAnchorMuSig2SigningRequest,
	[]*btcec.PublicKey, error) {

	var pkg sdk.CustomAnchorTransferPackage
	if err := pkg.UnmarshalBinary(r.Package); err != nil {
		return nil, nil, err
	}
	requests, err := pkg.SigningRequests()
	if err != nil {
		return nil, nil, err
	}
	for i := range requests.MuSig2 {
		request := &requests.MuSig2[i]
		if request.ID != r.RequestID {
			continue
		}
		keys := make([]*btcec.PublicKey, len(request.Participants))
		found := false
		for j, public := range request.Participants {
			// LND sorts signers. Reject a different SDK order
			// than signing another aggregate key.
			if j > 0 && bytes.Compare(
				request.Participants[j-1][:], public[:],
			) >= 0 {
				return nil, nil, ErrInvalid
			}
			keys[j], err = btcec.ParsePubKey(public[:])
			if err != nil {
				return nil, nil, ErrInvalid
			}
			found = found || public == r.PublicKey
		}
		if !found {
			return nil, nil, ErrBinding
		}
		return request, keys, nil
	}
	return nil, nil, ErrBinding
}

func validatePeers(request *sdk.CustomAnchorMuSig2SigningRequest,
	local sdk.PubKey, peers []PeerNonce) error {

	if len(peers) != len(request.Participants)-1 {
		return ErrInvalid
	}
	i := 0
	for _, key := range request.Participants {
		if key == local {
			continue
		}
		if peers[i].PublicKey != key {
			return ErrBinding
		}
		for _, raw := range [][]byte{
			peers[i].Nonce[:33], peers[i].Nonce[33:],
		} {
			parsed, err := btcec.ParsePubKey(raw)
			if err != nil ||
				!bytes.Equal(
					raw, parsed.SerializeCompressed(),
				) {

				return ErrInvalid
			}
		}
		i++
	}
	return nil
}

func samePeers(a, b []PeerNonce) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateSession(r *round, request *sdk.CustomAnchorMuSig2SigningRequest,
	response *signrpc.MuSig2SessionResponse) error {

	if response == nil || len(response.SessionId) != 32 ||
		!response.HaveAllNonces ||
		response.Version != muSigVersion ||
		!bytes.Equal(response.LocalPublicNonces, r.Public[:]) ||
		!bytes.Equal(
			response.TaprootInternalKey, request.InternalKey[:],
		) {

		return ErrInvalid
	}
	internal, err := schnorr.ParsePubKey(request.InternalKey[:])
	if err != nil {
		return err
	}
	output := txscript.ComputeTaprootOutputKey(
		internal, request.TaprootMerkleRoot,
	)
	if !bytes.Equal(response.CombinedKey, schnorr.SerializePubKey(output)) {
		return ErrInvalid
	}
	return nil
}

func verifyPartial(r *round, request *sdk.CustomAnchorMuSig2SigningRequest,
	keys []*btcec.PublicKey, signature []byte) bool {

	if len(signature) != 32 {
		return false
	}
	partial := new(musig2.PartialSignature)
	if err := partial.Decode(bytes.NewReader(signature)); err != nil {
		return false
	}
	all := [][musig2.PubNonceSize]byte{r.Public}
	for _, peer := range r.Peers {
		all = append(all, peer.Nonce)
	}
	combined, err := musig2.AggregateNonces(all)
	if err != nil {
		return false
	}
	key, err := btcec.ParsePubKey(r.PublicKey[:])
	if err != nil {
		return false
	}
	option := musig2.WithTaprootSignTweak(request.TaprootMerkleRoot)
	if len(request.TaprootMerkleRoot) == 0 {
		option = musig2.WithBip86SignTweak()
	}
	return partial.Verify(r.Public, combined, keys, key,
		request.Sighash, option)
}

func (s *Signer) load(ctx context.Context, id [32]byte) (*round, error) {
	var r round
	err := s.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := tx.Bucket(roundsBucket).Get(id[:])
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return ErrInvalid
		}
		if r.Version != 1 || r.ID != id {
			return ErrInvalid
		}
		return validateRound(&r)
	})
	return &r, err
}

func (s *Signer) store(ctx context.Context, r *round) error {
	if err := validateRound(r); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return tx.Bucket(roundsBucket).Put(r.ID[:], raw)
	})
}

func validateRound(r *round) error {
	if r.Version != 1 || r.ID == ([32]byte{}) || r.Key.Family < 0 ||
		r.Key.Index > math.MaxInt32 {

		return ErrInvalid
	}
	request, keys, err := r.request()
	if err != nil {
		return fmt.Errorf("validate signing journal: %w", err)
	}
	if len(r.Peers) != 0 {
		err := validatePeers(request, r.PublicKey, r.Peers)
		if err != nil {
			return err
		}
	}
	if len(r.Signature) != 0 {
		if len(r.Peers) == 0 || !verifyPartial(r, request, keys,
			r.Signature) {

			return ErrInvalid
		}
		return nil
	}
	if !bytes.Equal(r.Secret[64:], r.PublicKey[:]) {
		return ErrInvalid
	}
	for i := 0; i < 2; i++ {
		var scalar btcec.ModNScalar
		overflow := scalar.SetByteSlice(r.Secret[i*32 : (i+1)*32])
		if overflow || scalar.IsZero() {
			return ErrInvalid
		}
		var point btcec.JacobianPoint
		btcec.ScalarBaseMultNonConst(&scalar, &point)
		point.ToAffine()
		public := btcec.NewPublicKey(&point.X, &point.Y)
		if !bytes.Equal(public.SerializeCompressed(),
			r.Public[i*33:(i+1)*33]) {

			return ErrInvalid
		}
	}
	return nil
}
