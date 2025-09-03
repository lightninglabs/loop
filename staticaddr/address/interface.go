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

	// GetStaticAddress fetches static address parameters for a given
	// address ID.
	GetStaticAddress(ctx context.Context, pkScript []byte) (*Parameters,
		error)

	// GetAllStaticAddresses retrieves all static addresses from the store.
	GetAllStaticAddresses(ctx context.Context) ([]*Parameters,
		error)
}

// Parameters hold all the necessary information for the 2-of-2 multisig
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

// TaprootAddress returns the taproot address of the static address.
// Example: bc1phl46hgna56hfs0aykgccq29xl7z5z265vvs47lhfkkna749zmqyqa380nh.
func (p *Parameters) TaprootAddress(network *chaincfg.Params) (string, error) {
	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(p.Expiry), p.ClientPubkey,
		p.ServerPubkey,
	)
	if err != nil {
		return "", err
	}

	tpAddress, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(staticAddress.TaprootKey), network,
	)
	if err != nil {
		return "", err
	}

	return tpAddress.String(), nil
}
