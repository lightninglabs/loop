package loop

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// settlementRecorder ensures a short MPP never causes preimage disclosure.
type settlementRecorder struct {
	lndclient.InvoicesClient

	settled, canceled bool
}

func (r *settlementRecorder) SettleInvoice(context.Context, lntypes.Preimage) error {
	r.settled = true
	return nil
}

func (r *settlementRecorder) CancelInvoice(context.Context, lntypes.Hash) error {
	r.canceled = true
	return nil
}

func TestAssetInvoiceSettlementMinimum(t *testing.T) {
	for _, tc := range []struct {
		name    string
		units   []uint64
		minimum uint64
		settle  bool
	}{
		{"exact MPP", []uint64{2482, 2482}, 4964, true},
		{"MPP rounding shortfall", []uint64{2482, 2481}, 4964, false},
		{"better rate", []uint64{2500, 2500}, 4964, true},
		{"overflow", []uint64{math.MaxUint64, 1}, 4964, false},
		{"no asset payment", nil, 4964, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var htlcs []lndclient.InvoiceHtlc
			for _, units := range tc.units {
				data, err := json.Marshal(rfqmsg.JsonHtlc{
					Balances: []*rfqmsg.JsonAssetTranche{{AssetID: "01", Amount: units}},
				})
				require.NoError(t, err)
				htlcs = append(htlcs, lndclient.InvoiceHtlc{
					State: lnrpc.InvoiceHTLCState_ACCEPTED, CustomChannelData: data,
				})
			}
			// Failed attempts must not inflate the sum.
			htlcs = append(htlcs, lndclient.InvoiceHtlc{
				State: lnrpc.InvoiceHTLCState_CANCELED,
			})
			r := &settlementRecorder{}
			s := &loopInSwap{
				swapKit: swapKit{swapConfig: swapConfig{
					lnd: &lndclient.LndServices{Invoices: r},
				}},
				LoopInContract: loopdb.LoopInContract{
					AssetSwapInfo: &loopdb.LoopInAssetSwap{
						AssetId: []byte{1}, MinAssetAmount: tc.minimum,
					},
				},
			}
			// Use the normal swap logger for the cancellation path.
			cfg := newLoopInTestContext(t)
			s.swapKit.log = newSwapKit(lntypes.Hash{}, 0,
				newSwapConfig(&cfg.lnd.LndServices, cfg.store, cfg.server,
					nil, nil), &loopdb.SwapContract{}).log
			require.NoError(t, s.settleAssetInvoice(t.Context(),
				lndclient.InvoiceUpdate{Invoice: lndclient.Invoice{Htlcs: htlcs}}))
			require.Equal(t, tc.settle, r.settled)
			require.Equal(t, !tc.settle, r.canceled)
		})
	}
}

func TestAssetLoopInSelectors(t *testing.T) {
	_, pub := test.CreateKey(3)
	key := pub.SerializeCompressed()
	for _, tc := range []struct {
		name      string
		id, group []byte
		valid     bool
	}{
		{"asset", make([]byte, 32), nil, true},
		{"group", nil, key, true},
		{"both", make([]byte, 32), key, false},
		{"neither", nil, nil, false},
		{"invalid group", nil, make([]byte, 33), false},
		{"short group", nil, key[1:], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &LoopInRequest{AssetId: tc.id, GroupKey: tc.group,
				AssetEdgeNode: key, MinAssetAmount: 1}
			err := validateAssetLoopIn(r, &assets.TapdClient{}, true)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, route.NewVertex(pub), *r.LastHop)
		})
	}
}

func TestAssetLoopInQuote(t *testing.T) {
	for _, group := range []bool{false, true} {
		ctx := createClientTestContext(t, nil)
		t.Cleanup(ctx.finish)
		require.NoError(t, ctx.swapClient.waitForInitialized(t.Context()))
		_, pub := test.CreateKey(3)
		peer := route.NewVertex(pub)
		mock := &tapdInvoiceClientMock{peer: peer}
		ctx.swapClient.AssetClient = &assets.TapdClient{TaprootAssetChannelsClient: mock}
		recorder := &settlementRecorder{}
		ctx.swapClient.lndServices.Invoices = recorder
		r := &LoopInQuoteRequest{Amount: 50_000, ExternalHtlc: true,
			AssetInfo: &AssetLoopInQuoteRequest{AssetId: make([]byte, 32),
				AssetEdgeNode: peer[:]},
		}
		if group {
			r.AssetInfo.AssetId = nil
			r.AssetInfo.GroupKey = pub.SerializeCompressed()
		}
		quote, err := ctx.swapClient.LoopInQuote(t.Context(), r)
		require.NoError(t, err)
		require.EqualValues(t, 497, quote.AssetInfo.AssetAmount)
		require.Equal(t, "1000000", quote.AssetInfo.RateCoefficient)
		require.Equal(t, r.AssetInfo.GroupKey, mock.requests[0].GroupKey)
		require.True(t, recorder.canceled)
		require.False(t, recorder.settled)
		require.Equal(t, &peer, r.LastHop)
	}
}
