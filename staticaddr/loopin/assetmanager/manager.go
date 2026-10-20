// Package assetmanager assembles the normal client's durable asset inventory,
// request creation, shared deposit exclusion, and existing session recovery.
package assetmanager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
	"google.golang.org/protobuf/proto"
)

// Runtime is the authenticated local, versioned SDK process boundary.
type Runtime interface {
	Call(context.Context, bridge.Method, any, any) error
}

// Config contains concrete node, journal and shared inventory dependencies.
type Config struct {
	Runtime    Runtime
	Store      *Store
	Journal    assetexec.Store
	Address    *address.Manager
	Deposits   *deposit.Manager
	Server     assetclient.Server
	Receiver   *assetln.Receiver
	Owner      [32]byte
	Peer       [33]byte
	Network    string
	NodePubkey [33]byte
	FetchL402  func(context.Context) error
	OnError    func(error)
}

// Manager coordinates immutable requests before offers and existing sessions
// after offers. Its worker must stop before either journal is closed.
type Manager struct {
	cfg       Config
	Session   *assetclient.Session
	requestMu sync.Mutex
}

// New assembles the production client actions without any in-memory signer.
func New(cfg Config) (*Manager, error) {
	if cfg.Runtime == nil || cfg.Store == nil || cfg.Journal == nil ||
		cfg.Address == nil || cfg.Deposits == nil ||
		cfg.Server == nil ||
		cfg.Receiver == nil || cfg.Owner == ([32]byte{}) ||
		cfg.Peer == ([33]byte{}) || cfg.FetchL402 == nil {

		return nil, assetexec.ErrInvalid
	}
	m := &Manager{cfg: cfg}
	actions := &runtimeActions{manager: m}
	adapter := &assetclient.Adapter{
		Actions:         actions,
		Receiver:        cfg.Receiver,
		Server:          cfg.Server,
		Store:           cfg.Journal,
		Height:          actions.CurrentHeight,
		HoldExpiryDelta: 20,
	}
	controller := assetexec.NewController(cfg.Journal, adapter, time.Now)
	m.Session = &assetclient.Session{
		Controller: controller,
		Adapter:    adapter,
	}
	return m, nil
}

// Initialize verifies the local runtime identity and restores asset exclusions
// before the Bitcoin deposit manager or public client RPC is started.
func (m *Manager) Initialize(ctx context.Context) error {
	var ready bridge.Ready
	if err := m.cfg.Runtime.Call(
		ctx, bridge.CheckReady, struct{}{}, &ready,
	); err != nil {
		return err
	}
	if ready.Network != m.cfg.Network ||
		ready.LndPubkey != m.cfg.NodePubkey {

		return assetexec.ErrInvalid
	}
	entries, err := m.cfg.Store.Inventory(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = m.cfg.Deposits.RegisterAssetDeposit(
			ctx, entry.Deposit.Outpoint,
			func() error { return nil },
		); err != nil {
			return err
		}
	}
	return m.syncInventory(ctx)
}

