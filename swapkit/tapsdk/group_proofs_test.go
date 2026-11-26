package tapsdk

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func groupEvidence(t *testing.T, tag string, anchor bool) *proof.Proof {
	t.Helper()
	g := bindingGenesis()
	g.Tag = tag
	p := bindingProof(t, g, bindingKey(2).PubKey(), bindingKey(3).PubKey(), nil, g.FirstPrevOut, 100, false)
	p.Asset.PrevWitnesses[0].TxWitness = wire.TxWitness{make([]byte, 64)}
	if anchor {
		var err error
		p.GroupKeyReveal, err = asset.NewGroupKeyReveal(
			asset.GroupKey{
				RawKey: keychain.KeyDescriptor{
					PubKey: bindingKey(11).PubKey(),
				},
			}, g.ID(),
		)
		require.NoError(t, err)
	}
	return p
}

func TestVerifyGroupMembership(t *testing.T) {
	anchor := groupEvidence(t, "anchor", true)
	fund := groupEvidence(t, "fund", false)
	pay := groupEvidence(t, "pay", false)
	encode := func(p *proof.Proof) []byte {
		raw, err := p.Bytes()
		require.NoError(t, err)
		return raw
	}
	proofs := [][]byte{encode(anchor), encode(fund), encode(pay)}
	key := anchor.Asset.GroupKey.GroupPubKey.SerializeCompressed()
	needed := []asset.ID{fund.Asset.ID(), pay.Asset.ID()}
	limit := len(proofs[0]) + len(proofs[1]) + len(proofs[2])
	check := func(node *evidenceNode, ids []asset.ID, data [][]byte,
		maxBytes int) error {

		return VerifyGroupMembership(
			t.Context(), node, &address.RegressionNetTap, key, ids,
			data, maxBytes,
		)
	}
	node := &evidenceNode{}
	require.NoError(t, check(node, needed, proofs, limit))
	require.Equal(t, []string{verifyProofCall, verifyProofCall, verifyProofCall}, node.calls)
	require.NoError(t, check(&evidenceNode{}, []asset.ID{fund.Asset.ID(), fund.Asset.ID()}, proofs[:2], limit))
	require.NoError(t, check(&evidenceNode{}, []asset.ID{anchor.Asset.ID()}, proofs[:1], limit))
	for _, test := range []struct {
		name string
		data [][]byte
		node *evidenceNode
		max  int
	}{
		{"missing issuance", proofs[:2], &evidenceNode{}, limit},
		{"wrong order", [][]byte{proofs[1], proofs[0], proofs[2]}, &evidenceNode{}, limit},
		{"duplicate issuance", [][]byte{proofs[0], proofs[1], proofs[1]}, &evidenceNode{}, limit},
		{"byte limit", proofs, &evidenceNode{}, limit - 1},
		{"invalid proof", proofs, &evidenceNode{invalid: true}, limit},
		{"wrong decoded issuance", proofs, &evidenceNode{wrongID: true}, limit},
		{"wrong decoded group", proofs, &evidenceNode{wrongGroup: true}, limit},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorIs(t, check(test.node, needed, test.data, test.max), swapkit.ErrInvalid)
		})
	}
	node = &evidenceNode{verifyErr: errors.New("node unavailable")}
	require.ErrorIs(t, check(node, needed, proofs, limit), node.verifyErr)
	require.Len(t, node.calls, 1)
	collectible := *fund
	collectible.Asset = *fund.Asset.Copy()
	collectible.Asset.Type = asset.Collectible
	require.ErrorIs(t, check(
		&evidenceNode{}, needed,
		[][]byte{proofs[0], encode(&collectible), proofs[2]}, limit+1000,
	), swapkit.ErrInvalid)
}
