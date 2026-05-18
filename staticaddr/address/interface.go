package address

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Store is the database interface that is used to store and retrieve
// static addresses.
type Store interface {
	// CreateStaticAddress inserts a new static address with its parameters
	// into the store.
	CreateStaticAddress(ctx context.Context, addrParams *Parameters) error

	// GetStaticAddressID retrieves the static address row ID for the
	// address script.
	GetStaticAddressID(ctx context.Context, pkScript []byte) (int32, error)

	// GetAllStaticAddresses retrieves all static addresses from the store.
	GetAllStaticAddresses(ctx context.Context) ([]*Parameters,
		error)

	// GetLegacyParameters retrieves the first static address created for the
	// L402. This is the immutable legacy/root address that anchors existing
	// single-address deposits.
	GetLegacyParameters(ctx context.Context) (*Parameters, error)
}

// Parameters holds all the necessary information for the 2-of-2 multisig
// address.
type Parameters struct {
	// ID is the database primary key of the static address row. A zero value
	// means the parameters have not been persisted yet.
	ID int32

	// ClientPubkey is the client's pubkey for the static address. It is
	// used for the 2-of-2 funding output as well as for the client's
	// timeout path.
	ClientPubkey *btcec.PublicKey

	// ServerPubkey is the server's pubkey for the static address. It is
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

// TaprootAddress returns the bech32m taproot address for these static address
// parameters on the target network.
func (p *Parameters) TaprootAddress(network *chaincfg.Params) (
	*btcutil.AddressTaproot, error) {

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(p.Expiry),
		p.ClientPubkey, p.ServerPubkey,
	)
	if err != nil {
		return nil, err
	}

	return btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(staticAddress.TaprootKey), network,
	)
}