func (m *Manager) syncInventory(ctx context.Context) error {
	var deposits []bridge.Deposit
	if err := m.cfg.Runtime.Call(
		ctx, bridge.Inventory, bridge.InventoryRequest{}, &deposits,
	); err != nil {
		return err
	}
	for _, d := range deposits {
		if err := m.registerDeposit(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) registerDeposit(ctx context.Context, d bridge.Deposit) error {
	point, err := wire.NewOutPointFromString(d.Outpoint)
	if err != nil || point.String() != d.Outpoint || d.Amount == 0 ||
		len(d.Proof) == 0 || len(d.Contract) == 0 {

		return assetexec.ErrInvalid
	}
	if _, err := btcec.ParsePubKey(d.GroupKey[:]); err != nil {
		return assetexec.ErrInvalid
	}
	return m.cfg.Deposits.RegisterAssetDeposit(
		ctx, d.Outpoint, func() error {
			return m.cfg.Store.addDeposit(d)
		},
	)
}

// CreateDeposit persists a funding request before the runtime can publish it.
func (m *Manager) CreateDeposit(ctx context.Context, id [32]byte,
	group [33]byte, amount uint64) error {

	if id == ([32]byte{}) || amount == 0 {
		return assetexec.ErrInvalid
	}
	if _, _, err := m.cfg.Address.NewAddress(ctx); err != nil {
		return err
	}
	params, err := m.cfg.Address.GetStaticAddressParameters(ctx)
	if err != nil {
		return err
	}
	req := &bridge.CreateDepositRequest{
		RequestID: id,
		GroupKey:  group,
		Amount:    amount,
		CSV:       params.Expiry,
		KeyFamily: int32(params.KeyLocator.Family),
		KeyIndex:  params.KeyLocator.Index,
	}
	copy(req.ClientKey[:], params.ClientPubkey.SerializeCompressed())
	copy(req.ServerKey[:], params.ServerPubkey.SerializeCompressed())
	return m.cfg.Store.PutJob(ctx, &Job{ID: id, Deposit: req})
}

// Start records exact deposit selection and request bytes before networking.
// The first production version requires one whole isolated asset anchor.
func (m *Manager) Start(ctx context.Context, request SwapRequest) error {
	if request.RequestID == ([32]byte{}) || request.Amount == 0 ||
		request.MaxDebit < request.Amount {

		return assetexec.ErrInvalid
	}
	point, err := wire.NewOutPointFromString(request.Outpoint)
	if err != nil || point.String() != request.Outpoint {
		return assetexec.ErrInvalid
	}
	entries, err := m.cfg.Store.Inventory(ctx)
	if err != nil {
		return err
	}
	var input *bridge.Deposit
	for _, entry := range entries {
		if entry.Deposit.Outpoint == request.Outpoint {
			d := entry.Deposit
			input = &d
			break
		}
	}
	if input == nil {
		return assetexec.ErrNotFound
	}
	if !validSwapAmount(input.Amount, request) ||
		input.GroupKey != request.GroupKey ||
		input.ConfirmationHeight == 0 {

		return assetexec.ErrInvalid
	}
	rpcRequest := &swapserverrpc.CreateAssetLoopInRequest{
		ProtocolVersion: 1, RequestId: request.RequestID[:],
		GroupKey: request.GroupKey[:], PayoutAmount: request.Amount,
		MaxAssetDebit: request.MaxDebit, MaxAssetFee: request.MaxFee,
		Contract: input.Contract,
		Deposits: []*swapserverrpc.AssetDepositInput{{
			Txid:        point.Hash[:],
			OutputIndex: point.Index,
			Proof:       input.Proof,
		}},
	}
	if proto.Size(rpcRequest) > bridge.PublicRPCMaxBytes {
		return assetexec.ErrInvalid
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(
		rpcRequest,
	)
	if err != nil {
		return err
	}
	return m.cfg.Store.PutJob(ctx, &Job{
		ID: request.RequestID, Swap: &request, ServerRequest: raw,
	})
}

// Jobs returns request identities and their persisted progress for client RPC.
func (m *Manager) Jobs(ctx context.Context) ([]Job, error) {
	return m.cfg.Store.Jobs(ctx)
}

// Inventory returns native deposit metadata and reservation ownership.
func (m *Manager) Inventory(ctx context.Context) ([]InventoryEntry, error) {
	return m.cfg.Store.Inventory(ctx)
}

func (m *Manager) resume(ctx context.Context, j *Job) error {
	if j.Complete || j.Canceled {
		return m.reportRefund(ctx, j)
	}
	if j.Refund != nil {
		var proof []byte
		err := m.cfg.Runtime.Call(
			ctx, bridge.RefundDeposit, *j.Refund, &proof,
		)
		if err != nil {
			return err
		}
		return m.cfg.Store.finishRefund(ctx, j, proof)
	}
	if j.Deposit != nil {
		var result bridge.Deposit
		if err := m.cfg.Runtime.Call(
			ctx, bridge.CreateDeposit, *j.Deposit, &result,
		); err != nil {
			return err
		}
		if result.GroupKey != j.Deposit.GroupKey ||
			result.Amount != j.Deposit.Amount {

			return assetexec.ErrInvalid
		}
		if err := m.registerDeposit(ctx, result); err != nil {
			return err
		}
		j.Complete = true
		return m.cfg.Store.updateJob(ctx, j)
	}
	if j.Offer == nil {
		canceled, err := m.cancelExpiredUnknownOffer(ctx, j)
		if err != nil || canceled {
			return err
		}
		if err := m.cfg.FetchL402(ctx); err != nil {
			return err
		}
		var request swapserverrpc.CreateAssetLoopInRequest
		if err := proto.Unmarshal(
			j.ServerRequest, &request,
		); err != nil {
			return err
		}
		offer, err := m.Session.Adapter.RequestOffer(
			ctx, m.cfg.Owner, &request,
		)
		if err != nil {
			return err
		}
		j.Offer = &offer
		if err = m.cfg.Store.updateJob(ctx, j); err != nil {
			return err
		}
	}
	if !time.Now().Before(j.Offer.Terms.OfferExpiry) {
		_, err := m.cfg.Journal.Get(ctx, j.Offer.Hash)
		if errors.Is(err, assetexec.ErrNotFound) {
			if err := m.verifyUnstarted(ctx, j); err != nil {
				return err
			}
			return m.cfg.Store.cancelUnstarted(ctx, j)
		}
		if err != nil {
			return err
		}
	}
	if len(j.Intent) == 0 {
		var recovery []byte
		operation, err := operation(*j.Offer)
		if err != nil {
			return err
		}
		if err = m.cfg.Runtime.Call(
			ctx, bridge.PrepareRecovery, operation, &recovery,
		); err != nil {
			return err
		}
		j.Intent, err = json.Marshal(assetclient.InvoiceIntent{
			Peer: m.cfg.Peer[:], RecoveryRequest: recovery,
		})
		if err != nil {
			return err
		}
		if err = m.cfg.Store.updateJob(ctx, j); err != nil {
			return err
		}
	}
	// A completed controller may outlive a lost final request-journal
	// update.
	record, err := m.cfg.Journal.Get(ctx, j.Offer.Hash)
	if err != nil && !errors.Is(err, assetexec.ErrNotFound) {
		return err
	}
	if record == nil || !record.State.Terminal() {
		if err = m.Session.Controller.Start(
			ctx, *j.Offer, j.Intent,
		); err != nil {
			return err
		}
	}
	j.Complete = true
	return m.cfg.Store.updateJob(ctx, j)
}

// Run recovers pending creation requests and existing swaps until shutdown.
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	initialized := make(chan struct{})
	close(initialized)
	result := make(chan error, 1)
	go func() {
		result <- m.Session.Run(ctx, assetclient.WorkerConfig{
			Initialized: initialized, Interval: time.Second,
			ActionTimeout: 30 * time.Second,
			Concurrency:   4,
			OnError: func(_ [32]byte, err error) {
				m.report(err)
			},
		})
	}()
	defer func() { cancel(); <-result }()
	for {
		refreshCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		m.report(m.syncInventory(refreshCtx))
		m.report(m.expireDeposits(refreshCtx))
		stop()
		jobs, err := m.cfg.Store.Jobs(ctx)
		if err != nil {
			return err
		}
		for i := range jobs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			actionCtx, stop := context.WithTimeout(
				ctx,
				30*time.Second,
			)
			m.requestMu.Lock()
			err = m.resume(actionCtx, &jobs[i])
			m.requestMu.Unlock()
			stop()
			m.report(err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-result:
			timer.Stop()
			result <- err
			return fmt.Errorf("asset recovery worker: %w", err)
		case <-timer.C:
		}
	}
}

func (m *Manager) report(err error) {
	if err != nil && !pending(err) && m.cfg.OnError != nil {
		m.cfg.OnError(err)
	}
}

// reportRefund retries proof delivery after the client has independently
// recovered. Server availability never delays local cancellation or release.
func (m *Manager) reportRefund(ctx context.Context, job *Job) error {
	if job.Offer == nil {
		return nil
	}
	if job.Canceled {
		return m.reportCancellation(ctx, job)
	}
	record, err := m.cfg.Journal.Get(ctx, job.Offer.Hash)
	if err == nil && record.State == assetexec.Failed {
		return m.reportCancellation(ctx, job)
	}
	if job.RefundReported {
		return nil
	}
	if err != nil || record.State != assetexec.HtlcTimeoutSwept {
		return err
	}
	if len(record.RefundEvidence) == 0 {
		return assetexec.ErrInvalid
	}
	peerCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err = m.cfg.Server.ReportAssetLoopInRefund(peerCtx,
		&swapserverrpc.ReportAssetLoopInRefundRequest{
			PaymentHash: job.Offer.Hash[:],
			Proof:       record.RefundEvidence,
		})
	if err != nil {
		return err
	}
	job.RefundReported = true
	return m.cfg.Store.updateJob(ctx, job)
}

// RefundDeposit reserves an expired original deposit before its CSV refund.
// Pending offer requests and active swaps retain ownership until their own
// controller has proved cancellation or a confirmed resolution.
func (m *Manager) RefundDeposit(ctx context.Context, outpoint string) (
	[32]byte, error) {

	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return [32]byte{}, assetexec.ErrInvalid
	}
	id := sha256.Sum256([]byte("Loop asset deposit refund v1:" + outpoint))
	if prior, err := m.cfg.Store.getJob(ctx, id); err == nil {
		if prior.Refund == nil || prior.Refund.Outpoint != outpoint {
			return id, assetexec.ErrConflict
		}
		return id, nil
	} else if !errors.Is(err, assetexec.ErrNotFound) {
		return id, err
	}
	var height uint32
	if err := m.cfg.Runtime.Call(
		ctx, bridge.Height, struct{}{}, &height,
	); err != nil {
		return id, err
	}
	entries, err := m.cfg.Store.Inventory(ctx)
	if err != nil {
		return id, err
	}
	for _, entry := range entries {
		if entry.Deposit.Outpoint != outpoint {
			continue
		}
		if entry.Deposit.RefundHeight == 0 ||
			height < entry.Deposit.RefundHeight {

			return id, assetexec.ErrPending
		}
		return id, m.cfg.Store.PutJob(ctx, &Job{
			ID: id,
			Refund: &bridge.RefundDepositRequest{
				Outpoint: outpoint,
			},
		})
	}
	return id, assetexec.ErrNotFound
}

func (m *Manager) expireDeposits(ctx context.Context) error {
	entries, err := m.cfg.Store.Inventory(ctx)
	if err != nil {
		return err
	}
	var height uint32
	if err := m.cfg.Runtime.Call(
		ctx, bridge.Height, struct{}{}, &height,
	); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Consumed || entry.RequestID != ([32]byte{}) ||
			entry.Deposit.RefundHeight == 0 ||
			height < entry.Deposit.RefundHeight {

			continue
		}
		_, err := m.RefundDeposit(ctx, entry.Deposit.Outpoint)
		if err != nil && !errors.Is(err, assetexec.ErrConflict) {
			return err
		}
	}
	return nil
}

