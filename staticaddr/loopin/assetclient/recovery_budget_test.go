package assetclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type preimageServer struct {
	Server

	get func(context.Context) (*swapserverrpc.AssetLoopInStatus, error)
}

func (s *preimageServer) GetAssetLoopIn(ctx context.Context,
	_ *swapserverrpc.AssetLoopInReference, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return s.get(ctx)
}

type recoveryActions struct {
	sessionActions

	evidence []byte
	lookup   func(context.Context) ([32]byte, bool, error)
	refund   func(context.Context) ([]byte, error)
}

func (a *recoveryActions) Prepared(context.Context, assetexec.Offer) (
	[]byte, error) {

	return a.evidence, nil
}

func (a *recoveryActions) LookupPreimage(ctx context.Context,
	_ assetexec.Offer) ([32]byte, bool, error) {

	return a.lookup(ctx)
}

func (a *recoveryActions) RecoverRefund(ctx context.Context,
	_ assetexec.Offer, _ assetexec.Fallback) ([]byte, error) {

	return a.refund(ctx)
}

type cancelInvoices struct {
	invoicesrpc.InvoicesClient

	calls int
}

func (i *cancelInvoices) CancelInvoice(ctx context.Context,
	_ *invoicesrpc.CancelInvoiceMsg, _ ...grpc.CallOption) (
	*invoicesrpc.CancelInvoiceResp, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i.calls++
	return &invoicesrpc.CancelInvoiceResp{}, nil
}

func TestBlockedPeerCannotStarveChainLookupOrRefund(t *testing.T) {
	_, _, offer := offerFixture(t)
	store, err := assetexec.OpenBoltStore(
		filepath.Join(t.TempDir(), "client.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	evidence, err := json.Marshal(assetln.Evidence{
		Hash: offer.Hash, GroupKey: offer.Terms.GroupKey,
		Amount: offer.Terms.PayoutAmount, ChannelID: 42, HTLCIndex: 7,
	})
	require.NoError(t, err)
	chainCalls, refundCalls := 0, 0
	actions := &recoveryActions{
		evidence: evidence,
		lookup: func(ctx context.Context) ([32]byte, bool, error) {
			require.NoError(t, ctx.Err())
			chainCalls++
			return [32]byte{}, false, nil
		},
		refund: func(ctx context.Context) ([]byte, error) {
			require.NoError(t, ctx.Err())
			refundCalls++
			return []byte("current confirmed refund proofs"), nil
		},
	}
	controller := assetexec.NewController(store, actions, nil)
	require.NoError(
		t,
		controller.Start(t.Context(), offer, []byte("intent")),
	)
	_, err = controller.PrepareInvoice(t.Context(), offer.Hash)
	require.NoError(t, err)
	require.NoError(t, controller.ReceiveFallback(t.Context(), offer.Hash,
		[]byte("server fallback")))
	require.NoError(t, controller.ExposeClaim(t.Context(), offer.Hash))

	invoices := &cancelInvoices{}
	adapter := &Adapter{
		Actions: actions, Store: store, HoldExpiryDelta: 10,
		Receiver: &assetln.Receiver{
			Lightning: &testLightning{invoice: &lnrpc.Invoice{
				RHash: offer.Hash[:],
				State: lnrpc.Invoice_ACCEPTED,
			}},
			Invoices: invoices,
		},
		Server: &preimageServer{get: func(ctx context.Context) (
			*swapserverrpc.AssetLoopInStatus, error) {

			<-ctx.Done()
			return nil, ctx.Err()
		}},
	}
	controller = assetexec.NewController(store, adapter, func() time.Time {
		return offer.Terms.OfferExpiry
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, controller.Recover(ctx, offer.Hash))
	require.NoError(t, ctx.Err())
	require.Equal(t, 1, chainCalls)
	require.Equal(t, 1, refundCalls)
	require.Equal(t, 1, invoices.calls)
	record, err := store.Get(t.Context(), offer.Hash)
	require.NoError(t, err)
	require.Equal(t, assetexec.HtlcTimeoutSwept, record.State)
	require.True(t, record.DepositsReleased)
}

func TestMalformedPeerPreimageCannotVetoChainWitness(t *testing.T) {
	for _, name := range []string{"short", "wrong hash"} {
		for _, chainFound := range []bool{true, false} {
			t.Run(name+"/"+map[bool]string{
				true: "witness", false: "missing",
			}[chainFound], func(t *testing.T) {
				testMalformedPeerPreimage(t, name, chainFound)
			})
		}
	}
}

func testMalformedPeerPreimage(t *testing.T, name string, chainFound bool) {
	t.Helper()
	_, _, offer := offerFixture(t)
	preimage := [32]byte{42}
	offer.Hash = sha256.Sum256(preimage[:])
	invalid := []byte{1}
	if name == "wrong hash" {
		invalid = make([]byte, 32)
	}
	calls := 0
	adapter := &Adapter{
		Receiver: &assetln.Receiver{
			Lightning: &testLightning{
				invoice: &lnrpc.Invoice{
					RHash: offer.Hash[:],
					State: lnrpc.Invoice_ACCEPTED,
				},
			},
		},
		Server: &preimageServer{
			get: func(context.Context) (
				*swapserverrpc.AssetLoopInStatus, error) {

				return &swapserverrpc.AssetLoopInStatus{
					ExecutionPreimage: invalid,
				}, nil
			},
		},
		Actions: &recoveryActions{
			lookup: func(ctx context.Context) (
				[32]byte, bool, error) {

				calls++
				require.NoError(
					t,
					ctx.Err(),
				)
				return preimage, chainFound, nil
			},
		},
	}
	got, found, err := adapter.LookupPreimage(
		t.Context(),
		offer,
	)
	require.Equal(t, 1, calls)
	require.Equal(t, chainFound, found)
	if chainFound {
		require.NoError(t, err)
		require.Equal(t, preimage, got)
	} else {
		require.ErrorIs(t, err, assetexec.ErrInvalid)
	}
}
