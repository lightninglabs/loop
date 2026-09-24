package address

import (
	"context"

	"github.com/lightninglabs/loop/staticaddr/script"
)

// AddressParameters describes one static address for callers of the address
// manager API. It aliases the script-level parameters used to spend that address.
type AddressParameters = script.Parameters

// Store is the database interface that is used to store and retrieve
// static addresses.
type Store interface {
	// CreateStaticAddress inserts a new static address with its parameters
	// into the store.
	CreateStaticAddress(ctx context.Context, addrParams *AddressParameters) error

	// GetStaticAddressID retrieves the static address row ID for the
	// address script.
	GetStaticAddressID(ctx context.Context, pkScript []byte) (int32, error)

	// ListStaticAddresses retrieves up to limit addresses in ascending ID
	// order, strictly after afterID. Use zero to start from the beginning.
	ListStaticAddresses(ctx context.Context, afterID, limit int32) (
		[]*AddressParameters, error)

	// GetAllStaticAddresses retrieves all static addresses from the store.
	GetAllStaticAddresses(ctx context.Context) ([]*AddressParameters, error)

	// GetLegacyParameters retrieves the first static address created for the
	// L402. This is the immutable legacy/root address that anchors existing
	// single-address deposits.
	GetLegacyParameters(ctx context.Context) (*AddressParameters, error)
	// UpdateStaticAddressLabel updates the local label for a static address by
	// its pkScript so metadata changes never alter address scripts or server
	// state.
	UpdateStaticAddressLabel(ctx context.Context, pkScript []byte,
		label string) error
}
