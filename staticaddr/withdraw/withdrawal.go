package withdraw

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

const (
	IdLength = 32
)

// ID is a unique identifier for a deposit.
type ID [IdLength]byte

// FromByteSlice creates a deposit id from a byte slice.
func (r *ID) FromByteSlice(b []byte) error {
	if len(b) != IdLength {
		return fmt.Errorf("withdrawal id must be 32 bytes, got %d, %x",
			len(b), b)
	}

	copy(r[:], b)

	return nil
}

// Withdrawal represents a finalized static address withdrawal record in the
// database.
type Withdrawal struct {
	// ID is the unique identifier of the deposit.
	ID ID

	// TxID is the transaction ID of the withdrawal.
	TxID chainhash.Hash

	// Deposits is a list of deposits used to fund the withdrawal.
	Deposits []*deposit.Deposit

	// TotalDepositAmount is the total amount of all deposits used to fund
	// the withdrawal.
	TotalDepositAmount btcutil.Amount

	// WithdrawnAmount is the amount withdrawn. It represents the total
	// value of selected deposits minus fees and change.
	WithdrawnAmount btcutil.Amount

	// ChangeAmount is the optional change returned to the static address.
	ChangeAmount btcutil.Amount

	// InitiationTime is the time at which the withdrawal was initiated.
	InitiationTime time.Time

	// ConfirmationHeight is the block height at which the withdrawal was
	// confirmed.
	ConfirmationHeight int64
}

// GetRandomWithdrawalID generates a random withdrawal ID.
func GetRandomWithdrawalID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}
