package loopin

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
)

// AssetConfig supplies asset services to the normal deposit and Loop In FSMs.
type AssetConfig struct {
	deposit.AssetConfig

	Address   *address.Manager
	Deposits  *deposit.Manager
	Server    AssetServer
	Tapd      tapchannelrpc.TaprootAssetChannelsClient
	Owner     [32]byte
	Peer      [33]byte
	FetchL402 func(context.Context) error
}

// AssetsEnabled reports whether this manager can initiate asset swaps.
func (m *Manager) AssetsEnabled() bool {
	return m != nil && m.cfg != nil && m.cfg.Assets != nil
}

// InitializeAssets validates the node services before ordinary FSM recovery.
func (m *Manager) InitializeAssets(ctx context.Context) error {
	if !m.AssetsEnabled() {
		return nil
	}
	cfg := m.cfg.Assets
	if cfg.Wallet == nil || cfg.Chain == nil || cfg.Address == nil ||
		cfg.Deposits == nil || cfg.Server == nil || cfg.Tapd == nil ||
		m.cfg.LndClient == nil || m.cfg.InvoicesClient == nil ||
		cfg.FetchL402 == nil ||
		cfg.Owner == ([32]byte{}) || cfg.Peer == ([33]byte{}) ||
		cfg.FeeSat == 0 || cfg.MaxFeeSat < cfg.FeeSat || cfg.MinConfs == 0 ||
		m.cfg.Signer == nil || m.cfg.WalletKit == nil {

		return swapkit.ErrInvalid
	}
	cfg.Deposits.ConfigureAssets(&cfg.AssetConfig)
	return nil
}

// CreateAssetDeposit starts funding through the normal deposit manager.
func (m *Manager) CreateAssetDeposit(ctx context.Context, id [32]byte,
	group [33]byte, amount uint64) (*deposit.Deposit, error) {

	if !m.AssetsEnabled() || id == ([32]byte{}) || amount == 0 {
		return nil, swapkit.ErrInvalid
	}
	cfg := m.cfg.Assets
	if _, _, err := cfg.Address.NewAddress(ctx); err != nil {
		return nil, err
	}
	params, err := cfg.Address.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}
	req := swapkit.CreateDepositRequest{
		RequestID: id, GroupKey: group, Amount: amount, CSV: params.Expiry,
		KeyFamily: int32(params.KeyLocator.Family),
		KeyIndex:  params.KeyLocator.Index,
	}
	copy(req.ClientKey[:], params.ClientPubkey.SerializeCompressed())
	copy(req.ServerKey[:], params.ServerPubkey.SerializeCompressed())
	return cfg.Deposits.CreateAssetDeposit(ctx, req)
}

// StartAssetLoopIn sends an asset request through the normal manager loop.
func (m *Manager) StartAssetLoopIn(ctx context.Context,
	request AssetSwapRequest) (*StaticAddressLoopIn, error) {

	if !m.AssetsEnabled() {
		return nil, swapkit.ErrInvalid
	}
	req := &newSwapRequest{
		assetRequest: &request, respChan: make(chan *newSwapResponse, 1),
	}
	select {
	case m.newLoopInChan <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.exitChan:
		return nil, fmt.Errorf("loop-in manager has been canceled")
	}
	select {
	case response := <-req.respChan:
		return response.loopIn, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.exitChan:
		return nil, fmt.Errorf("loop-in manager has been canceled")
	}
}

func (m *Manager) initiateAssetLoopIn(ctx context.Context,
	request AssetSwapRequest) (*StaticAddressLoopIn, error) {

	if request.RequestID == ([32]byte{}) || request.Amount == 0 ||
		request.MaxDebit < request.Amount {

		return nil, swapkit.ErrInvalid
	}
	prior, err := m.GetAllSwaps(ctx)
	if err != nil {
		return nil, err
	}
	for _, swap := range prior {
		if swap.Asset == nil || swap.Asset.Request.RequestID != request.RequestID {
			continue
		}
		if swap.Asset.Request != request {
			return nil, swapkit.ErrInvalid
		}
		return swap, nil
	}
	point, err := wire.NewOutPointFromString(request.Outpoint)
	if err != nil || point.String() != request.Outpoint {
		return nil, swapkit.ErrInvalid
	}
	d, err := m.cfg.Assets.Deposits.AssetDeposit(ctx, request.Outpoint)
	if err != nil {
		return nil, err
	}
	asset := d.AssetSnapshot()
	if asset == nil || !d.IsInState(deposit.Deposited) ||
		!validAssetSwapAmount(asset.Amount, request) ||
		asset.GroupKey != request.GroupKey || asset.ConfirmationHeight == 0 {

		return nil, swapkit.ErrInvalid
	}
	params, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}
	swap := &StaticAddressLoopIn{
		Asset:    &AssetSwapInfo{Request: request},
		Deposits: []*deposit.Deposit{d}, DepositOutpoints: []string{request.Outpoint},
		InitiationHeight: m.currentHeight.Load(), InitiationTime: time.Now(),
		ProtocolVersion: version.ProtocolVersion_V0,
		ClientPubkey:    params.ClientPubkey, ServerPubkey: params.ServerPubkey,
		HtlcKeyLocator: params.KeyLocator,
	}
	return m.startLoopInFsm(ctx, swap)
}

// AssetInventory returns asset deposits from the ordinary deposit store.
func (m *Manager) AssetInventory(ctx context.Context) (
	[]*deposit.Deposit, error) {

	all, err := m.cfg.DepositManager.GetAllDeposits(ctx)
	if err != nil {
		return nil, err
	}
	var result []*deposit.Deposit
	for _, d := range all {
		if d.Asset != nil {
			result = append(result, d)
		}
	}
	return result, nil
}

// RefundAssetDeposit requests the normal deposit FSM's expiry action.
func (m *Manager) RefundAssetDeposit(ctx context.Context, point string) (
	deposit.ID, error) {

	if !m.AssetsEnabled() {
		return deposit.ID{}, swapkit.ErrInvalid
	}
	return m.cfg.Assets.Deposits.RefundAssetDeposit(ctx, point)
}

// assetStates binds asset operations to the normal static Loop In lifecycle.
func (f *FSM) assetStates() fsm.States {
	states := f.LoopInStatesV0()
	init := states[InitHtlcTx]
	init.Action = f.InitAssetHtlcAction
	states[InitHtlcTx] = init
	sign := states[SignHtlcTx]
	sign.Action = f.SignAssetHtlcAction
	states[SignHtlcTx] = sign
	monitor := states[MonitorInvoiceAndHtlcTx]
	monitor.Action = f.MonitorAssetInvoiceAction
	delete(monitor.Transitions, fsm.OnError)
	states[MonitorInvoiceAndHtlcTx] = monitor
	sweep := states[SweepHtlcTimeout]
	sweep.Action = f.SweepAssetHtlcTimeoutAction
	sweep.Transitions[OnHtlcTimeoutSwept] = HtlcTimeoutSwept
	sweep.Transitions[OnPaymentReceived] = PaymentReceived
	delete(sweep.Transitions, fsm.OnError)
	states[SweepHtlcTimeout] = sweep
	paid := states[PaymentReceived]
	paid.Transitions[OnRecover] = PaymentReceived
	states[PaymentReceived] = paid
	unlock := states[UnlockDeposits]
	unlock.Action = f.UnlockAssetDepositsAction
	states[UnlockDeposits] = unlock
	return states
}
