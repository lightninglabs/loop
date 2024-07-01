package loopin

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/loop"
)

var (
	// hopHintFactor is factor by which we scale the total amount of
	// inbound capacity we want our hop hints to represent, allowing us to
	// have some leeway if peers go offline.
	hopHintFactor = 2
)

// Manager manages the address state machines.
type Manager struct {
	cfg *Config

	runCtx context.Context

	sync.Mutex

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

	// initiationHeight stores the currently best known block height.
	initiationHeight uint32

	// currentHeight stores the currently best known block height.
	currentHeight uint32

	activeLoopIns map[ID]*FSM
}

// NewManager creates a new deposit withdrawal manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:           cfg,
		initChan:      make(chan struct{}),
		activeLoopIns: make(map[ID]*FSM),
	}
}

// Run runs the deposit withdrawal manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.runCtx = ctx

	m.Lock()
	m.currentHeight, m.initiationHeight = currentHeight, currentHeight
	m.Unlock()

	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(m.runCtx) //nolint:lll
	if err != nil {
		return err
	}

	err = m.recover()
	if err != nil {
		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	for {
		select {
		case height := <-newBlockChan:
			m.Lock()
			m.currentHeight = uint32(height)
			m.Unlock()

		case err = <-newBlockErrChan:
			return err

		case <-m.runCtx.Done():
			return m.runCtx.Err()
		}
	}
}

func (m *Manager) recover() error {
	return nil
}

// WaitInitComplete waits until the static address loop-in manager has completed
// its setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Static address loop-in manager initiation complete.")
	<-m.initChan
}

// LoopIn ...
func (m *Manager) LoopIn(req *loop.StaticAddressLoopInRequest) error {
	requestContext := &RequestContext{
		depositOutpoints: req.DepositOutpoints,
		maxSwapFee:       req.MaxSwapFee,
		maxMinerFee:      req.MaxMinerFee,
		lastHop:          req.LastHop,
		label:            req.Label,
		userAgent:        req.Initiator,
		private:          req.Private,
		routeHints:       req.RouteHints,
	}

	return m.startLoopInFsm(requestContext)
}

func (m *Manager) startLoopInFsm(reqCtx *RequestContext) error {
	// Create a state machine for a given deposit.
	fsm := NewFSM(m.runCtx, m.cfg)

	// Add the FSM to the active FSMs map.
	m.Lock()
	m.activeLoopIns[fsm.loopIn.ID] = fsm
	m.Unlock()

	// Send the start event to the state machine.
	go func() {
		err := fsm.SendEvent(OnNewRequest, reqCtx)
		if err != nil {
			log.Errorf("Error sending OnNewRequest event: %v", err)
		}
	}()

	err := fsm.DefaultObserver.WaitForState(m.runCtx, time.Minute, Succeeded)
	if err != nil {
		return err
	}

	return nil
}
