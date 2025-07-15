package deposit

import "context"

// Store defines the interface that the Manager requires from the storage layer.
type Store interface {
	// AddAssetDeposit adds a new asset deposit to the database.
	AddAssetDeposit(ctx context.Context, d *Deposit) error

	// UpdateDeposit updates the deposit state and extends the deposit
	// update log in the SQL store.
	UpdateDeposit(ctx context.Context, d *Deposit) error
}
