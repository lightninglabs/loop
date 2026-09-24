package reservation

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/looprpc"
	"google.golang.org/protobuf/proto"
)

var (
	ErrManagerStopped   = errors.New("reservation manager stopped")
	ErrReservationBusy  = errors.New("reservation is busy")
	ErrReservationLimit = errors.New("active reservation limit reached")
)

// ManagerConfig bounds node work and admission. Saved purchases resume even
// when they exceed MaxActiveReservations after a configuration change.
type ManagerConfig struct {
	FSM                   *Config
	PollInterval          time.Duration
	CallTimeout           time.Duration
	MaxActiveReservations int
}

type managerRequest struct {
	ctx       context.Context
	id        ID
	assetID   [32]byte
	amount    uint64
	skipProbe bool
	create    bool
	event     fsm.EventType
	approval  *looprpc.ApproveAssetReservationRequest
	response  chan managerResponse
}

type managerResponse struct {
	reservation *Reservation
	err         error
}

type reservationWorker struct {
	fsm      *FSM
	commands chan *managerRequest
}

// Manager restores purchases and owns one worker per live reservation.
// Its main loop owns the registry; workers serialize actions without holding
// a registry mutex across database, payment, or proof calls.
type Manager struct {
	cfg      ManagerConfig
	requests chan *managerRequest
	finished chan ID
	ready    chan struct{}
	done     chan struct{}
	started  atomic.Bool
}

// NewManager validates the services and limits and prepares the request queues.
// Call Run to restore saved reservations and start processing requests.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	if err := cfg.FSM.Validate(); err != nil {
		return nil, err
	}
	if cfg.PollInterval <= 0 || cfg.CallTimeout <= 0 ||
		cfg.MaxActiveReservations <= 0 {

		return nil, errors.New("invalid reservation manager limits")
	}
	return &Manager{
		cfg:      cfg,
		requests: make(chan *managerRequest),
		finished: make(chan ID),
		ready:    make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// Run restores unfinished reservations and starts their workers before accepting
// requests. MaxActiveReservations limits only new purchases; saved reservations
// always resume. This loop owns the worker registry and routes each command to
// the worker for its reservation.
//
// Run may be called once. Canceling ctx stops the workers and waits for them to
// exit. Their saved progress remains available to the next manager.
func (m *Manager) Run(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return errors.New("reservation manager already started")
	}

	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		close(m.done)
	}()

	readCtx, readCancel := context.WithTimeout(runCtx, m.cfg.CallTimeout)
	reservations, err := m.cfg.FSM.Store.GetReservations(readCtx, StateFilter{
		ActiveOnly: true,
	})
	readCancel()
	if err != nil {
		return err
	}
	active := make(map[ID]*reservationWorker)
	for _, reservation := range reservations {
		worker, err := m.newWorker(reservation)
		if err != nil {
			return err
		}
		active[reservation.ID] = worker
	}

	start := func(id ID, worker *reservationWorker) {
		workers.Go(func() { m.runWorker(runCtx, id, worker) })
	}
	for id, worker := range active {
		start(id, worker)
	}

	// All existing non-final reservations restored. Signal ready to process
	// new reservation requests.
	close(m.ready)

	for {
		select {
		case <-runCtx.Done():
			return runCtx.Err()

		case id := <-m.finished:
			delete(active, id)

		case request := <-m.requests:
			if request.ctx.Err() != nil {
				request.response <- managerResponse{
					err: request.ctx.Err(),
				}
				continue
			}

			// NewPurchase takes this path, including retries of a saved ID.
			// Create a missing reservation or reuse its saved progress,
			// starting a worker only if needed. This request carries no FSM
			// event; approval and recovery commands go below.
			if request.create {
				// Creation and its snapshot read share one timeout.
				// Either the caller or manager shutdown can cancel
				// this work without canceling the caller's context.
				callCtx, callCancel := context.WithTimeout(
					request.ctx, m.cfg.CallTimeout,
				)
				stopCancel := context.AfterFunc(runCtx, callCancel)
				r, err := m.create(callCtx, request, len(active))

				if err == nil && !IsFinal(r.State) &&
					active[r.ID] == nil {

					worker, workerErr := m.newWorker(r)
					if workerErr != nil {
						err = workerErr
					} else {
						active[r.ID] = worker
						start(r.ID, worker)
					}
				}
				// Never return the worker's live reservation to a caller.
				if err == nil {
					r, err = m.Get(callCtx, request.id)
				}
				stopCancel()
				callCancel()
				request.response <- managerResponse{
					reservation: r,
					err:         err,
				}
				continue
			}

			// Route this command to the reservation's existing worker.
			// Missing or finalized reservations have no worker to receive it.
			worker := active[request.id]
			if worker == nil {
				request.response <- managerResponse{
					err: ErrNotFound,
				}
				continue
			}
			select {
			case worker.commands <- request:

			default:
				request.response <- managerResponse{
					err: ErrReservationBusy,
				}
			}
		}
	}
}

