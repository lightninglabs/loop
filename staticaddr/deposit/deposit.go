package deposit

import (
	"crypto/rand"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
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

	// State is the current state of the deposit.
	State fsm.StateType

	// The outpoint of the deposit.
	wire.OutPoint

	// Value is the amount of the deposit.
	Value btcutil.Amount

	// ConfirmationHeight is the absolute height at which the deposit was
	// first confirmed.
	ConfirmationHeight int64

	// TimeOutSweepPkScript is the pk script that is used to sweep the
	// deposit to after it is expired.
	TimeOutSweepPkScript []byte
}

// IsPending returns true if the deposit is pending.
func (d *Deposit) IsPending() bool {
	return !d.IsFinal()
}

// IsFinal returns true if the deposit is final.
func (d *Deposit) IsFinal() bool {
	return d.State == Withdrawn || d.State == Expired ||
		d.State == Failed
}

// GetRandomDepositID generates a random deposit ID.
func GetRandomDepositID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}
