package deposit

import "context"

// Store defines the interface that the Manager requires from the storage layer.
type Store interface {
	// AddAssetDeposit adds a new asset deposit to the database.
	AddAssetDeposit(ctx context.Context, d *Deposit) error

	// UpdateDeposit updates the deposit state and extends the deposit
	// update log in the SQL store.
	UpdateDeposit(ctx context.Context, d *Deposit) error

	// GetAllDeposits returns all deposits known to the store.
	GetAllDeposits(ctx context.Context) ([]Deposit, error)

	// GetActiveDeposits returns all active deposits from the database.
	// Active deposits are those that have not yet been spent or swept.
	GetActiveDeposits(ctx context.Context) ([]Deposit, error)
}
