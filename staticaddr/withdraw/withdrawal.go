package withdraw

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// Withdrawal represents a finalized static address withdrawal record in the
// database.
type Withdrawal struct {
	// TxID is the transaction ID of the withdrawal.
	TxID chainhash.Hash

	// DepositOutpoints is a list of outpoints that were used to fund the
	// withdrawal.
	DepositOutpoints []string

	// TotalDepositAmount is the total amount of all deposits used to fund
	// the withdrawal.
	TotalDepositAmount btcutil.Amount

	// WithdrawnAmount is the amount withdrawn. It represents the total
	// value of selected deposits minus fees and change.
	WithdrawnAmount btcutil.Amount

	// ChangeAmount is the optional change returned to the static address.
	ChangeAmount btcutil.Amount

	// ConfirmationHeight is the block height at which the withdrawal was
	// confirmed.
	ConfirmationHeight uint32
}
