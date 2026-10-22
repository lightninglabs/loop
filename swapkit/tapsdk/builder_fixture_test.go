package tapsdk

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// builderFixtureJSON is the SDK's deterministic minted-proof and caller-funded
// anchor fixture, exported through public request and verification types. This
// freezes input bytes while the real builder derives each new kit commitment.
//
//go:embed testdata/builder_request.json
var builderFixtureJSON []byte

type builderFixtureClient struct {
	sdk.Client

	raw         []byte
	verified    *sdk.VerifyProofResponse
	verifyCalls int
	afterVerify func()
}

func (c *builderFixtureClient) VerifyProof(_ context.Context, raw []byte) (
	*sdk.VerifyProofResponse, error) {

	if !bytes.Equal(c.raw, raw) {
		return nil, fmt.Errorf("unexpected proof fixture")
	}
	c.verifyCalls++
	if c.afterVerify != nil {
		c.afterVerify()
	}
	return c.verified, nil
}

func (c *builderFixtureClient) QueryScriptKey(_ context.Context,
	key []byte) (*sdk.ScriptKey, error) {

	// The frozen SDK proof fixture is minted to the scalar-one BIP86 key.
	private, _ := btcec.PrivKeyFromBytes([]byte{1})
	output, err := sdk.ParsePubKey(txscript.ComputeTaprootKeyNoScript(
		private.PubKey(),
	).SerializeCompressed())
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(key, output[1:]) {
		return nil, fmt.Errorf("unexpected script key fixture")
	}
	raw, err := sdk.ParsePubKey(private.PubKey().SerializeCompressed())
	if err != nil {
		return nil, err
	}
	return &sdk.ScriptKey{
		PubKey: output, KeyDesc: sdk.KeyDescriptor{RawKeyBytes: raw},
	}, nil
}

type builderFixture struct {
	request *sdk.CustomAnchorRequest
	client  *builderFixtureClient
}

func newCustomAnchorBuilderFixture(t *testing.T) *builderFixture {
	t.Helper()
	//nolint:tagliatelle // Retain the SDK fixture's serialized field names.
	var fixture struct {
		Request  *sdk.CustomAnchorRequest `json:"Request"`
		Verified *sdk.VerifyProofResponse `json:"Verified"`
	}
	require.NoError(t, json.Unmarshal(builderFixtureJSON, &fixture))
	require.NotNil(t, fixture.Request)
	require.Len(t, fixture.Request.Inputs, 1)
	return &builderFixture{
		request: fixture.Request,
		client: &builderFixtureClient{
			raw:      fixture.Request.Inputs[0].ProofFile,
			verified: fixture.Verified,
		},
	}
}

func testKey(t *testing.T, scalar byte) sdk.PubKey {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	pub, err := sdk.ParsePubKey(key.PubKey().SerializeCompressed())
	require.NoError(t, err)
	return pub
}

func validateMuSig2Aggregate(expected sdk.XOnlyPubKey,
	participants []sdk.PubKey) error {

	keys := make([]*btcec.PublicKey, len(participants))
	for i, key := range participants {
		parsed, err := btcec.ParsePubKey(key[:])
		if err != nil {
			return err
		}
		keys[i] = parsed
	}
	aggregate, _, _, err := musig2.AggregateKeys(keys, false)
	if err != nil {
		return err
	}
	actual := aggregate.FinalKey.SerializeCompressed()[1:]
	if !bytes.Equal(expected[:], actual) {
		return fmt.Errorf("aggregate key mismatch")
	}
	return nil
}

func (c *builderFixtureClient) GetInfo(context.Context) (*sdk.Info, error) {
	return fixtureNodeInfo(), nil
}
