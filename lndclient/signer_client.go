package lndclient

import (
	"context"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"google.golang.org/grpc"
)

// SignerClient exposes sign functionality.
type SignerClient interface {
	SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
		signDescriptors []*input.SignDescriptor) ([][]byte, error)

	// ComputeInputScript generates the proper input script for P2WPKH
	// output and NP2WPKH outputs. This method only requires that the
	// `Output`, `HashType`, `SigHashes` and `InputIndex` fields are
	// populated within the sign descriptors.
	ComputeInputScript(ctx context.Context, tx *wire.MsgTx,
		signDescriptors []*input.SignDescriptor) ([]*input.Script, error)

	// SignMessage signs a message with the key specified in the key
	// locator. The returned signature is fixed-size LN wire format encoded.
	SignMessage(ctx context.Context, msg []byte,
		locator keychain.KeyLocator) ([]byte, error)

	// VerifyMessage verifies a signature over a message using the public
	// key provided. The signature must be fixed-size LN wire format
	// encoded.
	VerifyMessage(ctx context.Context, msg, sig []byte, pubkey [33]byte) (
		bool, error)

	// DeriveSharedKey returns a shared secret key by performing
	// Diffie-Hellman key derivation between the ephemeral public key and
	// the key specified by the key locator (or the node's identity private
	// key if no key locator is specified):
	//
	//     P_shared = privKeyNode * ephemeralPubkey
	//
	// The resulting shared public key is serialized in the compressed
	// format and hashed with SHA256, resulting in a final key length of 256
	// bits.
	DeriveSharedKey(ctx context.Context, ephemeralPubKey *btcec.PublicKey,
		keyLocator *keychain.KeyLocator) ([32]byte, error)
}

type signerClient struct {
	client    signrpc.SignerClient
	signerMac serializedMacaroon
}

func newSignerClient(conn *grpc.ClientConn,
	signerMac serializedMacaroon) *signerClient {

	return &signerClient{
		client:    signrpc.NewSignerClient(conn),
		signerMac: signerMac,
	}
}

func marshallSignDescriptors(signDescriptors []*input.SignDescriptor,
) []*signrpc.SignDescriptor {

	rpcSignDescs := make([]*signrpc.SignDescriptor, len(signDescriptors))
	for i, signDesc := range signDescriptors {
		var keyBytes []byte
		var keyLocator *signrpc.KeyLocator
		if signDesc.KeyDesc.PubKey != nil {
			keyBytes = signDesc.KeyDesc.PubKey.SerializeCompressed()
		} else {
			keyLocator = &signrpc.KeyLocator{
				KeyFamily: int32(
					signDesc.KeyDesc.KeyLocator.Family,
				),
				KeyIndex: int32(
					signDesc.KeyDesc.KeyLocator.Index,
				),
			}
		}

		var doubleTweak []byte
		if signDesc.DoubleTweak != nil {
			doubleTweak = signDesc.DoubleTweak.Serialize()
		}

		rpcSignDescs[i] = &signrpc.SignDescriptor{
			WitnessScript: signDesc.WitnessScript,
			Output: &signrpc.TxOut{
				PkScript: signDesc.Output.PkScript,
				Value:    signDesc.Output.Value,
			},
			Sighash:    uint32(signDesc.HashType),
			InputIndex: int32(signDesc.InputIndex),
			KeyDesc: &signrpc.KeyDescriptor{
				RawKeyBytes: keyBytes,
				KeyLoc:      keyLocator,
			},
			SingleTweak: signDesc.SingleTweak,
			DoubleTweak: doubleTweak,
		}
	}

	return rpcSignDescs
}

func (s *signerClient) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*input.SignDescriptor) ([][]byte, error) {

	txRaw, err := swap.EncodeTx(tx)
	if err != nil {
		return nil, err
	}
	rpcSignDescs := marshallSignDescriptors(signDescriptors)

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.signerMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.SignOutputRaw(rpcCtx,
		&signrpc.SignReq{
			RawTxBytes: txRaw,
			SignDescs:  rpcSignDescs,
		},
	)
	if err != nil {
		return nil, err
	}

	return resp.RawSigs, nil
}

// ComputeInputScript generates the proper input script for P2WPKH output and
// NP2WPKH outputs. This method only requires that the `Output`, `HashType`,
// `SigHashes` and `InputIndex` fields are populated within the sign
// descriptors.
func (s *signerClient) ComputeInputScript(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*input.SignDescriptor) ([]*input.Script, error) {

	txRaw, err := swap.EncodeTx(tx)
	if err != nil {
		return nil, err
	}
	rpcSignDescs := marshallSignDescriptors(signDescriptors)

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.signerMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.ComputeInputScript(
		rpcCtx, &signrpc.SignReq{
			RawTxBytes: txRaw,
			SignDescs:  rpcSignDescs,
		},
	)
	if err != nil {
		return nil, err
	}

	inputScripts := make([]*input.Script, 0, len(resp.InputScripts))
	for _, inputScript := range resp.InputScripts {
		inputScripts = append(inputScripts, &input.Script{
			SigScript: inputScript.SigScript,
			Witness:   inputScript.Witness,
		})
	}

	return inputScripts, nil
}

// SignMessage signs a message with the key specified in the key locator. The
// returned signature is fixed-size LN wire format encoded.
func (s *signerClient) SignMessage(ctx context.Context, msg []byte,
	locator keychain.KeyLocator) ([]byte, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &signrpc.SignMessageReq{
		Msg: msg,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(locator.Family),
			KeyIndex:  int32(locator.Index),
		},
	}

	rpcCtx = s.signerMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.SignMessage(rpcCtx, rpcIn)
	if err != nil {
		return nil, err
	}

	return resp.Signature, nil
}

// VerifyMessage verifies a signature over a message using the public key
// provided. The signature must be fixed-size LN wire format encoded.
func (s *signerClient) VerifyMessage(ctx context.Context, msg, sig []byte,
	pubkey [33]byte) (bool, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &signrpc.VerifyMessageReq{
		Msg:       msg,
		Signature: sig,
		Pubkey:    pubkey[:],
	}

	rpcCtx = s.signerMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.VerifyMessage(rpcCtx, rpcIn)
	if err != nil {
		return false, err
	}
	return resp.Valid, nil
}

// DeriveSharedKey returns a shared secret key by performing Diffie-Hellman key
// derivation between the ephemeral public key and the key specified by the key
// locator (or the node's identity private key if no key locator is specified):
//
//     P_shared = privKeyNode * ephemeralPubkey
//
// The resulting shared public key is serialized in the compressed format and
// hashed with SHA256, resulting in a final key length of 256 bits.
func (s *signerClient) DeriveSharedKey(ctx context.Context,
	ephemeralPubKey *btcec.PublicKey,
	keyLocator *keychain.KeyLocator) ([32]byte, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubKey.SerializeCompressed(),
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keyLocator.Family),
			KeyIndex:  int32(keyLocator.Index),
		},
	}

	rpcCtx = s.signerMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.DeriveSharedKey(rpcCtx, rpcIn)
	if err != nil {
		return [32]byte{}, err
	}

	var sharedKey [32]byte
	copy(sharedKey[:], resp.SharedKey)
	return sharedKey, nil
}
