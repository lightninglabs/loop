package address

import (
	"context"

	"github.com/lightninglabs/loop/staticaddr/script"
)

// Parameters aliases the script-level static address parameters for callers
// that interact with the address manager API.
type Parameters = script.Parameters

// Store is the database interface that is used to store and retrieve
// static addresses.
type Store interface {
	// CreateStaticAddress inserts a new static address with its parameters
	// into the store.
	CreateStaticAddress(ctx context.Context, addrParams *Parameters) error

	// GetAllStaticAddresses retrieves all static addresses from the store.
	GetAllStaticAddresses(ctx context.Context) ([]*Parameters, error)
}
