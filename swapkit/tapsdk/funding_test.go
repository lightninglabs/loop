package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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

	spent      wire.OutPoint
	heightErr  error
	unspentErr error
}

func (c *fundingChain) Height(ctx context.Context) (uint32, error) {
	if c.heightErr != nil {
		return 0, c.heightErr
	}
	return c.bindingChain.Height(ctx)
}

func (c *fundingChain) Unspent(_ context.Context,
	point wire.OutPoint) (bool, error) {

	if c.unspentErr != nil {
		return false, c.unspentErr
	}
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

// TestFundingProofReportsChainState checks that an exact wallet output which
// only waits on the chain is reported as retryable, not as an invalid request.
func TestFundingProofReportsChainState(t *testing.T) {
	first := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	second := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil,
		wire.OutPoint{Hash: chainhash.Hash{12}}, 101, false)
	row := func(p *proof.Proof) (*sdk.AssetRecord, sdk.Outpoint) {
		point := sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash),
			Index: p.OutPoint().Index}
		return &sdk.AssetRecord{
			Amount: 1000, AnchorOutpoint: &point,
			Genesis: sdk.IssuanceGenesis{
				IssuanceID: sdk.AssetID(p.Asset.ID()),
			},
			ScriptKey: sdk.ScriptKey{PubKey: bindingPub(2)},
		}, point
	}
	firstRow, firstPoint := row(first)
	secondRow, secondPoint := row(second)
	client := &fundingSDK{bindingSDK: &bindingSDK{valid: true},
		rows: []*sdk.AssetRecord{secondRow},
		proofs: map[sdk.Outpoint][]byte{
			firstPoint:  bindingRaw(t, first),
			secondPoint: bindingRaw(t, second),
		}}
	chain := &fundingChain{bindingChain: &bindingChain{tip: 101,
		blocks: map[uint32]*wire.MsgBlock{
			100: bindingBlock(first),
			101: bindingBlock(second),
		}}}
	r := &proofNode{client: client, chain: chain}
	request := swapkit.CreateDepositRequest{
		GroupKey: bindingPub(11), Amount: 1000,
	}
	funding := func(minConfs uint32) ([]byte, error) {
		return FundingProof(
			t.Context(), r.wallet(), r.chain, minConfs, request,
		)
	}

	// The only candidate has one of three required confirmations.
	_, err := funding(3)
	require.ErrorIs(t, err, swapkit.ErrPending)
	require.NotErrorIs(t, err, swapkit.ErrInvalid)

	// A chain backend failure is reported as such.
	chain.heightErr = errors.New("chain backend unavailable")
	_, err = funding(1)
	require.ErrorIs(t, err, chain.heightErr)
	chain.heightErr = nil
	chain.unspentErr = errors.New("mempool lookup unavailable")
	_, err = funding(1)
	require.ErrorIs(t, err, chain.unspentErr)
	chain.unspentErr = nil

	// A spent output can never fund the deposit and stays invalid.
	chain.spent = second.OutPoint()
	_, err = funding(1)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	chain.spent = wire.OutPoint{}

	// A waiting candidate does not hide another one that is ready.
	client.rows = []*sdk.AssetRecord{secondRow, firstRow}
	raw, err := funding(2)
	require.NoError(t, err)
	require.Equal(t, bindingRaw(t, first), raw)

	// The waiting candidate is selected once it has confirmed.
	client.rows = []*sdk.AssetRecord{secondRow}
	chain.tip = 103
	raw, err = funding(3)
	require.NoError(t, err)
	require.Equal(t, bindingRaw(t, second), raw)
}
