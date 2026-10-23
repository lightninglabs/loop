package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

type fundingSDK struct {
	*bindingSDK

	rows    []*sdk.AssetRecord
	proofs  map[sdk.Outpoint][]byte
	exports []sdk.Outpoint
}

func (s *fundingSDK) ListAssetRecords(context.Context,
	*sdk.ListAssetsRequest) ([]*sdk.AssetRecord, error) {

	return s.rows, nil
}

func (s *fundingSDK) ExportProof(_ context.Context, _ sdk.AssetRef,
	_ sdk.PubKey, point *sdk.Outpoint) (*sdk.ProofFile, error) {

	if point == nil {
		return nil, proof.ErrMultipleProofs
	}
	s.exports = append(s.exports, *point)
	return &sdk.ProofFile{RawProofFile: s.proofs[*point]}, nil
}

type fundingChain struct {
	*bindingChain

	spent wire.OutPoint
}

func (c *fundingChain) Unspent(_ context.Context,
	point wire.OutPoint) (bool, error) {

	return point != c.spent, nil
}

func TestFundingUsesExactOutpointWithReusedScriptKey(t *testing.T) {
	first := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	second := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil,
		wire.OutPoint{Hash: chainhash.Hash{12}}, 101, false)
	client := &fundingSDK{bindingSDK: &bindingSDK{valid: true},
		proofs: make(map[sdk.Outpoint][]byte)}
	for _, p := range []*proof.Proof{first, second} {
		point := sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash),
			Index: p.OutPoint().Index}
		client.rows = append(client.rows, &sdk.AssetRecord{
			Amount: 1000, AnchorOutpoint: &point,
			Genesis: sdk.IssuanceGenesis{
				IssuanceID: sdk.AssetID(p.Asset.ID()),
			},
			ScriptKey: sdk.ScriptKey{PubKey: bindingPub(2)},
		})
		client.proofs[point] = bindingRaw(t, p)
	}
	chain := &fundingChain{spent: first.OutPoint(),
		bindingChain: &bindingChain{tip: 102,
			blocks: map[uint32]*wire.MsgBlock{
				100: bindingBlock(first),
				101: bindingBlock(second),
			}}}
	r := &proofNode{client: client, chain: chain, minConfs: 1}
	request := swapkit.CreateDepositRequest{
		GroupKey: bindingPub(11), Amount: 1000,
	}
	raw, err := FundingProof(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		request,
	)
	require.NoError(t, err)
	require.Equal(t, bindingRaw(t, second), raw)
	require.Len(t, client.exports, 2)
	require.Equal(t, *client.rows[0].AnchorOutpoint, client.exports[0])
	require.Equal(t, *client.rows[1].AnchorOutpoint, client.exports[1])

	// Even a valid same-group proof cannot replace the selected output.
	client.rows = client.rows[1:]
	client.proofs[*client.rows[0].AnchorOutpoint] = bindingRaw(t, first)
	chain.spent = wire.OutPoint{}
	_, err = FundingProof(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		request,
	)
	require.ErrorIs(t, err, swapkit.ErrInvalid)

	client.rows[0].AnchorOutpoint = nil
	client.exports = nil
	_, err = FundingProof(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		request,
	)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	require.Empty(t, client.exports)
}