// newWorker restores a reservation's FSM and gives it room for one pending
// command. Run starts the worker separately and rejects further commands with
// ErrReservationBusy while that slot is occupied.
func (m *Manager) newWorker(r *Reservation) (*reservationWorker, error) {
	machine, err := NewFSM(m.cfg.FSM, r)
	if err != nil {
		return nil, err
	}
	return &reservationWorker{
		fsm:      machine,
		commands: make(chan *managerRequest, 1),
	}, nil
}

// runWorker resumes one reservation on startup, on each polling tick, and in
// response to commands. It processes one event at a time, bounds each attempt
// with CallTimeout, and answers callers with the resulting saved progress.
//
// The worker keeps running through temporary errors and while a ready
// reservation awaits expiry. It stops when the saved state is final or ctx is
// canceled, then releases its registry slot and answers any pending command.
func (m *Manager) runWorker(ctx context.Context, id ID, w *reservationWorker) {
	defer func() {
		select {
		case m.finished <- id:

		case <-ctx.Done():
		}

		// The registry no longer dispatches here. Answer anything queued
		// just before the final state instead of leaving its caller waiting.
		for {
			select {
			case pending := <-w.commands:
				pending.response <- managerResponse{
					err: ErrNotFound,
				}

			default:
				return
			}
		}
	}()

	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	var request *managerRequest
	for {
		event := OnRecover
		var data fsm.EventContext
		if request != nil {
			event, data = request.event, request.approval
		}
		callCtx, cancel := context.WithTimeout(ctx, m.cfg.CallTimeout)
		err := w.fsm.SendEvent(callCtx, event, data)
		if err == nil {
			err = w.fsm.LastActionError
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			log.ErrorS(ctx, "Reservation action will resume", err,
				"reservation_id", id)
		}
		readCtx, readCancel := context.WithTimeout(ctx, m.cfg.CallTimeout)
		r, readErr := m.cfg.FSM.Store.GetReservation(readCtx, id)
		readCancel()
		if err == nil {
			err = readErr
		}
		if request != nil {
			request.response <- managerResponse{
				reservation: r,
				err:         err,
			}
			request = nil
		}
		if readErr == nil && IsFinal(r.State) {
			return
		}
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:

		case request = <-w.commands:
		}
	}
}

