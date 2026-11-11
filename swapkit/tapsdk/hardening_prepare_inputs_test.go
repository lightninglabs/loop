package tapsdk

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// declaringClient records what PrepareInputs registers with tapd. The
// proof archive already holds the input proof, so no import is attempted.
type declaringClient struct {
	sdk.Client

	raw      []byte
	declared []sdk.ScriptKey
}

func (c *declaringClient) GetInfo(context.Context) (*sdk.Info, error) {
	return fixtureNodeInfo(), nil
}

func (c *declaringClient) DeclareScriptKey(_ context.Context,
	request *sdk.DeclareScriptKeyRequest) (*sdk.ScriptKey, error) {

	c.declared = append(c.declared, request.ScriptKey)
	return &request.ScriptKey, nil
}

func (c *declaringClient) ExportProof(context.Context, sdk.AssetRef,
	sdk.PubKey, *sdk.Outpoint) (*sdk.ProofFile, error) {

	return &sdk.ProofFile{RawProofFile: bytes.Clone(c.raw)}, nil
}

// TestPrepareInputsRejectsUnboundDescriptor checks that PrepareInputs verifies
// a caller-provided asset witness before registering it in tapd's script key
// registry. A control block that names an unrelated internal key does not
// commit to the input's script key, so declaring it would bind the script key
// to a wallet key the owner never approved. The honest witness still registers.
func TestPrepareInputsRejectsUnboundDescriptor(t *testing.T) {
	f := newPathFixture(t)

	// The honest stack registers the exact key derivation.
	honest := &declaringClient{raw: f.request.Inputs[0].ProofFile}
	require.NoError(t, PrepareInputs(t.Context(),
		sdk.NewWallet(honest, sdk.NetworkRegtest), f.request))
	require.Len(t, honest.declared, 1)
	require.True(t, descriptorDerives(t, honest.declared[0]))

	// The peer keeps the agreed proof and only replaces the witness with a
	// control block for its own internal key and no Merkle path.
	attacker := bindingKey(7).PubKey()
	control := append([]byte{byte(txscript.BaseLeafVersion)},
		schnorr.SerializePubKey(attacker)...)
	request := f.request.Clone()
	request.Inputs[0].Witness.Stack = [][]byte{
		{txscript.OP_TRUE}, control,
	}

	// PrepareInputs rejects the witness before registering anything.
	client := &declaringClient{raw: request.Inputs[0].ProofFile}
	err := PrepareInputs(t.Context(),
		sdk.NewWallet(client, sdk.NetworkRegtest), request)
	require.Error(t, err)
	require.Empty(t, client.declared)
}

// descriptorDerives reports whether the declared raw key and tweak
// produce the declared asset script key.
func descriptorDerives(t *testing.T, key sdk.ScriptKey) bool {
	t.Helper()
	raw, err := btcec.ParsePubKey(key.KeyDesc.RawKeyBytes[:])
	require.NoError(t, err)
	output := txscript.ComputeTaprootOutputKey(raw, key.TapTweak)
	return bytes.Equal(schnorr.SerializePubKey(output), key.PubKey[1:])
}
