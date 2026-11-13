package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const (
	insertIssuanceCall = "insert issuance"
	verifyProofCall    = "verify proof"
)

// evidenceNode models tapd's authenticated RPC boundary. The tests exercise
// evidence selection and call ordering; tapd performs cryptographic verification.
type evidenceNode struct {
	calls      []string
	inserted   *universerpc.AssetProof
	insertErr  error
	verifyErr  error
	invalid    bool
	wrongID    bool
	wrongGroup bool
}

func (n *evidenceNode) InsertProof(_ context.Context, req *universerpc.AssetProof,
	_ ...grpc.CallOption) (*universerpc.AssetProofResponse, error) {

	n.calls = append(n.calls, insertIssuanceCall)
	n.inserted = req
	return &universerpc.AssetProofResponse{}, n.insertErr
}

func (n *evidenceNode) VerifyProof(_ context.Context, req *taprpc.ProofFile,
	_ ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	n.calls = append(n.calls, verifyProofCall)
	if n.verifyErr != nil {
		return nil, n.verifyErr
	}
	f, err := proof.DecodeFile(req.RawProofFile)
	if err != nil {
		return nil, err
	}
	p, err := f.LastProof()
	if err != nil {
		return nil, err
	}
	id := p.Asset.ID()
	if n.wrongID {
		id = asset.ID{9}
	}
	a := &taprpc.Asset{AssetGenesis: &taprpc.GenesisInfo{AssetId: id[:]}}
	if p.Asset.GroupKey != nil {
		key := p.Asset.GroupKey.GroupPubKey.SerializeCompressed()
		if n.wrongGroup {
			key = bindingKey(12).PubKey().SerializeCompressed()
		}
		a.AssetGroup = &taprpc.AssetGroup{TweakedGroupKey: key}
	}
	return &taprpc.VerifyProofResponse{Valid: !n.invalid, DecodedProof: &taprpc.DecodedProof{Asset: a}}, nil
}

func TestVerifyProofWithIssuance(t *testing.T) {
	p := bindingProof(
		t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false,
	)
	raw := bindingRaw(t, p)
	node := &evidenceNode{}
	req := &taprpc.ProofFile{RawProofFile: raw}
	for range 2 {
		node.calls = nil
		result, err := VerifyProofWithIssuance(t.Context(), node, req)
		require.NoError(t, err)
		require.True(t, result.Valid)
		require.Equal(t, []string{insertIssuanceCall, verifyProofCall}, node.calls)
		require.Equal(t, universerpc.ProofType_PROOF_TYPE_ISSUANCE, node.inserted.Key.Id.ProofType)
		require.Equal(t, p.Asset.GroupKey.GroupPubKey.SerializeCompressed()[1:], node.inserted.Key.Id.GetGroupKey())
		require.Equal(t, p.OutPoint().String(), node.inserted.Key.LeafKey.GetOpStr())
	}
	node.calls = nil
	node.insertErr = errors.New("issuance unavailable")
	_, err := VerifyProofWithIssuance(t.Context(), node, req)
	require.ErrorIs(t, err, node.insertErr)
	require.Equal(t, []string{insertIssuanceCall}, node.calls)
	node.calls = nil
	node.insertErr = nil
	node.verifyErr = errors.New("verification unavailable")
	_, err = VerifyProofWithIssuance(t.Context(), node, req)
	require.ErrorIs(t, err, node.verifyErr)
	require.Equal(t, []string{insertIssuanceCall, verifyProofCall}, node.calls)
	node.calls = nil
	_, err = VerifyProofWithIssuance(t.Context(), node, &taprpc.ProofFile{RawProofFile: []byte{1}})
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	require.Empty(t, node.calls)
	changed := *p
	changed.Asset = *p.Asset.Copy()
	changed.Asset.Genesis.Tag += "-other"
	f, err := proof.NewFile(proof.V0, *p, changed)
	require.NoError(t, err)
	bytes, err := proof.EncodeFile(f)
	require.NoError(t, err)
	_, err = VerifyProofWithIssuance(t.Context(), node, &taprpc.ProofFile{RawProofFile: bytes})
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	require.Empty(t, node.calls)
}
