package reservation

import (
	"context"
	"errors"
	"sync"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/looprpc"
)

// States name the action resumed after a restart.
const (
	RequestQuote       fsm.StateType = "RequestQuote"
	QuoteFailed        fsm.StateType = "QuoteFailed"
	QuoteRejected      fsm.StateType = "QuoteRejected"
	ProbeRoutes        fsm.StateType = "ProbeRoutes"
	AwaitApproval      fsm.StateType = "AwaitApproval"
	PayPrepay          fsm.StateType = "PayPrepay"
	WaitForDelivery    fsm.StateType = "WaitForDelivery"
	VerifyReservation  fsm.StateType = "VerifyReservation"
	Ready              fsm.StateType = "Ready"
	CancelPrepay       fsm.StateType = "CancelPrepay"
	Canceled           fsm.StateType = "Canceled"
	Expired            fsm.StateType = "Expired"
	NeedAdminAttention fsm.StateType = "NeedAdminAttention"
)

// Events cannot bypass the checks in their destination action.
const (
	OnRecover       fsm.EventType = "OnRecover"
	OnQuote         fsm.EventType = "OnQuote"
	OnQuoteFailed   fsm.EventType = "OnQuoteFailed"
	OnQuoteRejected fsm.EventType = "OnQuoteRejected"
	OnProbed        fsm.EventType = "OnProbed"
	OnApprove       fsm.EventType = "OnApprove"
	OnApproved      fsm.EventType = "OnApproved"
	OnPaid          fsm.EventType = "OnPaid"
	OnDelivery      fsm.EventType = "OnDelivery"
	OnProof         fsm.EventType = "OnProof"
	OnCancel        fsm.EventType = "OnCancel"
	OnCanceled      fsm.EventType = "OnCanceled"
	OnTimeout       fsm.EventType = "OnTimeout"
)

// FSM saves state on entry and financial facts before node calls, following
// static-address Loop In. Its manager owns the live reservation.
type FSM struct {
	machine         *fsm.StateMachine
	eventMu         sync.Mutex
	cfg             *Config
	reservation     *Reservation
	persistErr      error
	event           fsm.EventType
	readyVerified   bool
	adminNotified   bool
	LastActionError error
}

// NewFSM reconstructs a saved state without making node calls.
func NewFSM(cfg *Config, reservation *Reservation) (*FSM, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := reservation.Validate(); err != nil {
		return nil, err
	}
	f := &FSM{
		cfg:         cfg,
		reservation: reservation,
	}
	if err := f.restore(reservation); err != nil {
		return nil, err
	}
	return f, nil
}

// restore rebuilds the in-memory state machine from the supplied reservation.
// The caller supplies saved facts; restore does not read or write the database.
//
// For a new purchase, the manager has already stored the reservation in
// RequestQuote. For an existing purchase, r.State selects the last saved step,
// preserving its quote, payment choices, and payment or delivery facts. The
// manager sends OnRecover afterward to start or resume that step's action.
//
// After a failed write, SendEvent reloads the reservation and calls restore to
// discard any uncommitted in-memory progress before handling the next event.
// A successful restore clears persistErr and reconnects the state-entry hook.
// It runs no action and makes no node calls itself.
func (f *FSM) restore(r *Reservation) error {
	if err := r.Validate(); err != nil {
		return err
	}
	states := f.states()
	if _, ok := states[r.State]; !ok {
		return errors.New("unknown client reservation state")
	}
	f.reservation = r
	f.machine = fsm.NewStateMachineWithState(states, r.State, 0)
	f.machine.ActionEntryFunc = f.enterState
	f.persistErr = nil
	return nil
}

// SendEvent reloads durable facts after a failed write. The shared FSM's
// entry hook cannot veto its in-memory transition, so that state is discarded.
func (f *FSM) SendEvent(ctx context.Context, event fsm.EventType,
	data fsm.EventContext) error {

	f.eventMu.Lock()
	defer f.eventMu.Unlock()
	if f.persistErr != nil {
		r, err := f.cfg.Store.GetReservation(ctx, f.reservation.ID)
		if err != nil {
			return err
		}
		if err := f.restore(r); err != nil {
			return err
		}
	}
	f.LastActionError = nil
	return f.machine.SendEvent(ctx, event, data)
}

