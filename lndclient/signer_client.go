package lndclient

import (
	"context"

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

	// SignMessage signs a message with the key specified in the key
	// locator. The returned signature is fixed-size LN wire format encoded.
	SignMessage(ctx context.Context, msg []byte,
		locator keychain.KeyLocator) ([]byte, error)
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

func (s *signerClient) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*input.SignDescriptor) ([][]byte, error) {

	txRaw, err := swap.EncodeTx(tx)
	if err != nil {
		return nil, err
	}

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
