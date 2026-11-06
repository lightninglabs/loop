package reservation

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestGroupPurchaseIdentity(t *testing.T) {
	_, pub := btcec.PrivKeyFromBytes([]byte{3})
	group := pub.SerializeCompressed()
	fund, pay := [32]byte{1}, [32]byte{2}
	_, err := ParseAssetSelector(fund[:], group)
	require.Error(t, err)
	_, err = ParseAssetSelector(nil, group[1:])
	require.Error(t, err)
	selector, err := ParseAssetSelector(nil, group)
	require.NoError(t, err)
	r := testReservation()
	r.GroupKey = selector.GroupKey
	r.Terms = Terms{Amount: r.Amount}
	r.State = RequestQuote
	store := NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	ctx := t.Context()
	require.NoError(t, store.CreateReservation(ctx, r))
	q := testPurchaseQuote(r)
	q.Terms.AssetId = fund[:]
	q.Group = &swapserverrpc.AssetReservationGroup{GroupKey: group, PaymentAssetId: pay[:]}
	require.NoError(t, r.ValidateQuote(q))
	wrong := proto.Clone(q).(*swapserverrpc.AssetReservationQuote)
	wrong.Group = nil
	require.Error(t, r.ValidateQuote(wrong))
	r.Quote = q
	r.Terms, err = TermsFromRPC(q.Terms)
	require.NoError(t, err)
	r.State = AwaitApproval
	require.NoError(t, store.UpdateReservation(ctx, r))
	saved, err := store.GetReservation(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, selector, saved.selector())
	require.Equal(t, fund, saved.AssetID)
	require.Equal(t, pay[:], saved.Quote.Group.PaymentAssetId)
	// Changing just the Lightning issuance must invalidate both the
	// approval hash and a replay of the stored quote.
	approval := testApproval(t, saved)
	saved.Quote.Group.PaymentAssetId = fund[:]
	require.ErrorContains(t, saved.CheckApproval(approval),
		"approval does not match reservation quote")
	require.Error(t, store.UpdateReservation(ctx, saved))
}