func (f *FSM) states() fsm.States {
	return fsm.States{
		RequestQuote: {
			Action: f.RequestQuoteAction,
			Transitions: fsm.Transitions{
				OnRecover:       RequestQuote,
				OnQuote:         ProbeRoutes,
				OnQuoteFailed:   QuoteFailed,
				OnQuoteRejected: QuoteRejected,
				OnCancel:        CancelPrepay,
				fsm.OnError:     NeedAdminAttention,
			},
		},
		ProbeRoutes: {
			Action: f.ProbeRoutesAction,
			Transitions: fsm.Transitions{
				OnRecover:   ProbeRoutes,
				OnProbed:    AwaitApproval,
				OnCancel:    CancelPrepay,
				fsm.OnError: NeedAdminAttention,
			},
		},
		AwaitApproval: {
			Action: f.AwaitApprovalAction,
			Transitions: fsm.Transitions{
				OnRecover:   AwaitApproval,
				OnApprove:   AwaitApproval,
				OnApproved:  PayPrepay,
				OnCancel:    CancelPrepay,
				fsm.OnError: NeedAdminAttention,
			},
		},
		PayPrepay: {
			Action: f.PayPrepayAction,
			Transitions: fsm.Transitions{
				OnRecover:   PayPrepay,
				OnPaid:      WaitForDelivery,
				OnCancel:    CancelPrepay,
				fsm.OnError: NeedAdminAttention,
			},
		},
		WaitForDelivery: {
			Action: f.WaitForDeliveryAction,
			Transitions: fsm.Transitions{
				OnRecover:   WaitForDelivery,
				OnDelivery:  VerifyReservation,
				fsm.OnError: NeedAdminAttention,
			},
		},
		VerifyReservation: {
			Action: f.VerifyReservationAction,
			Transitions: fsm.Transitions{
				OnRecover:   VerifyReservation,
				OnProof:     Ready,
				OnTimeout:   Expired,
				fsm.OnError: NeedAdminAttention,
			},
		},
		Ready: {
			Action: f.ReadyAction,
			Transitions: fsm.Transitions{
				OnRecover:   Ready,
				OnTimeout:   Expired,
				fsm.OnError: NeedAdminAttention,
			},
		},
		CancelPrepay: {
			Action: f.CancelPrepayAction,
			Transitions: fsm.Transitions{
				OnRecover: CancelPrepay,
				// A repeated cancel is idempotent. The caller may
				// cancel again, or its cancel may race the quote
				// expiry that already moved us here.
				OnCancel:        CancelPrepay,
				OnPaid:          WaitForDelivery,
				OnCanceled:      Canceled,
				OnQuoteRejected: QuoteRejected,
				fsm.OnError:     NeedAdminAttention,
			},
		},
		NeedAdminAttention: {
			Action: f.NeedAdminAttentionAction,
			Transitions: fsm.Transitions{
				OnRecover: NeedAdminAttention,
			},
		},
		QuoteFailed: {Action: fsm.NoOpAction,
			Transitions: fsm.Transitions{OnRecover: QuoteFailed}},
		QuoteRejected: {
			Action:      fsm.NoOpAction,
			Transitions: fsm.Transitions{OnRecover: QuoteRejected},
		},
		Canceled: {
			Action:      fsm.NoOpAction,
			Transitions: fsm.Transitions{OnRecover: Canceled},
		},
		Expired: {
			Action:      fsm.NoOpAction,
			Transitions: fsm.Transitions{OnRecover: Expired},
		},
	}
}

// enterState runs after the shared FSM selects its next state and before that
// state's action. It records the triggering event so actions can distinguish
// approval from recovery. Re-entering the same state needs no transition write.
//
// A transition saves the new state together with any payment choices: approval
// stores the routing cap and SkipProbe. Background retries preserve that choice.
// The live reservation is updated only after the write succeeds.
//
// If approval validation or persistence fails, persistErr tells the next action
// to stop before making service calls. The shared FSM has already advanced in
// memory, so SendEvent reloads the saved reservation before handling another event.
func (f *FSM) enterState(ctx context.Context, n fsm.Notification) {
	f.event = n.Event
	if f.reservation.State == n.NextState {
		return
	}

	r := *f.reservation
	if n.Event == OnApproved {
		// The quote stays fixed. Persist the caller's remaining choices
		// atomically with the state that authorizes prepay preparation.
		req, _ := n.EventContext.(*looprpc.ApproveAssetReservationRequest)
		if err := r.CheckApproval(req); err != nil {
			f.persistErr, f.LastActionError = err, err
			return
		}
		r.MaxRouteFeeMsat = req.MaxRouteFeeMsat
		r.SkipProbe = req.SkipProbe
	}
	r.State = n.NextState
	f.persistErr = f.cfg.Store.UpdateReservation(ctx, &r)
	if f.persistErr != nil {
		f.LastActionError = f.persistErr
		return
	}
	*f.reservation = r
}

func (f *FSM) save(ctx context.Context) bool {
	if f.persistErr != nil {
		return false
	}
	f.persistErr = f.cfg.Store.UpdateReservation(ctx, f.reservation)
	if f.persistErr != nil {
		f.LastActionError = f.persistErr
		return false
	}
	return true
}

// stayInState records the action's result and ends this event without another
// transition. The error may be nil, for example after sending a payment whose
// settlement is still unknown. The manager later sends OnRecover or handles
// the next caller command; this helper does not retry an operation itself.
func (f *FSM) stayInState(err error) fsm.EventType {
	f.LastActionError = err
	return fsm.NoOp
}

func (f *FSM) fail(err error) fsm.EventType {
	f.LastActionError = err
	return fsm.OnError
}

// IsFinal reports whether purchase recovery and expiry watching are complete.
func IsFinal(state fsm.StateType) bool {
	return state == QuoteFailed || state == QuoteRejected ||
		state == Canceled || state == Expired
}
