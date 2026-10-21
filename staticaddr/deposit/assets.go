package deposit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapkit"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
)

// AssetConfig supplies the SDK wallet and chain checks to the deposit FSM.
// Signing uses the manager's ordinary WalletKit and Signer services.
type AssetConfig struct {
	Wallet    *sdk.Wallet
	Chain     assetkit.Chain
	FeeSat    uint64
	MaxFeeSat uint64
	MinConfs  uint32
}

// ConfigureAssets installs SDK services before deposit recovery starts.
func (m *Manager) ConfigureAssets(cfg *AssetConfig) {
	m.cfg.Asset = cfg
}

// CreateAssetDeposit saves the completed funding package before publication.
// A repeated ID retrieves that ordinary deposit record; unfinished building
// has no publication effect and can be retried from scratch.
func (m *Manager) CreateAssetDeposit(ctx context.Context,
	request swapkit.CreateDepositRequest) (*Deposit, error) {

	if m.cfg.Asset == nil || request.RequestID == ([32]byte{}) {
		return nil, swapkit.ErrInvalid
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	id := ID(request.RequestID)
	prior, err := m.cfg.Store.GetDeposit(ctx, id)
	if err == nil {
		var policy swapkit.Registration
		if prior.Asset == nil || prior.Asset.GroupKey != request.GroupKey ||
			prior.Asset.Amount != request.Amount ||
			json.Unmarshal(prior.Asset.Contract, &policy) != nil ||
			policy.ClientKey != request.ClientKey ||
			policy.ServerKey != request.ServerKey || policy.CSV != request.CSV {

			return nil, swapkit.ErrInvalid
		}
		if !prior.IsInFinalState() {
			m.mu.Lock()
			active := m.activeDeposits[prior.OutPoint] != nil
			m.mu.Unlock()
			if !active {
				if err := m.resumeAssetDeposit(prior); err != nil {
					return nil, err
				}
			}
		}
		return prior, nil
	}
	if !errors.Is(err, ErrDepositNotFound) && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	asset, tx, err := m.buildAssetDeposit(ctx, request)
	if err != nil {
		return nil, err
	}
	point, err := wire.NewOutPointFromString(asset.Outpoint)
	if err != nil || point.String() != asset.Outpoint ||
		asset.ValueSat > math.MaxInt64 || len(tx) == 0 {

		return nil, swapkit.ErrInvalid
	}
	d := &Deposit{
		ID: id, OutPoint: *point, state: PublishAssetDeposit,
		Value: btcutil.Amount(asset.ValueSat), Asset: &asset,
		AssetTx: tx, TimeOutSweepPkScript: []byte{},
	}
	if err := m.cfg.Store.CreateDeposit(ctx, d); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.deposits[d.OutPoint] = d
	m.mu.Unlock()
	runCtx := m.runCtx
	if runCtx == nil {
		runCtx = ctx
	}
	if err := m.startDepositFsm(runCtx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// resumeAssetDeposit restores a saved record if creation lost its live FSM.
func (m *Manager) resumeAssetDeposit(d *Deposit) error {
	ctx := m.runCtx
	if ctx == nil {
		return errors.New("deposit manager is not running")
	}
	sm, err := NewFSM(ctx, d, m.cfg, m.finalizedDepositChan, true)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.deposits[d.OutPoint], m.activeDeposits[d.OutPoint] = d, sm
	m.mu.Unlock()
	go func() { _ = sm.SendEvent(ctx, OnRecover, nil) }()
	return nil
}

// AssetDeposit returns an active asset deposit owned by the ordinary manager.
func (m *Manager) AssetDeposit(ctx context.Context, outpoint string) (
	*Deposit, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return nil, swapkit.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sm := m.activeDeposits[*point]
	if sm == nil || sm.deposit.Asset == nil {
		return nil, ErrDepositNotFound
	}
	return sm.deposit, nil
}

// RefundAssetDeposit sends the ordinary expiry event for an available asset.
func (m *Manager) RefundAssetDeposit(ctx context.Context, outpoint string) (
	ID, error) {

	d, err := m.AssetDeposit(ctx, outpoint)
	if err != nil {
		return ID{}, err
	}
	d.Lock()
	ready := d.state == Deposited &&
		d.Asset.RefundHeight > 0 &&
		m.currentHeight.Load() >= d.Asset.RefundHeight
	id := d.ID
	d.Unlock()
	if !ready {
		return id, fmt.Errorf("asset deposit is unavailable for refund")
	}
	m.mu.Lock()
	sm := m.activeDeposits[d.OutPoint]
	m.mu.Unlock()
	runCtx := m.runCtx
	if runCtx == nil {
		runCtx = ctx
	}
	go func() { _ = sm.SendEvent(runCtx, OnExpiry, nil) }()
	return id, sm.DefaultObserver.WaitForState(
		ctx, DefaultTransitionTimeout, PublishExpirySweep,
	)
}

// PublishAssetDepositAction resumes the saved funding package after restart.
func (f *FSM) PublishAssetDepositAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	err := f.waitAssetOperation(ctx, "funding", func() error {
		d := f.deposit
		d.Lock()
		asset, tx := *d.Asset, d.AssetTx
		d.Unlock()
		result, err := f.publishAssetDeposit(ctx, asset, tx)
		if err != nil {
			return err
		}
		d.Lock()
		defer d.Unlock()
		previous, height, state := *d.Asset, d.ConfirmationHeight, d.state
		*d.Asset, d.AssetTx, d.state = result, nil, Deposited
		d.ConfirmationHeight = int64(result.ConfirmationHeight)
		err = f.cfg.Store.UpdateDeposit(ctx, d)
		if err != nil {
			*d.Asset, d.AssetTx, d.state = previous, tx, state
			d.ConfirmationHeight = height
		}
		return err
	})
	if err != nil {
		return fsm.NoOp
	}
	return OnStart
}

func (f *FSM) publishAssetExpiry(ctx context.Context) fsm.EventType {
	err := f.waitAssetOperation(ctx, "refund", func() error {
		d := f.deposit
		d.Lock()
		asset, tx := *d.Asset, d.AssetTx
		d.Unlock()
		if len(tx) == 0 {
			var err error
			tx, err = f.buildAssetRefund(ctx, asset)
			if err != nil {
				return err
			}
			d.Lock()
			d.AssetTx = tx
			err = f.cfg.Store.UpdateDeposit(ctx, d)
			if err != nil {
				d.AssetTx = nil
			}
			d.Unlock()
			if err != nil {
				return err
			}
		}
		_, err := f.publishAssetRefund(ctx, asset, tx)
		return err
	})
	if err != nil {
		return fsm.NoOp
	}
	return OnExpiryPublished
}

// waitAssetOperation waits inside the ordinary FSM action because tapd proof
// export can finish after the block notification that confirmed a transfer.
func (f *FSM) waitAssetOperation(ctx context.Context, operation string,
	action func() error) error {

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := action()
		if err == nil {
			return nil
		}
		if !errors.Is(err, swapkit.ErrPending) {
			f.Errorf("asset %s: %v", operation, err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
