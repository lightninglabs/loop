package reservation

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/deposit"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type reservationProofVerifier struct {
	valid bool
	err   error
}

type issuanceProofClient struct {
	request   *universerpc.AssetProof
	calls     []string
	insertErr error
	valid     bool
}

func (v *issuanceProofClient) InsertProof(_ context.Context,
	req *universerpc.AssetProof, _ ...grpc.CallOption) (
	*universerpc.AssetProofResponse, error) {

	v.calls = append(v.calls, "issuance")
	v.request = req
	return &universerpc.AssetProofResponse{}, v.insertErr
}

func (v *issuanceProofClient) VerifyProof(context.Context,
	*taprpc.ProofFile, ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	v.calls = append(v.calls, "history")
	return &taprpc.VerifyProofResponse{
		Valid: v.valid,
	}, nil
}

func (v reservationProofVerifier) VerifyProof(context.Context,
	*taprpc.ProofFile, ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	return &taprpc.VerifyProofResponse{
		Valid: v.valid,
	}, v.err
}

func TestReservationProofBinding(t *testing.T) {
	_, server := btcec.PrivKeyFromBytes([]byte{1})
	_, client := btcec.PrivKeyFromBytes([]byte{2})
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Index: 4,
		},
		Tag:  "reservation proof",
		Type: asset.Normal,
	}
	kit, err := deposit.NewKit(server, client, keychain.KeyLocator{},
		genesis.ID(), 1440, &address.RegressionNetTap)
	require.NoError(t, err)
	opTrue, _, _, _, err := htlc.CreateOpTrueLeaf()
	require.NoError(t, err)
	a, err := asset.New(genesis, 100, 0, 0,
		asset.NewScriptKey(opTrue.PubKey), nil,
		asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	root, err := commitment.FromAssets(&version, a)
	require.NoError(t, err)
	_, inclusion, err := root.Proof(a.TapCommitmentKey(), a.AssetCommitmentKey())
	require.NoError(t, err)
	key, err := input.MuSig2CombineKeys(input.MuSig2Version100RC2,
		[]*btcec.PublicKey{server, client}, true, &input.MuSig2Tweaks{})
	require.NoError(t, err)
	timeout, err := kit.GenTimeoutPathScript()
	require.NoError(t, err)
	sibling, err := commitment.NewPreimageFromLeaf(txscript.NewBaseTapLeaf(timeout))
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	script, err := tapscript.PayToAddrScript(*key.PreTweakedKey,
		siblingHash, *root)
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&genesis.FirstPrevOut, nil, nil))
	tx.AddTxOut(wire.NewTxOut(330, []byte{txscript.OP_TRUE}))
	tx.AddTxOut(wire.NewTxOut(2000, script))
	p := proof.Proof{
		AnchorTx:    *tx,
		Asset:       *a,
		BlockHeight: 100,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 1,
			InternalKey: key.PreTweakedKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof:              *inclusion,
				TapSiblingPreimage: sibling,
			},
		},
	}
	file, err := proof.NewFile(proof.V0, p)
	require.NoError(t, err)
	var raw bytes.Buffer
	require.NoError(t, file.Encode(&raw))
	verifier := reservationProofVerifier{
		valid: true,
	}
	got, err := VerifyReservationProof(
		t.Context(), verifier, kit, raw.Bytes(),
		p.OutPoint(), 100)
	require.NoError(t, err)
	require.Equal(t, p.OutPoint(), got.OutPoint())
	_, err = VerifyReservationProof(t.Context(), verifier, kit, raw.Bytes(),
		p.OutPoint(), 99)
	require.ErrorIs(t, err, ErrInvalidReservation)
	wrong := p.OutPoint()
	wrong.Index = 0
	_, err = VerifyReservationProof(
		t.Context(), verifier, kit, raw.Bytes(), wrong, 100)
	require.ErrorIs(t, err, ErrInvalidReservation)
	verifier.valid = false
	_, err = VerifyReservationProof(
		t.Context(), verifier, kit, raw.Bytes(), p.OutPoint(), 100)
	require.ErrorIs(t, err, ErrInvalidReservation)
	verifier.err = errors.New("node unavailable")
	_, err = VerifyReservationProof(
		t.Context(), verifier, kit, raw.Bytes(), p.OutPoint(), 100)
	require.ErrorIs(t, err, verifier.err)
	require.NotErrorIs(t, err, ErrInvalidReservation)

	t.Run("issuance before full history", func(t *testing.T) {
		node := &issuanceProofClient{
			valid: true,
		}
		v := tapProofVerifier{
			tap: node,
		}
		_, err := VerifyReservationProof(
			t.Context(), v, kit, raw.Bytes(),
			p.OutPoint(), 100)
		require.NoError(t, err)
		require.Equal(t, []string{"issuance", "history"}, node.calls)
		id := genesis.ID()
		require.Equal(t, id[:], node.request.Key.Id.GetAssetId())
		require.Equal(t, universerpc.ProofType_PROOF_TYPE_ISSUANCE,
			node.request.Key.Id.ProofType)
		require.Equal(t, p.OutPoint().String(),
			node.request.Key.LeafKey.GetOpStr())
		encoded, err := file.RawProofAt(0)
		require.NoError(t, err)
		require.Equal(t, encoded, node.request.AssetLeaf.Proof)

		// Recovery repeats the verified metadata insert, not a wallet
		// import. Full history must still pass on every invocation.
		node.calls = nil
		node.valid = false
		_, err = VerifyReservationProof(
			t.Context(), v, kit, raw.Bytes(),
			p.OutPoint(), 100)
		require.ErrorIs(t, err, ErrInvalidReservation)
		require.Equal(t, []string{"issuance", "history"}, node.calls)

		node.calls = nil
		node.insertErr = errors.New("issuance rejected")
		_, err = VerifyReservationProof(
			t.Context(), v, kit, raw.Bytes(),
			p.OutPoint(), 100)
		require.ErrorIs(t, err, node.insertErr)
		require.Equal(t, []string{"issuance"}, node.calls)

		node.calls = nil
		_, err = VerifyReservationProof(
			t.Context(), v, kit, raw.Bytes(),
			p.OutPoint(), 99)
		require.ErrorIs(t, err, ErrInvalidReservation)
		require.Empty(t, node.calls)
	})
}