func (m *Manager) reportCancellation(ctx context.Context, job *Job) error {
	if job.CancellationReported {
		return nil
	}
	peerCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := m.cfg.Server.CancelAssetLoopIn(
		peerCtx,
		&swapserverrpc.AssetLoopInReference{
			PaymentHash: job.Offer.Hash[:],
		},
	)
	if err != nil {
		return err
	}
	job.CancellationReported = true
	return m.cfg.Store.updateJob(ctx, job)
}

// cancelExpiredUnknownOffer handles an offer response lost before its local
// journal write. Offer persistence precedes every runtime preparation and
// controller call, so this request cannot have exposed claim authority. Keep
// its reservation until the original CSV maturity and verify no active client
// record contradicts that ordering before allowing the original refund.
func (m *Manager) cancelExpiredUnknownOffer(ctx context.Context, job *Job) (
	bool, error) {

	entries, err := m.cfg.Store.Inventory(ctx)
	if err != nil {
		return false, err
	}
	var maturity uint32
	for _, entry := range entries {
		if entry.Deposit.Outpoint == job.Swap.Outpoint {
			maturity = entry.Deposit.RefundHeight
			break
		}
	}
	if maturity == 0 {
		return false, nil
	}
	var height uint32
	if err := m.cfg.Runtime.Call(
		ctx, bridge.Height, struct{}{}, &height,
	); err != nil {
		return false, err
	}
	if height < maturity {
		return false, nil
	}
	if err := m.verifyUnstarted(ctx, job); err != nil {
		return false, err
	}
	return true, m.cfg.Store.cancelUnstarted(ctx, job)
}

func (m *Manager) verifyUnstarted(ctx context.Context, job *Job) error {
	records, err := m.cfg.Journal.ListActive(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record == nil || record.Offer.Terms.RequestID == job.ID {
			return assetexec.ErrState
		}
		if slices.Contains(record.Offer.Terms.DepositOutpoints,
			job.Swap.Outpoint) {

			return assetexec.ErrState
		}
	}
	return nil
}
