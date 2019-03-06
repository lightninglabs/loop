package test

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
)

type mockSigner struct {
}

func (s *mockSigner) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*input.SignDescriptor) ([][]byte, error) {

	rawSigs := [][]byte{{1, 2, 3}}

	return rawSigs, nil
}
