package test

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

type mockSigner struct {
	lnd *LndMockServices
}

func (s *mockSigner) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor) ([][]byte, error) {

	s.lnd.SignOutputRawChannel <- SignOutputRawRequest{
		Tx:              tx,
		SignDescriptors: signDescriptors,
	}

	rawSigs := [][]byte{{1, 2, 3}}

	return rawSigs, nil
}

func (s *mockSigner) ComputeInputScript(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor) ([]*input.Script, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *mockSigner) SignMessage(ctx context.Context, msg []byte,
	locator keychain.KeyLocator) ([]byte, error) {

	return s.lnd.Signature, nil
}

func (s *mockSigner) VerifyMessage(ctx context.Context, msg, sig []byte,
	pubkey [33]byte) (bool, error) {

	// Make the mock somewhat functional by asserting that the message and
	// signature is what we expect from the mock parameters.
	mockAssertion := bytes.Equal(msg, []byte(s.lnd.SignatureMsg)) &&
		bytes.Equal(sig, s.lnd.Signature)

	return mockAssertion, nil
}

func (s *mockSigner) DeriveSharedKey(context.Context, *btcec.PublicKey,
	*keychain.KeyLocator) ([32]byte, error) {

	return [32]byte{4, 5, 6}, nil
}
