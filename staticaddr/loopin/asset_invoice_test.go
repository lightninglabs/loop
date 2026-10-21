package loopin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type assetLightningService struct {
	lndclient.LightningClient

	raw lnrpc.LightningClient
}

func (s assetLightningService) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, lnrpc.LightningClient) {

	return ctx, time.Minute, s.raw
}

type assetInvoiceService struct {
	lndclient.InvoicesClient

	raw invoicesrpc.InvoicesClient
}

func (s assetInvoiceService) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, invoicesrpc.InvoicesClient) {

	return ctx, time.Minute, s.raw
}

type invoiceLightning struct {
	lnrpc.LightningClient

	invoice   *lnrpc.Invoice
	lookupErr error
	decoded   *lnrpc.PayReq
	channels  []*lnrpc.Channel
}

func (m *invoiceLightning) DecodePayReq(context.Context, *lnrpc.PayReqString,
	...grpc.CallOption) (*lnrpc.PayReq, error) {

	return m.decoded, nil
}

func (m *invoiceLightning) ListChannels(
	context.Context,
	*lnrpc.ListChannelsRequest,
	...grpc.CallOption,
) (*lnrpc.ListChannelsResponse,
	error) {

	return &lnrpc.ListChannelsResponse{Channels: m.channels}, nil
}

func (m *invoiceLightning) LookupInvoice(context.Context, *lnrpc.PaymentHash,
	...grpc.CallOption) (*lnrpc.Invoice, error) {

	return m.invoice, m.lookupErr
}

type invoiceAssets struct {
	tapchannelrpc.TaprootAssetChannelsClient

	add func(
		*tapchannelrpc.AddInvoiceRequest,
	) *tapchannelrpc.AddInvoiceResponse
}

func (m *invoiceAssets) AddInvoice(_ context.Context,
	r *tapchannelrpc.AddInvoiceRequest, _ ...grpc.CallOption) (
	*tapchannelrpc.AddInvoiceResponse, error) {

	return m.add(r), nil
}

type settleInvoices struct {
	invoicesrpc.InvoicesClient

	err error
}

func (m *settleInvoices) SettleInvoice(context.Context,
	*invoicesrpc.SettleInvoiceMsg, ...grpc.CallOption) (
	*invoicesrpc.SettleInvoiceResp, error) {

	return nil, m.err
}

func TestCreateHoldInvoice(t *testing.T) {
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	offer := swapkit.Offer{
		Hash: sha256.Sum256([]byte("server secret")),
		Terms: swapkit.Terms{
			PayoutAmount: 1000, MaxDebit: 1010, RefundHeight: 200,
			RecoveryDelta: 50, OfferExpiry: time.Now().Add(time.Minute),
		},
	}
	copy(offer.Terms.GroupKey[:], key.PubKey().SerializeCompressed())
	ln := &invoiceLightning{decoded: &lnrpc.PayReq{
		PaymentHash: hex.EncodeToString(offer.Hash[:]),
		NumMsat:     1_000_000, Timestamp: time.Now().Unix(),
		Expiry: 3600, CltvExpiry: 171,
	}}
	tapd := &invoiceAssets{add: func(
		req *tapchannelrpc.AddInvoiceRequest,
	) *tapchannelrpc.AddInvoiceResponse {

		require.Equal(t, offer.Hash[:], req.HodlInvoice.PaymentHash)
		require.Equal(t, offer.Terms.GroupKey[:], req.GroupKey)
		require.Equal(t, offer.Terms.PayoutAmount, req.AssetAmount)
		require.Equal(t, req.AssetAmount, *req.AssetMinAmt)
		require.Empty(t, req.InvoiceRequest.RPreimage)
		require.Zero(t, req.InvoiceRequest.ValueMsat)
		require.Zero(t, req.InvoiceRequest.Value)
		require.EqualValues(t, 171, req.InvoiceRequest.CltvExpiry)
		return &tapchannelrpc.AddInvoiceResponse{
			InvoiceResult: &lnrpc.AddInvoiceResponse{
				PaymentRequest: "hold invoice",
			},
			AcceptedBuyQuote: &rfqrpc.PeerAcceptedBuyQuote{
				Id:             make([]byte, 32),
				Expiry:         uint64(offer.Terms.OfferExpiry.Unix()),
				Peer:           hex.EncodeToString(req.PeerPubkey),
				AssetMaxAmount: offer.Terms.PayoutAmount,
				AssetSpec: &rfqrpc.AssetSpec{
					GroupPubKey: offer.Terms.GroupKey[1:],
				},
			},
		}
	}}
	f := &FSM{
		loopIn: &StaticAddressLoopIn{
			SwapHash: offer.Hash, Asset: &AssetSwapInfo{Offer: offer},
		},
		cfg: &Config{
			LndClient: assetLightningService{raw: ln},
			Assets:    &AssetConfig{Tapd: tapd},
		},
	}
	_, err := f.createAssetInvoice(t.Context(), 100)
	require.NoError(t, err)
	ln.decoded.CltvExpiry = 80
	_, err = f.createAssetInvoice(t.Context(), 100)
	require.ErrorIs(t, err, assets.ErrEvidence)
}

func TestReceiverIncompleteResponsesFailClosed(t *testing.T) {
	preimage := [32]byte{1}
	hash := sha256.Sum256(preimage[:])
	lost := errors.New("lost settlement response")
	ln := &invoiceLightning{}
	f := &FSM{
		loopIn: &StaticAddressLoopIn{
			SwapHash: hash, Asset: &AssetSwapInfo{
				Offer: swapkit.Offer{Hash: hash},
				Accepted: &assets.Evidence{
					Hash: hash, ChannelID: 42, HTLCIndex: 1,
				},
			},
		},
		cfg: &Config{
			LndClient: assetLightningService{raw: ln},
			InvoicesClient: assetInvoiceService{
				raw: &settleInvoices{err: lost},
			},
			Assets: &AssetConfig{},
		},
	}
	require.ErrorIs(t, f.settleAssetInvoice(t.Context(), preimage), lost)
	ln.invoice = &lnrpc.Invoice{State: lnrpc.Invoice_SETTLED}
	require.ErrorIs(t, f.settleAssetInvoice(t.Context(), preimage), lost)
	ln.invoice.RHash = hash[:]
	ln.invoice.Htlcs = []*lnrpc.InvoiceHTLC{{
		State: lnrpc.InvoiceHTLCState_SETTLED, ChanId: 42, HtlcIndex: 1,
	}}
	require.NoError(t, f.settleAssetInvoice(t.Context(), preimage))
	_, err := f.acceptedAssetPayment(t.Context(), 100)
	require.ErrorIs(t, err, assets.ErrEvidence)
}
