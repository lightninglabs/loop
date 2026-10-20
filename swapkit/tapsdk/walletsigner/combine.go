package walletsigner

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
)

// PeerPartial contains the partial for a peer already bound by Sign.
type PeerPartial struct {
	PublicKey sdk.PubKey
	Signature []byte
}

// Combine verifies every peer partial against the persisted nonce binding,
// combines the already durable local partial, and verifies the final Schnorr
// signature. It needs no live LND session and is safe to replay after restart.
// The result includes a sighash suffix only when the SDK requests one.
func (s *Signer) Combine(ctx context.Context, id [32]byte,
	peers []PeerPartial) ([]byte, error) {

	r, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(r.Signature) != 32 || len(peers) != len(r.Peers) {
		return nil, ErrInvalid
	}
	request, keys, err := r.request()
	if err != nil {
		return nil, err
	}
	all := [][musig2.PubNonceSize]byte{r.Public}
	for _, peer := range r.Peers {
		all = append(all, peer.Nonce)
	}
	combined, err := musig2.AggregateNonces(all)
	if err != nil {
		return nil, err
	}
	local := new(musig2.PartialSignature)
	if err := local.Decode(bytes.NewReader(r.Signature)); err != nil {
		return nil, ErrInvalid
	}
	partials := []*musig2.PartialSignature{local}
	option := musig2.WithTaprootSignTweak(request.TaprootMerkleRoot)
	combineOption := musig2.WithTaprootTweakedCombine(
		request.Sighash, keys, request.TaprootMerkleRoot, false,
	)
	if len(request.TaprootMerkleRoot) == 0 {
		option = musig2.WithBip86SignTweak()
		combineOption = musig2.WithBip86TweakedCombine(
			request.Sighash, keys, false,
		)
	}
	for i, peer := range peers {
		if peer.PublicKey != r.Peers[i].PublicKey ||
			len(peer.Signature) != 32 {

			return nil, ErrBinding
		}
		partial := new(musig2.PartialSignature)
		if err := partial.Decode(
			bytes.NewReader(peer.Signature),
		); err != nil {
			return nil, ErrInvalid
		}
		key, err := btcec.ParsePubKey(peer.PublicKey[:])
		if err != nil || !partial.Verify(r.Peers[i].Nonce, combined,
			keys, key, request.Sighash, option) {

			return nil, ErrInvalid
		}
		partials = append(partials, partial)
	}
	internal, err := schnorr.ParsePubKey(request.InternalKey[:])
	if err != nil {
		return nil, err
	}
	output := txscript.ComputeTaprootOutputKey(
		internal, request.TaprootMerkleRoot,
	)
	nonce, err := signingNonce(combined, output, request.Sighash)
	if err != nil {
		return nil, err
	}
	signature := musig2.CombineSigs(nonce, partials, combineOption)
	if !signature.Verify(request.Sighash[:], output) {
		return nil, ErrInvalid
	}
	return withSighash(signature.Serialize(), request.SighashType), nil
}

// signingNonce implements BIP327's public R = R1 + b*R2 calculation. The
// library exposes nonce aggregation and signature combination separately;
// restoring R requires only public data, never a private signing key.
func signingNonce(combined [musig2.PubNonceSize]byte,
	output *btcec.PublicKey, message sdk.Hash) (*btcec.PublicKey, error) {

	hash := chainhash.TaggedHash(musig2.NonceBlindTag, combined[:],
		schnorr.SerializePubKey(output), message[:])
	var coefficient btcec.ModNScalar
	coefficient.SetByteSlice(hash[:])
	first, err := btcec.ParseJacobian(combined[:33])
	if err != nil {
		return nil, err
	}
	second, err := btcec.ParseJacobian(combined[33:])
	if err != nil {
		return nil, err
	}
	var result btcec.JacobianPoint
	btcec.ScalarMultNonConst(&coefficient, &second, &second)
	btcec.AddNonConst(&first, &second, &result)
	if result.Z.IsZero() {
		return btcec.Generator(), nil
	}
	result.ToAffine()
	return btcec.NewPublicKey(&result.X, &result.Y), nil
}

func withSighash(signature []byte, sighash uint32) []byte {
	if sighash != uint32(txscript.SigHashDefault) {
		return append(signature, byte(sighash))
	}
	return signature
}
