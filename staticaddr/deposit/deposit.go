package deposit

import (
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
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

// Deposit bundles an utxo at a static address together with manager-relevant
// data.
type Deposit struct {
	// ID is the unique identifier of the deposit.
	ID ID

	// state is the current state of the deposit.
	state fsm.StateType

	// Outpoint of the deposit.
	wire.OutPoint

	// Value is the amount of the deposit.
	Value btcutil.Amount

	// ConfirmationHeight is the absolute height at which the deposit was
	// first confirmed.
	ConfirmationHeight int64

	// TimeOutSweepPkScript is the pk script that is used to sweep the
	// deposit to after it is expired.
	TimeOutSweepPkScript []byte

	// ExpirySweepTxid is the transaction id of the expiry sweep.
	ExpirySweepTxid chainhash.Hash

	// FinalizedWithdrawalTx is the coop signed withdrawal transaction. It
	// is republished on new block arrivals and on client restarts.
	FinalizedWithdrawalTx *wire.MsgTx

	sync.Mutex
}

// IsInPendingState returns true if the deposit is pending.
func (d *Deposit) IsInPendingState() bool {
	d.Lock()
	defer d.Unlock()

	return !d.IsInFinalState()
}

// IsInFinalState returns true if the deposit is final.
func (d *Deposit) IsInFinalState() bool {
	d.Lock()
	defer d.Unlock()

	return d.state == Expired || d.state == Withdrawn || d.state == Failed
}

func (d *Deposit) IsExpired(currentHeight, expiry uint32) bool {
	d.Lock()
	defer d.Unlock()

	return currentHeight >= uint32(d.ConfirmationHeight)+expiry
}

func (d *Deposit) GetState() fsm.StateType {
	d.Lock()
	defer d.Unlock()

	return d.state
}

func (d *Deposit) SetState(state fsm.StateType) {
	d.Lock()
	defer d.Unlock()

	d.state = state
}

func (d *Deposit) IsInState(state fsm.StateType) bool {
	d.Lock()
	defer d.Unlock()

	return d.state == state
}

// GetRandomDepositID generates a random deposit ID.
func GetRandomDepositID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}
