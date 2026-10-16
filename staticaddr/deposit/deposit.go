package deposit

import (
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ID is a unique identifier for a deposit.
type ID [IdLength]byte

// FromByteSlice creates a deposit id from a byte slice.
func (r *ID) FromByteSlice(b []byte) error {
	if len(b) != IdLength {
		return fmt.Errorf("deposit id must be 32 bytes, got %d, %x",
			len(b), b)
	}

	copy(r[:], b)

	return nil
}

// OnchainCostStatus tells whether the on-chain cost of a deposit is known. It
// is stored as onchain_cost_known: NULL if undetermined, otherwise whether the
// cost is known.
type OnchainCostStatus uint8

const (
	// OnchainCostUndetermined means the cost hasn't been determined yet.
	OnchainCostUndetermined OnchainCostStatus = iota

	// OnchainCostKnown means the client wallet knows the fee of the
	// deposit's funding transaction, and the cost is the deposit's share
	// of it.
	OnchainCostKnown

	// OnchainCostUnknown means the client wallet doesn't know the fee of
	// the deposit's funding transaction, for example for a deposit funded
	// by an external wallet. The cost is zero then.
	OnchainCostUnknown
)

// OnchainCost is the client-side on-chain cost of a deposit.
type OnchainCost struct {
	// Amount is the deposit's share of the fee of its funding
	// transaction. It is zero unless the status is known.
	Amount btcutil.Amount

	// Status tells whether the cost is known.
	Status OnchainCostStatus
}

// Deposit bundles an utxo at a static address together with manager-relevant
// data.
//
// Lock order: if both Manager.mu and a Deposit lock are needed, acquire
// Manager.mu before Deposit.Lock. Never acquire Manager.mu while holding a
// Deposit lock.
//
// The state and ConfirmationHeight fields are mutable and protected by the
// deposit lock.
type Deposit struct {
	sync.Mutex

	// Outpoint of the deposit.
	wire.OutPoint

	// ID is the unique identifier of the deposit.
	ID ID

	// state is the current state of the deposit.
	state fsm.StateType

	// Value is the amount of the deposit.
	Value btcutil.Amount

	// ConfirmationHeight is the absolute height at which the deposit was
	// first confirmed. A value of zero means the deposit is still
	// unconfirmed.
	ConfirmationHeight int64

	// TimeOutSweepPkScript is the pk script that is used to sweep the
	// deposit to after it is expired.
	TimeOutSweepPkScript []byte

	// ExpirySweepTxid is the transaction id of the expiry sweep.
	ExpirySweepTxid chainhash.Hash

	// SwapHash is an optional reference to a static address loop-in swap
	// that used this deposit.
	SwapHash *lntypes.Hash

	// FinalizedWithdrawalTx is the coop-signed withdrawal transaction. It
	// is republished on new block arrivals and on client restarts.
	FinalizedWithdrawalTx *wire.MsgTx

	// OnchainCost is the deposit's share of the fee of its funding
	// transaction. The manager determines it shortly after it finds the
	// deposit. It is protected by the deposit lock for deposits the
	// manager holds; use GetOnchainCost to read it.
	OnchainCost OnchainCost
}

// GetOnchainCost returns the on-chain cost of the deposit.
func (d *Deposit) GetOnchainCost() OnchainCost {
	d.Lock()
	defer d.Unlock()

	return d.OnchainCost
}

// IsInFinalState returns true if the deposit is final.
func (d *Deposit) IsInFinalState() bool {
	d.Lock()
	defer d.Unlock()

	return d.isInFinalStateNoLock()
}

// isInFinalStateNoLock returns true if the deposit is final without acquiring
// the deposit lock.
func (d *Deposit) isInFinalStateNoLock() bool {
	return d.state == Expired || d.state == Withdrawn ||
		d.state == LoopedIn || d.state == HtlcTimeoutSwept ||
		d.state == ChannelPublished
}

func (d *Deposit) IsExpired(currentHeight, expiry uint32) bool {
	d.Lock()
	defer d.Unlock()

	if d.ConfirmationHeight <= 0 {
		return false
	}

	return currentHeight >= uint32(d.ConfirmationHeight)+expiry
}

func (d *Deposit) GetState() fsm.StateType {
	d.Lock()
	defer d.Unlock()

	return d.state
}

func (d *Deposit) getStateNoLock() fsm.StateType {
	return d.state
}

func (d *Deposit) SetState(state fsm.StateType) {
	d.Lock()
	defer d.Unlock()

	d.state = state
}

func (d *Deposit) setStateNoLock(state fsm.StateType) {
	d.state = state
}

func (d *Deposit) IsInState(state fsm.StateType) bool {
	d.Lock()
	defer d.Unlock()

	return d.state == state
}

func (d *Deposit) isInStateNoLock(state fsm.StateType) bool {
	return d.state == state
}

// IsInStateNoLock returns whether the deposit is in the given state without
// acquiring the deposit lock.
func (d *Deposit) IsInStateNoLock(state fsm.StateType) bool {
	return d.isInStateNoLock(state)
}

// GetConfirmationHeight returns the deposit confirmation height.
func (d *Deposit) GetConfirmationHeight() int64 {
	d.Lock()
	defer d.Unlock()

	return d.ConfirmationHeight
}

// GetConfirmationHeightNoLock returns the deposit confirmation height without
// acquiring the deposit lock.
func (d *Deposit) GetConfirmationHeightNoLock() int64 {
	return d.ConfirmationHeight
}

// GetRandomDepositID generates a random deposit ID.
func GetRandomDepositID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}
