package test

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

type mockSigner struct {
	lnd *LndMockServices
}

func (s *mockSigner) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*input.SignDescriptor) ([][]byte, error) {

	rawSigs := [][]byte{{1, 2, 3}}

	return rawSigs, nil
}

func (s *mockSigner) SignMessage(ctx context.Context, msg []byte,
	locator keychain.KeyLocator) ([]byte, error) {

	return s.lnd.Signature, nil
}
