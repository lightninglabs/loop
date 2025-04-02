package deposit

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	IdLength = 32
)

// Store is the database interface that is used to store and retrieve
// static address deposits.
type Store interface {
	// CreateDeposit inserts a new deposit into the store.
	CreateDeposit(ctx context.Context, deposit *Deposit) error

	// UpdateDeposit updates the deposit in the database.
	UpdateDeposit(ctx context.Context, deposit *Deposit) error

	// GetDeposit retrieves a deposit with depositID from the database.
	GetDeposit(ctx context.Context, depositID ID) (*Deposit, error)

	// DepositForOutpoint retrieves the deposit with the given outpoint.
	DepositForOutpoint(ctx context.Context, outpoint string) (*Deposit,
		error)

	// AllDeposits retrieves all deposits from the store.
	AllDeposits(ctx context.Context) ([]*Deposit, error)
}

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// GetStaticAddressParameters returns the static address parameters.
	GetStaticAddressParameters(ctx context.Context) (*address.Parameters,
		error)

	// GetStaticAddress returns the deposit address for the given
	// client and server public keys.
	GetStaticAddress(ctx context.Context) (*script.StaticAddress, error)

	// ListUnspent returns a list of utxos at the static address.
	ListUnspent(ctx context.Context, minConfs,
		maxConfs int32) ([]*lnwallet.Utxo, error)

	// GetTaprootAddress returns a taproot address.
	GetTaprootAddress(clientPubkey, serverPubkey *btcec.PublicKey,
		expiry int64) (*btcutil.AddressTaproot, error)
}
