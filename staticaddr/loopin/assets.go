package loopin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	"github.com/lightninglabs/loop/swapserverrpc"
	"google.golang.org/protobuf/proto"
)

// AssetDriver supplies typed asset operations over the normal node services.
// It owns no database, worker, transport, or signing-session journal.
type AssetDriver interface {
	CheckReady(context.Context) error
	Height(context.Context) (uint32, error)
	ValidateOffer(context.Context, swapkit.Offer) error
	PrepareRecovery(context.Context, swapkit.Offer) ([]byte, error)
	BeginFallback(context.Context, swapkit.Offer, []byte) (
		*assetexec.FallbackRound, error)
	CreateDeposit(context.Context, swapkit.CreateDepositRequest,
		*assetkit.DepositFunding, func() error) (swapkit.Deposit, error)
	RefundDeposit(context.Context, swapkit.RefundDepositRequest,
		*assetkit.DepositFunding, func() error) ([]byte, error)
	LookupPreimage(context.Context, swapkit.Operation) (
		swapkit.PreimageResult, error)
	InspectClaim(context.Context, swapkit.Operation) ([]byte, error)
	RecoverRefund(context.Context, swapkit.Operation) ([]byte, error)
}

// AssetConfig supplies node services and the normal inventory/store.
type AssetConfig struct {
	Driver     AssetDriver
	Store      AssetStore
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

// initAssetSession binds the asset protocol driver to the ordinary loop-in
// manager. All request and execution records use its existing SQL database.
func (m *Manager) initAssetSession() error {
	cfg := m.cfg.Assets
	if cfg == nil {
		return nil
	}
	if cfg.Driver == nil || cfg.Store == nil || cfg.Address == nil ||
		cfg.Deposits == nil || cfg.Server == nil || cfg.Receiver == nil ||
		cfg.Owner == ([32]byte{}) || cfg.Peer == ([33]byte{}) ||
		cfg.FetchL402 == nil {

		return assetexec.ErrInvalid
	}
	m.assetCfg = cfg
	actions := &assetActions{manager: m}
	adapter := &assetclient.Adapter{
		Actions: actions, Receiver: cfg.Receiver, Server: cfg.Server,
		Store: cfg.Store, Height: actions.CurrentHeight,
		HoldExpiryDelta: 20,
	}
	m.assetSession = &assetclient.Session{
		Controller: assetexec.NewController(cfg.Store, adapter, time.Now),
		Adapter:    adapter,
	}
	return nil
}

// AssetsEnabled reports whether this manager has a configured asset driver.
func (m *Manager) AssetsEnabled() bool {
	return m != nil && m.assetCfg != nil
}

// AssetStatus returns the execution record used by normal status RPCs.
func (m *Manager) AssetStatus(ctx context.Context, hash [32]byte) (
	*assetexec.Record, error) {

	return m.assetCfg.Store.Get(ctx, hash)
}

// InitializeAssets verifies node identity and restores asset exclusions
// before the Bitcoin deposit manager or public client RPC is started.
func (m *Manager) InitializeAssets(ctx context.Context) error {
	if !m.AssetsEnabled() {
		return nil
	}
	if err := m.assetCfg.Driver.CheckReady(ctx); err != nil {
		return err
	}
	entries, err := m.assetCfg.Store.AssetInventory(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = m.assetCfg.Deposits.RegisterAssetDeposit(
			ctx, entry.Deposit.Outpoint,
			func() error { return nil },
		); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) registerDeposit(ctx context.Context,
	d swapkit.Deposit) error {

	point, err := wire.NewOutPointFromString(d.Outpoint)
	if err != nil || point.String() != d.Outpoint || d.Amount == 0 ||
		len(d.Proof) == 0 || len(d.Contract) == 0 {

		return assetexec.ErrInvalid
	}
	if _, err := btcec.ParsePubKey(d.GroupKey[:]); err != nil {
		return assetexec.ErrInvalid
	}
	return m.assetCfg.Deposits.RegisterAssetDeposit(
		ctx, d.Outpoint, func() error {
			return m.assetCfg.Store.AddAssetDeposit(ctx, d)
		},
	)
}

// CreateAssetDeposit saves a funding request before publishing it.
func (m *Manager) CreateAssetDeposit(ctx context.Context, id [32]byte,
	group [33]byte, amount uint64) error {

	if id == ([32]byte{}) || amount == 0 {
		return assetexec.ErrInvalid
	}
	if _, _, err := m.assetCfg.Address.NewAddress(ctx); err != nil {
		return err
	}
	params, err := m.assetCfg.Address.GetStaticAddressParameters(ctx)
	if err != nil {
		return err
	}
	req := &swapkit.CreateDepositRequest{
		RequestID: id,
		GroupKey:  group,
		Amount:    amount,
		CSV:       params.Expiry,
		KeyFamily: int32(params.KeyLocator.Family),
		KeyIndex:  params.KeyLocator.Index,
	}
	copy(req.ClientKey[:], params.ClientPubkey.SerializeCompressed())
	copy(req.ServerKey[:], params.ServerPubkey.SerializeCompressed())
	return m.assetCfg.Store.PutAssetJob(ctx, &AssetJob{ID: id, Deposit: req})
}

// StartAssetLoopIn saves deposit selection and request bytes before dispatch.
// The first production version requires one whole isolated asset anchor.
func (m *Manager) StartAssetLoopIn(ctx context.Context,
	request AssetSwapRequest) error {

	if request.RequestID == ([32]byte{}) || request.Amount == 0 ||
		request.MaxDebit < request.Amount {

		return assetexec.ErrInvalid
	}
	point, err := wire.NewOutPointFromString(request.Outpoint)
	if err != nil || point.String() != request.Outpoint {
		return assetexec.ErrInvalid
	}
	entries, err := m.assetCfg.Store.AssetInventory(ctx)
	if err != nil {
		return err
	}
	var input *swapkit.Deposit
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
	if !validAssetSwapAmount(input.Amount, request) ||
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
	if proto.Size(rpcRequest) > swapkit.PublicRPCMaxBytes {
		return assetexec.ErrInvalid
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(
		rpcRequest,
	)
	if err != nil {
		return err
	}
	return m.assetCfg.Store.PutAssetJob(ctx, &AssetJob{
		ID: request.RequestID, Swap: &request, ServerRequest: raw,
	})
}

// AssetJobs returns request identities and persisted progress for client RPC.
func (m *Manager) AssetJobs(ctx context.Context) ([]AssetJob, error) {
	return m.assetCfg.Store.AssetJobs(ctx)
}

// AssetInventory returns native deposit metadata and reservation ownership.
func (m *Manager) AssetInventory(ctx context.Context) (
	[]AssetInventoryEntry, error) {

	return m.assetCfg.Store.AssetInventory(ctx)
}

func (m *Manager) resumeAsset(ctx context.Context, j *AssetJob) error {
	if (j.Complete && j.Deposit == nil) || j.Canceled {
		return m.reportRefund(ctx, j)
	}
	if j.Refund != nil || j.Deposit != nil {
		var funding assetkit.DepositFunding
		if len(j.Funding) > 0 {
			if err := json.Unmarshal(j.Funding, &funding); err != nil {
				return err
			}
		}
		save := func() error {
			raw, err := json.Marshal(funding)
			if err != nil {
				return err
			}
			j.Funding = raw
			return m.assetCfg.Store.UpdateAssetJob(ctx, j)
		}
		if j.Refund != nil {
			proof, err := m.assetCfg.Driver.RefundDeposit(
				ctx, *j.Refund, &funding, save,
			)
			if err != nil {
				return err
			}
			return m.assetCfg.Store.FinishAssetRefund(ctx, j, proof)
		}
		result, err := m.assetCfg.Driver.CreateDeposit(
			ctx, *j.Deposit, &funding, save,
		)
		if err != nil {
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
		return m.assetCfg.Store.UpdateAssetJob(ctx, j)
	}
	if j.Offer == nil {
		canceled, err := m.cancelExpiredUnknownOffer(ctx, j)
		if err != nil || canceled {
			return err
		}
		if err := m.assetCfg.FetchL402(ctx); err != nil {
			return err
		}
		var request swapserverrpc.CreateAssetLoopInRequest
		if err := proto.Unmarshal(
			j.ServerRequest, &request,
		); err != nil {
			return err
		}
		offer, err := m.assetSession.Adapter.RequestOffer(
			ctx, m.assetCfg.Owner, &request,
		)
		if err != nil {
			return err
		}
		j.Offer = &offer
		if err = m.assetCfg.Store.UpdateAssetJob(ctx, j); err != nil {
			return err
		}
	}
	if !time.Now().Before(j.Offer.Terms.OfferExpiry) {
		_, err := m.assetCfg.Store.Get(ctx, j.Offer.Hash)
		if errors.Is(err, assetexec.ErrNotFound) {
			if err := m.verifyUnstarted(ctx, j); err != nil {
				return err
			}
			return m.assetCfg.Store.CancelUnstartedAssetJob(ctx, j)
		}
		if err != nil {
			return err
		}
	}
	if len(j.Intent) == 0 {
		recovery, err := m.assetCfg.Driver.PrepareRecovery(ctx, *j.Offer)
		if err != nil {
			return err
		}
		intent, err := json.Marshal(assetclient.InvoiceIntent{
			Peer: m.assetCfg.Peer[:], RecoveryRequest: recovery,
		})
		if err != nil {
			return err
		}
		j.Intent = intent
		if err = m.assetCfg.Store.UpdateAssetJob(ctx, j); err != nil {
			return err
		}
	}
	// A completed controller may outlive a lost final request-journal
	// update.
	record, err := m.assetCfg.Store.Get(ctx, j.Offer.Hash)
	if err != nil && !errors.Is(err, assetexec.ErrNotFound) {
		return err
	}
	if record == nil || !record.State.Terminal() {
		if err = m.assetSession.Controller.Start(
			ctx, *j.Offer, j.Intent,
		); err != nil {
			return err
		}
	}
	j.Complete = true
	return m.assetCfg.Store.UpdateAssetJob(ctx, j)
}

func (m *Manager) recoverAssetRequests(ctx context.Context) error {
	refreshCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	m.report(m.expireDeposits(refreshCtx))
	stop()
	jobs, err := m.assetCfg.Store.AssetJobs(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		actionCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		m.assetRequestMu.Lock()
		err = m.resumeAsset(actionCtx, &jobs[i])
		m.assetRequestMu.Unlock()
		stop()
		m.report(err)
	}
	return nil
}

func (m *Manager) report(err error) {
	if err != nil && !pendingAsset(err) && m.assetCfg.OnError != nil {
		m.assetCfg.OnError(err)
	}
}

// reportRefund retries proof delivery after the client has independently
// recovered. Server availability never delays local cancellation or release.
func (m *Manager) reportRefund(ctx context.Context, job *AssetJob) error {
	if job.Offer == nil {
		return nil
	}
	if job.Canceled {
		return m.reportCancellation(ctx, job)
	}
	record, err := m.assetCfg.Store.Get(ctx, job.Offer.Hash)
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
	_, err = m.assetCfg.Server.ReportAssetLoopInRefund(peerCtx,
		&swapserverrpc.ReportAssetLoopInRefundRequest{
			PaymentHash: job.Offer.Hash[:],
			Proof:       record.RefundEvidence,
		})
	if err != nil {
		return err
	}
	job.RefundReported = true
	return m.assetCfg.Store.UpdateAssetJob(ctx, job)
}

// RefundAssetDeposit reserves an expired deposit before its CSV refund.
// Pending offer requests and active swaps retain ownership until their own
// controller has proved cancellation or a confirmed resolution.
func (m *Manager) RefundAssetDeposit(ctx context.Context, outpoint string) (
	[32]byte, error) {

	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return [32]byte{}, assetexec.ErrInvalid
	}
	id := sha256.Sum256([]byte("Loop asset deposit refund v1:" + outpoint))
	if prior, err := m.assetCfg.Store.GetAssetJob(ctx, id); err == nil {
		if prior.Refund == nil || prior.Refund.Outpoint != outpoint {
			return id, assetexec.ErrConflict
		}
		return id, nil
	} else if !errors.Is(err, assetexec.ErrNotFound) {
		return id, err
	}
	height, err := m.assetCfg.Driver.Height(ctx)
	if err != nil {
		return id, err
	}
	entries, err := m.assetCfg.Store.AssetInventory(ctx)
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
		return id, m.assetCfg.Store.PutAssetJob(ctx, &AssetJob{
			ID: id,
			Refund: &swapkit.RefundDepositRequest{
				Outpoint: outpoint,
			},
		})
	}
	return id, assetexec.ErrNotFound
}

func (m *Manager) expireDeposits(ctx context.Context) error {
	entries, err := m.assetCfg.Store.AssetInventory(ctx)
	if err != nil {
		return err
	}
	height, err := m.assetCfg.Driver.Height(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Consumed || entry.RequestID != ([32]byte{}) ||
			entry.Deposit.RefundHeight == 0 ||
			height < entry.Deposit.RefundHeight {

			continue
		}
		_, err := m.RefundAssetDeposit(ctx, entry.Deposit.Outpoint)
		if err != nil && !errors.Is(err, assetexec.ErrConflict) {
			return err
		}
	}
	return nil
}

func (m *Manager) reportCancellation(ctx context.Context, job *AssetJob) error {
	if job.CancellationReported {
		return nil
	}
	peerCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := m.assetCfg.Server.CancelAssetLoopIn(
		peerCtx,
		&swapserverrpc.AssetLoopInReference{
			PaymentHash: job.Offer.Hash[:],
		},
	)
	if err != nil {
		return err
	}
	job.CancellationReported = true
	return m.assetCfg.Store.UpdateAssetJob(ctx, job)
}

// cancelExpiredUnknownOffer handles an offer response lost before its local
// journal write. Offer persistence precedes every transaction preparation and
// controller call, so this request cannot have exposed claim authority. Keep
// its reservation until the original CSV maturity and verify no active client
// record contradicts that ordering before allowing the original refund.
func (m *Manager) cancelExpiredUnknownOffer(ctx context.Context,
	job *AssetJob) (
	bool, error) {

	entries, err := m.assetCfg.Store.AssetInventory(ctx)
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
	height, err := m.assetCfg.Driver.Height(ctx)
	if err != nil {
		return false, err
	}
	if height < maturity {
		return false, nil
	}
	if err := m.verifyUnstarted(ctx, job); err != nil {
		return false, err
	}
	return true, m.assetCfg.Store.CancelUnstartedAssetJob(ctx, job)
}

func (m *Manager) verifyUnstarted(ctx context.Context, job *AssetJob) error {
	records, err := m.assetCfg.Store.ListActive(ctx)
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

// pendingAsset identifies expected retry boundaries without logging them as
// operational failures.
func pendingAsset(err error) bool {
	return errors.Is(err, assetexec.ErrPending) ||
		errors.Is(err, swapkit.ErrPending)
}
