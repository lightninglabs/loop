package loopin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type assetFSMNode struct {
	lnrpc.LightningClient

	invoice  *lnrpc.Invoice
	channels []*lnrpc.Channel
	terms    assetln.Terms
}

func (n *assetFSMNode) LookupInvoice(context.Context, *lnrpc.PaymentHash,
	...grpc.CallOption) (*lnrpc.Invoice, error) {

	return n.invoice, nil
}

func (n *assetFSMNode) ListChannels(context.Context, *lnrpc.ListChannelsRequest,
	...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {

	return &lnrpc.ListChannelsResponse{Channels: n.channels}, nil
}

func (n *assetFSMNode) DecodePayReq(context.Context, *lnrpc.PayReqString,
	...grpc.CallOption) (*lnrpc.PayReq, error) {

	return &lnrpc.PayReq{
		PaymentHash: hex.EncodeToString(n.terms.Hash[:]), NumMsat: 100_000,
		Timestamp: time.Now().Unix(), Expiry: 3600,
		CltvExpiry: int64(n.terms.RefundHeight - 100 + n.terms.RecoveryBlocks +
			n.terms.HoldExpiryDelta + 1),
	}, nil
}

type assetFSMInvoices struct {
	invoicesrpc.InvoicesClient

	node    *assetFSMNode
	cancels int
}

func (i *assetFSMInvoices) SettleInvoice(_ context.Context,
	req *invoicesrpc.SettleInvoiceMsg, _ ...grpc.CallOption) (
	*invoicesrpc.SettleInvoiceResp, error) {

	i.node.invoice.State = lnrpc.Invoice_SETTLED
	i.node.invoice.Htlcs[0].State = lnrpc.InvoiceHTLCState_SETTLED
	i.node.invoice.RPreimage = req.Preimage
	return &invoicesrpc.SettleInvoiceResp{}, nil
}

func (i *assetFSMInvoices) CancelInvoice(context.Context,
	*invoicesrpc.CancelInvoiceMsg, ...grpc.CallOption) (
	*invoicesrpc.CancelInvoiceResp, error) {

	i.cancels++
	i.node.invoice.State = lnrpc.Invoice_CANCELED
	i.node.invoice.Htlcs[0].State = lnrpc.InvoiceHTLCState_CANCELED
	return &invoicesrpc.CancelInvoiceResp{}, nil
}

type assetFSMTapd struct {
	tapchannelrpc.TaprootAssetChannelsClient

	terms assetln.Terms
}

func (a *assetFSMTapd) AddInvoice(_ context.Context,
	req *tapchannelrpc.AddInvoiceRequest, _ ...grpc.CallOption) (
	*tapchannelrpc.AddInvoiceResponse, error) {

	return &tapchannelrpc.AddInvoiceResponse{
		InvoiceResult: &lnrpc.AddInvoiceResponse{PaymentRequest: "asset invoice"},
		AcceptedBuyQuote: &rfqrpc.PeerAcceptedBuyQuote{
			Id: make([]byte, 32), Expiry: uint64(a.terms.Deadline),
			Peer: hex.EncodeToString(req.PeerPubkey), AssetMaxAmount: a.terms.Amount,
			AssetSpec: &rfqrpc.AssetSpec{GroupPubKey: a.terms.GroupKey[1:]},
		},
	}, nil
}

type assetFSMDriver struct {
	AssetDriver

	height  uint32
	refunds int
}

func (d *assetFSMDriver) Height(context.Context) (uint32, error) {
	return d.height, nil
}
func (*assetFSMDriver) PrepareRecovery(context.Context, swapkit.Offer) (
	[]byte, error) {

	return []byte("unsigned recovery"), nil
}
func (*assetFSMDriver) BeginFallback(context.Context, swapkit.Offer, []byte) (
	*swapkit.FallbackRound, error) {

	return &swapkit.FallbackRound{
		Close: func() {},
		Finalize: func(context.Context, []byte) (swapkit.Fallback, error) {
			return swapkit.Fallback{Bundle: []byte("complete fallback"),
				ClaimSignatures: []byte("conditional signatures")}, nil
		},
	}, nil
}
func (*assetFSMDriver) LookupPreimage(context.Context, swapkit.Operation) (
	swapkit.PreimageResult, error) {

	return swapkit.PreimageResult{}, nil
}
func (d *assetFSMDriver) RecoverRefund(context.Context, swapkit.Operation) (
	[]byte, error) {

	d.refunds++
	return []byte("confirmed spendable asset refund"), nil
}

type assetFSMServer struct {
	AssetServer

	commit          func()
	preimage        []byte
	commits         int
	prepareFailures int
}

func (s *assetFSMServer) PrepareAssetLoopIn(context.Context,
	*swapserverrpc.PrepareAssetLoopInRequest, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	if s.prepareFailures > 0 {
		s.prepareFailures--
		return nil, status.Error(codes.Unavailable, "pending")
	}
	return &swapserverrpc.AssetLoopInStatus{}, nil
}
func (*assetFSMServer) AssetLoopInFallback(context.Context,
	*swapserverrpc.AssetLoopInReference, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInFallbackResponse, error) {

	return &swapserverrpc.AssetLoopInFallbackResponse{
		RecoveryBundle: []byte("server partial"),
	}, nil
}
func (s *assetFSMServer) CommitAssetLoopIn(context.Context,
	*swapserverrpc.CommitAssetLoopInRequest, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	s.commits++
	if s.commit != nil {
		s.commit()
	}
	return nil, fmt.Errorf("lost commit reply")
}
func (s *assetFSMServer) GetAssetLoopIn(context.Context,
	*swapserverrpc.AssetLoopInReference, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return &swapserverrpc.AssetLoopInStatus{ExecutionPreimage: s.preimage}, nil
}
func (*assetFSMServer) CancelAssetLoopIn(context.Context,
	*swapserverrpc.AssetLoopInReference, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return &swapserverrpc.AssetLoopInStatus{}, nil
}
func (*assetFSMServer) ReportAssetLoopInRefund(context.Context,
	*swapserverrpc.ReportAssetLoopInRefundRequest, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return &swapserverrpc.AssetLoopInStatus{}, nil
}

type assetFSMDeposits struct {
	mockDepositManager

	store *deposit.SqlStore
}

func (m *assetFSMDeposits) TransitionDeposits(ctx context.Context,
	deposits []*deposit.Deposit, _ fsm.EventType, state fsm.StateType) error {

	for _, d := range deposits {
		if d.IsInFinalState() || d.IsInState(state) {
			return fmt.Errorf("deposit already finalized or in state %s", state)
		}
		d.SetState(state)
		if err := m.store.UpdateDeposit(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

type assetFSMFixture struct {
	db       *loopdb.SqliteSwapStore
	dbConfig *loopdb.SqliteConfig
	store    *SqlStore
	swap     *StaticAddressLoopIn
	cfg      *Config
	node     *assetFSMNode
	invoices *assetFSMInvoices
	server   *assetFSMServer
	driver   *assetFSMDriver
	preimage [32]byte
}

func newAssetFSMFixture(t *testing.T) *assetFSMFixture {
	t.Helper()
	f := &assetFSMFixture{
		dbConfig: &loopdb.SqliteConfig{
			DatabaseFileName: filepath.Join(t.TempDir(), "loop.db"),
		},
		preimage: [32]byte{7},
		server:   &assetFSMServer{},
		driver:   &assetFSMDriver{height: 100},
	}
	var err error
	f.db, err = loopdb.NewSqliteStore(f.dbConfig, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.db.Close()) })
	f.store = NewSqlStore(
		loopdb.NewTypedStore[Querier](f.db), clock.NewDefaultClock(),
		&chaincfg.RegressionNetParams,
	)
	_, key := btcec.PrivKeyFromBytes([]byte{1})
	group := [33]byte{}
	copy(group[:], key.SerializeCompressed())
	point := wire.OutPoint{Hash: chainhash.Hash{1}}
	terms := swapkit.Terms{
		RequestID: [32]byte{1}, GroupKey: group, PayoutAmount: 1000, MaxDebit: 1000,
		RefundHeight: 200, RecoveryDelta: 10, ExecutionDelta: 5,
		OfferExpiry:      time.Now().Add(time.Hour),
		DepositOutpoints: []string{point.String()},
	}
	offer := swapkit.Offer{Hash: sha256.Sum256(f.preimage[:]), Terms: terms}
	asset := &AssetSwapInfo{Offer: offer, Request: AssetSwapRequest{
		RequestID: terms.RequestID, GroupKey: group, Amount: 1000, MaxDebit: 1000,
		Outpoint: point.String(),
	}}
	d := &deposit.Deposit{ID: deposit.ID{1}, OutPoint: point, Value: 10000,
		Asset: &swapkit.Deposit{
			Outpoint: point.String(), GroupKey: group, Amount: 1000,
			Contract: []byte("contract"), Proof: []byte("input proof")},
		TimeOutSweepPkScript: []byte{},
	}
	d.SetState(deposit.LoopingIn)
	deposits := deposit.NewSqlStore(f.db.BaseDB)
	require.NoError(t, deposits.CreateDeposit(t.Context(), d))
	f.swap = &StaticAddressLoopIn{
		Asset: asset, SwapHash: offer.Hash, InitiationTime: time.Now(),
		Deposits: []*deposit.Deposit{d}, DepositOutpoints: []string{point.String()},
		ClientPubkey: key, ServerPubkey: key, HtlcCltvExpiry: 200,
	}
	f.swap.SetState(InitHtlcTx)
	require.NoError(t, f.store.CreateLoopIn(t.Context(), f.swap))
	termsLN := asset.paymentTerms()
	f.node = &assetFSMNode{terms: termsLN, invoice: &lnrpc.Invoice{
		RHash: offer.Hash[:], State: lnrpc.Invoice_ACCEPTED,
		Htlcs: []*lnrpc.InvoiceHTLC{{State: lnrpc.InvoiceHTLCState_ACCEPTED,
			ChanId: 42, HtlcIndex: 3, ExpiryHeight: 231,
			CustomChannelData: fmt.Appendf(nil,
				`{"rfq_id":"%064x","balances":[{"asset_id":"%x","amount":1000}]}`,
				1, group[1:]),
		}},
	}, channels: []*lnrpc.Channel{{ChanId: 42,
		CustomChannelData: fmt.Appendf(nil,
			`{"group_key":"%x"}`, group[:])}}}
	f.invoices = &assetFSMInvoices{node: f.node}
	f.cfg = &Config{
		Store:          f.store,
		AddressManager: &mockAddressManager{params: &script.Parameters{}},
		DepositManager: &assetFSMDeposits{store: deposits},
		Assets: &AssetConfig{
			Driver: f.driver, Server: f.server,
			Receiver: &assetln.Receiver{
				Lightning: f.node, Invoices: f.invoices,
				Assets: &assetFSMTapd{terms: termsLN},
			},
		},
	}
	return f
}

func (f *assetFSMFixture) reopen(t *testing.T) {
	t.Helper()
	require.NoError(t, f.db.Close())
	var err error
	f.db, err = loopdb.NewSqliteStore(f.dbConfig, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	f.store = NewSqlStore(
		loopdb.NewTypedStore[Querier](f.db), clock.NewDefaultClock(),
		&chaincfg.RegressionNetParams,
	)
	f.cfg.Store = f.store
	f.cfg.DepositManager = &assetFSMDeposits{
		store: deposit.NewSqlStore(f.db.BaseDB),
	}
	f.swap, err = f.store.GetLoopInByHash(t.Context(), f.swap.SwapHash)
	require.NoError(t, err)
}

// TestAssetFSMRestart uses the actual FSM and a reopened normal SQL database.
// A lost commit reply retains the fallback; server disclosure later settles
// the invoice and completes without a server claim-proof dependency.
func TestAssetFSMRestart(t *testing.T) {
	f := newAssetFSMFixture(t)
	f.server.prepareFailures = 1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.server.commit = func() {
		stored, err := f.store.GetLoopInByHash(t.Context(), f.swap.SwapHash)
		require.NoError(t, err)
		require.Equal(t, MonitorInvoiceAndHtlcTx, stored.GetState())
		require.NotEmpty(t, stored.Asset.Fallback.Bundle)
		require.NotNil(t, stored.Asset.Accepted)
		require.Zero(t, stored.SwapPreimage)
		cancel()
	}
	machine, err := NewFSM(ctx, f.swap, f.cfg, true)
	require.NoError(t, err)
	require.NoError(t, machine.SendEvent(ctx, OnHtlcInitiated, nil))
	require.Equal(t, 1, f.server.commits)
	f.reopen(t)
	require.Equal(t, MonitorInvoiceAndHtlcTx, f.swap.GetState())
	f.server.preimage = f.preimage[:]
	machine, err = NewFSM(t.Context(), f.swap, f.cfg, true)
	require.NoError(t, err)
	require.NoError(t, machine.SendEvent(t.Context(), OnRecover, nil))
	f.reopen(t)
	require.Equal(t, Succeeded, f.swap.GetState())
	require.Equal(t, f.preimage, [32]byte(f.swap.SwapPreimage))
	require.Equal(t, deposit.LoopedIn, f.swap.Deposits[0].GetState())
	require.Zero(t, f.driver.refunds)
}

// TestAssetFSMRejectsUnsafePayout ensures native evidence is checked in the
// actual signing action. An interrupted unexposed swap follows normal abort
// recovery instead of inventing another request-replay worker.
func TestAssetFSMRejectsUnsafePayout(t *testing.T) {
	for _, kind := range []string{"wrong group", "underpayment", "short expiry"} {
		t.Run(kind, func(t *testing.T) {
			f := newAssetFSMFixture(t)
			switch kind {
			case "wrong group":
				_, other := btcec.PrivKeyFromBytes([]byte{2})
				f.node.channels[0].CustomChannelData = fmt.Appendf(nil,
					`{"group_key":"%x"}`, other.SerializeCompressed())
			case "underpayment":
				f.node.invoice.Htlcs[0].CustomChannelData = fmt.Appendf(nil,
					`{"rfq_id":"%064x","balances":[{"asset_id":"%x","amount":999}]}`,
					1, f.node.terms.GroupKey[1:])
			case "short expiry":
				f.node.invoice.Htlcs[0].ExpiryHeight = 230
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			machine, err := NewFSM(ctx, f.swap, f.cfg, true)
			require.NoError(t, err)
			require.NoError(t, machine.SendEvent(ctx, OnHtlcInitiated, nil))
			require.Zero(t, f.server.commits)
			f.reopen(t)
			require.Equal(t, SignHtlcTx, f.swap.GetState())
			machine, err = NewFSM(t.Context(), f.swap, f.cfg, true)
			require.NoError(t, err)
			require.NoError(t, machine.SendEvent(t.Context(), OnRecover, nil))
			f.reopen(t)
			require.Equal(t, Failed, f.swap.GetState())
			require.Equal(t, deposit.Deposited, f.swap.Deposits[0].GetState())
		})
	}
}

// TestAssetFSMRefundAfterRestart keeps exposed deposits in the normal timeout
// branch and completes only after the driver returns a spendable refund proof.
func TestAssetFSMRefundAfterRestart(t *testing.T) {
	f := newAssetFSMFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.server.commit = cancel
	machine, err := NewFSM(ctx, f.swap, f.cfg, true)
	require.NoError(t, err)
	require.NoError(t, machine.SendEvent(ctx, OnHtlcInitiated, nil))
	f.reopen(t)
	f.driver.height = 200
	machine, err = NewFSM(t.Context(), f.swap, f.cfg, true)
	require.NoError(t, err)
	require.NoError(t, machine.SendEvent(t.Context(), OnRecover, nil))
	f.reopen(t)
	require.Equal(t, HtlcTimeoutSwept, f.swap.GetState())
	require.Equal(t, deposit.HtlcTimeoutSwept, f.swap.Deposits[0].GetState())
	require.Equal(t, 1, f.driver.refunds)
	require.Equal(t, 1, f.invoices.cancels)
}

type failAssetExposureStore struct {
	StaticAddressLoopInStore
}

func (s failAssetExposureStore) UpdateLoopIn(ctx context.Context,
	swap *StaticAddressLoopIn) error {

	if swap.GetState() == MonitorInvoiceAndHtlcTx {
		return fmt.Errorf("write failed before claim exposure")
	}
	return s.StaticAddressLoopInStore.UpdateLoopIn(ctx, swap)
}

// TestAssetFSMCannotExposeWithoutSavedFallback checks the explicit protocol
// save inside the real action, independently of the informational entry hook.
func TestAssetFSMCannotExposeWithoutSavedFallback(t *testing.T) {
	f := newAssetFSMFixture(t)
	f.cfg.Store = failAssetExposureStore{StaticAddressLoopInStore: f.store}
	machine, err := NewFSM(t.Context(), f.swap, f.cfg, true)
	require.NoError(t, err)
	require.NoError(t, machine.SendEvent(t.Context(), OnHtlcInitiated, nil))
	require.Zero(t, f.server.commits)
	f.reopen(t)
	require.Equal(t, Failed, f.swap.GetState())
	require.Equal(t, deposit.Deposited, f.swap.Deposits[0].GetState())
}

// TestAssetFSMRecoversDepositTransition covers the gap between ordinary deposit
// and swap updates without adding another journal or completion marker.
func TestAssetFSMRecoversDepositTransition(t *testing.T) {
	for _, test := range []struct {
		name         string
		swapState    fsm.StateType
		depositState fsm.StateType
		final        fsm.StateType
	}{
		{"paid", PaymentReceived, deposit.LoopedIn, Succeeded},
		{"refunded", SweepHtlcTimeout, deposit.HtlcTimeoutSwept, HtlcTimeoutSwept},
		{"unlocked", UnlockDeposits, deposit.Deposited, Failed},
		{"before lock", InitHtlcTx, deposit.Deposited, Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAssetFSMFixture(t)
			f.swap.SetState(test.swapState)
			f.swap.Deposits[0].SetState(test.depositState)
			require.NoError(t, deposit.NewSqlStore(f.db.BaseDB).UpdateDeposit(
				t.Context(), f.swap.Deposits[0],
			))
			require.NoError(t, f.store.UpdateLoopIn(t.Context(), f.swap))
			f.reopen(t)
			machine, err := NewFSM(t.Context(), f.swap, f.cfg, true)
			require.NoError(t, err)
			require.NoError(t, machine.SendEvent(t.Context(), OnRecover, nil))
			f.reopen(t)
			require.Equal(t, test.final, f.swap.GetState())
			require.Equal(t, test.depositState, f.swap.Deposits[0].GetState())
			require.Zero(t, f.driver.refunds)
		})
	}
}

// TestAssetRequestReplay returns an existing normal swap before checking its
// already-reserved deposit, but rejects changes to the user's accepted terms.
func TestAssetRequestReplay(t *testing.T) {
	f := newAssetFSMFixture(t)
	f.reopen(t)
	manager := &Manager{cfg: f.cfg}
	request := f.swap.Asset.Request
	replayed, err := manager.initiateAssetLoopIn(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, f.swap.SwapHash, replayed.SwapHash)
	request.Amount--
	_, err = manager.initiateAssetLoopIn(t.Context(), request)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