// create reuses a saved reservation when its asset and amount match the request.
// For a new ID, it checks the active limit, derives a client key, and saves the
// request and probe preference before a worker can ask for a quote. Run calls
// this method serially so concurrent requests cannot create duplicate purchases.
func (m *Manager) create(ctx context.Context, request *managerRequest,
	active int) (*Reservation, error) {

	r, err := m.cfg.FSM.Store.GetReservation(ctx, request.id)
	if err == nil {
		if r.AssetID != request.assetID || r.Amount != request.amount {
			return nil, ErrRequestConflict
		}
		return r, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if active >= m.cfg.MaxActiveReservations {
		return nil, ErrReservationLimit
	}
	key, err := m.cfg.FSM.Wallet.DeriveKey(ctx)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, errors.New("missing reservation client key")
	}
	r = &Reservation{
		ID:        request.id,
		ClientKey: *key,
		State:     RequestQuote,
		Terms: Terms{
			AssetID: request.assetID,
			Amount:  request.amount,
		},
		SkipProbe: request.skipProbe,
	}
	if err := m.cfg.FSM.Store.CreateReservation(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// WaitInitComplete waits until saved reservations have workers and Run can
// accept requests. Individual workers may still be recovering their purchases.
// Waiting ends if ctx is canceled or the manager stops before becoming ready.
func (m *Manager) WaitInitComplete(ctx context.Context) error {
	select {
	case <-m.done:
		return ErrManagerStopped

	case <-ctx.Done():
		return ctx.Err()

	case <-m.ready:
		return nil
	}
}

// submit waits for initialization, hands the request to Run, and waits for its
// reply. A buffered reply lets the worker finish if the caller stops waiting.
// Once a command is queued, canceling the caller's context may end this wait
// while the worker still processes the command under the manager's context.
func (m *Manager) submit(request *managerRequest) (*Reservation, error) {
	if err := m.WaitInitComplete(request.ctx); err != nil {
		return nil, err
	}
	request.response = make(chan managerResponse, 1)
	select {
	case <-request.ctx.Done():
		return nil, request.ctx.Err()

	case <-m.done:
		return nil, ErrManagerStopped

	case m.requests <- request:
	}
	select {
	case <-request.ctx.Done():
		return nil, request.ctx.Err()

	case <-m.done:
		return nil, ErrManagerStopped

	case result := <-request.response:
		return result.reservation, result.err
	}
}

// NewPurchase validates and saves a purchase, then returns its saved progress
// while a worker requests the quote. Reuse id after a lost response to retrieve
// the same reservation and client key; changing its asset or amount is rejected.
// skipProbe applies only to a new purchase. Payment requires separate approval.
func (m *Manager) NewPurchase(ctx context.Context, id ID, assetID [32]byte,
	amount uint64, skipProbe bool) (*Reservation, error) {

	if id == (ID{}) || assetID == ([32]byte{}) || amount == 0 ||
		amount > math.MaxInt64 {

		return nil, errors.New("invalid reservation request")
	}
	return m.submit(&managerRequest{
		ctx:       ctx,
		id:        id,
		assetID:   assetID,
		amount:    amount,
		create:    true,
		skipProbe: skipProbe,
	})
}

// Approve copies the caller's consent and sends it to the reservation's worker.
// The FSM checks the quote hash and payment choices, then saves those choices
// with the state that permits payment. The reply reports progress after that
// attempt; the worker continues resolving payment and delivery afterward.
func (m *Manager) Approve(ctx context.Context, id ID,
	approval *looprpc.ApproveAssetReservationRequest) (*Reservation, error) {

	if approval == nil {
		return nil, errors.New("missing quote approval")
	}
	request := proto.Clone(approval).(*looprpc.ApproveAssetReservationRequest)
	return m.submit(&managerRequest{
		ctx:      ctx,
		id:       id,
		event:    OnApprove,
		approval: request,
	})
}

// Wake asks the worker to resume its current step immediately through OnRecover.
// Notifications trigger the same checks as periodic recovery; the FSM reads
// payment and delivery facts from its services before advancing.
func (m *Manager) Wake(ctx context.Context, id ID) (*Reservation, error) {
	return m.submit(&managerRequest{
		ctx:   ctx,
		id:    id,
		event: OnRecover,
	})
}

// Get loads one reservation from storage within CallTimeout. The returned
// snapshot is independent of the worker and may include a final state.
func (m *Manager) Get(ctx context.Context, id ID) (*Reservation, error) {
	readCtx, cancel := context.WithTimeout(ctx, m.cfg.CallTimeout)
	defer cancel()
	return m.cfg.FSM.Store.GetReservation(readCtx, id)
}

// List loads saved reservations matching the filter within CallTimeout. An
// empty filter includes final purchases. It reads storage directly,
// independently of the live worker registry.
func (m *Manager) List(ctx context.Context, filter StateFilter) (
	[]*Reservation, error) {

	readCtx, cancel := context.WithTimeout(ctx, m.cfg.CallTimeout)
	defer cancel()
	return m.cfg.FSM.Store.GetReservations(readCtx, filter)
}
