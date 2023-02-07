package test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

type mockSigner struct {
	lnd *LndMockServices
}

func (s *mockSigner) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor,
	_ []*wire.TxOut) ([][]byte, error) {

	s.lnd.SignOutputRawChannel <- SignOutputRawRequest{
		Tx:              tx,
		SignDescriptors: signDescriptors,
	}

	rawSigs := [][]byte{{1, 2, 3}}

	return rawSigs, nil
}

func (s *mockSigner) ComputeInputScript(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([]*input.Script, error) {

	return nil, fmt.Errorf("unimplemented")
}

func (s *mockSigner) SignMessage(ctx context.Context, msg []byte,
	locator keychain.KeyLocator, opts ...lndclient.SignMessageOption) (
	[]byte, error) {

	return s.lnd.Signature, nil
}

func (s *mockSigner) VerifyMessage(ctx context.Context, msg, sig []byte,
	pubkey [33]byte, opts ...lndclient.VerifyMessageOption) (bool, error) {

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

// MuSig2CreateSession creates a new MuSig2 signing session using the local
// key identified by the key locator. The complete list of all public keys of
// all signing parties must be provided, including the public key of the local
// signing key. If nonces of other parties are already known, they can be
// submitted as well to reduce the number of method calls necessary later on.
func (s *mockSigner) MuSig2CreateSession(context.Context, input.MuSig2Version,
	*keychain.KeyLocator, [][]byte, ...lndclient.MuSig2SessionOpts) (
	*input.MuSig2SessionInfo, error) {

	const testPubKey = "F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BCE036F9"
	pubKeyBytes, err := hex.DecodeString(testPubKey)
	if err != nil {
		return nil, err
	}

	combinedKey, err := schnorr.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, err
	}

	return &input.MuSig2SessionInfo{
		CombinedKey:   combinedKey,
		HaveAllNonces: true,
	}, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID. This method returns true
// once we have all nonces for all other signing participants.
func (s *mockSigner) MuSig2RegisterNonces(context.Context, [32]byte,
	[][66]byte) (bool, error) {

	return true, nil
}

// MuSig2Sign creates a partial signature using the local signing key
// that was specified when the session was created. This can only be
// called when all public nonces of all participants are known and have
// been registered with the session. If this node isn't responsible for
// combining all the partial signatures, then the cleanup parameter
// should be set, indicating that the session can be removed from memory
// once the signature was produced.
func (s *mockSigner) MuSig2Sign(context.Context, [32]byte, [32]byte,
	bool) ([]byte, error) {

	return nil, nil
}

// MuSig2CombineSig combines the given partial signature(s) with the
// local one, if it already exists. Once a partial signature of all
// participants is registered, the final signature will be combined and
// returned.
func (s *mockSigner) MuSig2CombineSig(context.Context, [32]byte,
	[][]byte) (bool, []byte, error) {

	return true, nil, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (s *mockSigner) MuSig2Cleanup(context.Context, [32]byte) error {
	return nil
}
