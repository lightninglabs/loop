package address

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	ErrAddressAlreadyExists = fmt.Errorf("address already exists")
)

// Store is the database interface that is used to store and retrieve
// static addresses.
type Store interface {
	// CreateStaticAddress inserts a new static address with its parameters
	// into the store.
	CreateStaticAddress(ctx context.Context, addrParams *Parameters) error

	// GetStaticAddress fetches static address parameters for a given
	// address ID.
	GetStaticAddress(ctx context.Context, pkScript []byte) (*Parameters,
		error)

	// GetAllStaticAddresses retrieves all static addresses from the store.
	GetAllStaticAddresses(ctx context.Context) ([]*Parameters,
		error)
}

// Parameters holds all the necessary information for the 2-of-2 multisig
// address.
type Parameters struct {
	// ClientPubkey is the client's pubkey for the static address. It is
	// used for the 2-of-2 funding output as well as for the client's
	// timeout path.
	ClientPubkey *btcec.PublicKey

	// ClientPubkey is the client's pubkey for the static address. It is
	// used for the 2-of-2 funding output.
	ServerPubkey *btcec.PublicKey

	// Expiry is the CSV timout value at which the client can claim the
	// static address's timout path.
	Expiry uint32

	// PkScript is the unique static address's output script.
	PkScript []byte

	// KeyLocator is the locator of the client's key.
	KeyLocator keychain.KeyLocator

	// ProtocolVersion is the protocol version of the static address.
	ProtocolVersion version.AddressProtocolVersion

	// InitiationHeight is the height at which the address was initiated.
	InitiationHeight int32
}
