package withdraw

import (
	"crypto/rand"
	"fmt"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
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

// Closure...
type Closure struct {
	// ID is the unique identifier of the deposit.
	ID ID

	// State is the current state of the deposit.
	State fsm.StateType

	// Deposits is the set of deposits that are part of the closure.
	Deposits []*deposit.Deposit

	// ConfirmationHeight is the absolute height at which the closure was
	// first confirmed.
	ConfirmationHeight int64

	// ClosureDestinationPkScript ...
	ClosureDestinationPkScript []byte
}

// GetRandomDepositID generates a random deposit ID.
func GetRandomDepositID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}
