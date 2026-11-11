package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// bindingSDK models successful authenticated tapd proof verification. These
// tests exercise the Loop contract's additional policy and current-chain checks,
// which must reject even a cryptographically valid proof for another contract.
type bindingSDK struct {
	sdk.Client

	valid bool
}

func (s *bindingSDK) VerifyProof(_ context.Context,
	raw []byte) (*sdk.VerifyProofResponse, error) {

	return fixtureVerified(raw, s.valid)
}

type bindingChain struct {
	Chain

	tip     uint32
	blocks  map[uint32]*wire.MsgBlock
	unspent bool
}

func (c *bindingChain) Height(context.Context) (uint32, error) {
	return c.tip, nil
}

func (c *bindingChain) Block(_ context.Context,
	height uint32) (*wire.MsgBlock, error) {

	block := c.blocks[height]
	if block == nil {
		return nil, swapkit.ErrPending
	}
	return block, nil
}

func (c *bindingChain) BlockHash(ctx context.Context,
	height uint32) (chainhash.Hash, error) {

	block, err := c.Block(ctx, height)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return block.BlockHash(), nil
}

func (c *bindingChain) Unspent(context.Context,
	wire.OutPoint) (bool, error) {

	return c.unspent, nil
}

func bindingKey(scalar byte) *btcec.PrivateKey {
	key, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	return key
}

func bindingPub(scalar byte) [33]byte {
	var key [33]byte
	copy(key[:], bindingKey(scalar).PubKey().SerializeCompressed())
	return key
}

// bindingProof constructs a real isolated Taproot commitment; an optional
// second asset deliberately makes reconstruction from the disclosed one fail.
func bindingProof(t *testing.T, genesis asset.Genesis, script,
	internal *btcec.PublicKey, sibling *commitment.TapscriptPreimage,
	previous wire.OutPoint, height uint32, hidden bool) *proof.Proof {

	t.Helper()
	group := &asset.GroupKey{GroupPubKey: *bindingKey(11).PubKey()}
	a, err := asset.New(genesis, 1000, 0, 0,
		asset.NewScriptKey(script), group,
		asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)
	assets := []*asset.Asset{a}
	if hidden {
		otherGenesis := genesis
		otherGenesis.Tag += "-hidden"
		other, err := asset.New(otherGenesis, 1, 0, 0,
			asset.NewScriptKey(bindingKey(9).PubKey()), nil,
			asset.WithAssetVersion(asset.V1))
		require.NoError(t, err)
		assets = append(assets, other)
	}
	version := commitment.TapCommitmentV2
	tree, err := commitment.FromAssets(&version, assets...)
	require.NoError(t, err)
	_, inclusion, err := tree.Proof(a.TapCommitmentKey(),
		a.AssetCommitmentKey())
	require.NoError(t, err)
	pkScript, _, _, err := tapsend.AnchorOutputScript(
		internal, sibling, tree,
	)
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&previous, nil, nil))
	tx.AddTxOut(wire.NewTxOut(10_000, pkScript))
	return &proof.Proof{
		PrevOut: previous, AnchorTx: *tx, Asset: *a,
		BlockHeight: height,
		BlockHeader: wire.BlockHeader{MerkleRoot: tx.TxHash()},
		InclusionProof: proof.TaprootProof{
			InternalKey: internal, OutputIndex: 0,
			CommitmentProof: &proof.CommitmentProof{
				Proof: *inclusion, TapSiblingPreimage: sibling,
			},
		},
		GenesisReveal: &genesis,
	}
}

func bindingGenesis() asset.Genesis {
	return asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{7}},
		Tag:          "binding-test", Type: asset.Normal,
	}
}

func bindingRaw(t *testing.T, p *proof.Proof) []byte {
	t.Helper()
	raw, err := proof.EncodeAsProofFile(p)
	require.NoError(t, err)
	return raw
}

func bindingBlock(p *proof.Proof) *wire.MsgBlock {
	return &wire.MsgBlock{
		Header: p.BlockHeader, Transactions: []*wire.MsgTx{&p.AnchorTx},
	}
}

// proofNode holds only test services. Tests call production primitives directly.
type proofNode struct {
	client   sdk.Client
	chain    Chain
	minConfs uint32
}

func (n *proofNode) wallet() *sdk.Wallet {
	return sdk.NewWallet(n.client, sdk.NetworkRegtest)
}

func (s *bindingSDK) GetInfo(
	context.Context,
) (*sdk.Info, error) {

	return fixtureNodeInfo(), nil
}

func fixtureNodeInfo() *sdk.Info { return &sdk.Info{Version: "0.8.8", Network: "regtest"} }

func fixtureVerified(raw []byte, valid bool) (*sdk.VerifyProofResponse, error) {
	p, err := LastProof(raw)
	if err != nil {
		return nil, err
	}
	key, err := sdk.ParsePubKey(
		p.Asset.ScriptKey.PubKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}
	var group *sdk.PubKey
	if p.Asset.GroupKey != nil {
		k, err := sdk.ParsePubKey(
			p.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, err
		}
		group = &k
	}
	return &sdk.VerifyProofResponse{
		Valid: valid,
		DecodedProof: &sdk.DecodedProof{
			AssetRef: sdk.AssetRefFromTypedAsset(
				sdk.AssetID(p.Asset.ID()),
				group,
				sdk.AssetType(p.Asset.Type),
			),
			IssuanceID: sdk.AssetID(p.Asset.ID()),
			Amount:     p.Asset.Amount, ScriptKey: key,
			Outpoint: sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash), Index: p.OutPoint().Index},
		},
	}, nil
}
