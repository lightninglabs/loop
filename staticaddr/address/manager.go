package address

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	// AddressClient is the client that communicates with the loop server
	// to manage static addresses.
	AddressClient staticaddressrpc.StaticAddressServerClient

	// FetchL402 is the function used to fetch the l402 token.
	FetchL402 func(context.Context) error

	// Store is the database store that is used to store static address
	// related records.
	Store Store

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params

	// ChainNotifier is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient
}

// Manager manages the address state machines.
type Manager struct {
	sync.Mutex

	cfg *ManagerConfig

	currentHeight atomic.Int32
}

// NewManager creates a new address manager.
func NewManager(cfg *ManagerConfig, currentHeight int32) *Manager {
	m := &Manager{
		cfg: cfg,
	}
	m.currentHeight.Store(currentHeight)

	return m
}

// Run runs the address manager.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	newBlockChan, newBlockErrChan, err :=
		utils.RegisterBlockEpochNtfnWithRetry(ctx, m.cfg.ChainNotifier)

	if err != nil {
		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(initChan)

	for {
		select {
		case currentHeight := <-newBlockChan:
			m.currentHeight.Store(currentHeight)

		case err = <-newBlockErrChan:
			return err

		case <-ctx.Done():
			// Signal subroutines that the manager is exiting.
			return ctx.Err()
		}
	}
}

// NewAddress creates a new static address with the server or returns an
// existing one.
func (m *Manager) NewAddress(ctx context.Context) (*btcutil.AddressTaproot,
	int64, error) {

	// If there's already a static address in the database, we can return
	// it.
	m.Lock()
	addresses, err := m.cfg.Store.GetAllStaticAddresses(ctx)
	if err != nil {
		m.Unlock()

		return nil, 0, err
	}
	if len(addresses) > 0 {
		clientPubKey := addresses[0].ClientPubkey
		serverPubKey := addresses[0].ServerPubkey
		expiry := int64(addresses[0].Expiry)

		defer m.Unlock()

		address, err := m.GetTaprootAddress(
			clientPubKey, serverPubKey, expiry,
		)
		if err != nil {
			return nil, 0, err
		}

		return address, expiry, nil
	}
	m.Unlock()

	// We are fetching a new L402 token from the server. There is one static
	// address per L402 token allowed.
	err = m.cfg.FetchL402(ctx)
	if err != nil {
		return nil, 0, err
	}

	clientPubKey, err := m.cfg.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		return nil, 0, err
	}

	// Send our clientPubKey to the server and wait for the server to
	// respond with he serverPubKey and the static address CSV expiry.
	protocolVersion := version.CurrentRPCProtocolVersion()
	resp, err := m.cfg.AddressClient.ServerNewAddress(
		ctx, &staticaddressrpc.ServerNewAddressRequest{
			ProtocolVersion: protocolVersion,
			ClientKey:       clientPubKey.PubKey.SerializeCompressed(), //nolint:lll
		},
	)
	if err != nil {
		return nil, 0, err
	}

	serverParams := resp.GetParams()

	serverPubKey, err := btcec.ParsePubKey(serverParams.ServerKey)
	if err != nil {
		return nil, 0, err
	}

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(serverParams.Expiry),
		clientPubKey.PubKey, serverPubKey,
	)
	if err != nil {
		return nil, 0, err
	}

	pkScript, err := staticAddress.StaticAddressScript()
	if err != nil {
		return nil, 0, err
	}

	// Create the static address from the parameters the server provided and
	// store all parameters in the database.
	addrParams := &Parameters{
		ClientPubkey: clientPubKey.PubKey,
		ServerPubkey: serverPubKey,
		PkScript:     pkScript,
		Expiry:       serverParams.Expiry,
		KeyLocator: keychain.KeyLocator{
			Family: clientPubKey.Family,
			Index:  clientPubKey.Index,
		},
		ProtocolVersion: version.AddressProtocolVersion(
			protocolVersion,
		),
		InitiationHeight: m.currentHeight.Load(),
	}
	err = m.cfg.Store.CreateStaticAddress(ctx, addrParams)
	if err != nil {
		return nil, 0, err
	}

	// Import the static address tapscript into our lnd wallet, so we can
	// track unspent outputs of it.
	tapScript := input.TapscriptFullTree(
		staticAddress.InternalPubKey, *staticAddress.TimeoutLeaf,
	)
	addr, err := m.cfg.WalletKit.ImportTaprootScript(ctx, tapScript)
	if err != nil {
		return nil, 0, err
	}

	log.Infof("Imported static address taproot script to lnd wallet: %v",
		addr)

	address, err := m.GetTaprootAddress(
		clientPubKey.PubKey, serverPubKey, int64(serverParams.Expiry),
	)
	if err != nil {
		return nil, 0, err
	}

	return address, int64(serverParams.Expiry), nil
}

// GetTaprootAddress returns a taproot address for the given client and server
// public keys and expiry.
func (m *Manager) GetTaprootAddress(clientPubkey, serverPubkey *btcec.PublicKey,
	expiry int64) (*btcutil.AddressTaproot, error) {

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, expiry, clientPubkey, serverPubkey,
	)
	if err != nil {
		return nil, err
	}

	return btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(staticAddress.TaprootKey),
		m.cfg.ChainParams,
	)
}

// ListUnspentRaw returns a list of utxos at the static address.
func (m *Manager) ListUnspentRaw(ctx context.Context, minConfs,
	maxConfs int32) (*btcutil.AddressTaproot, []*lnwallet.Utxo, error) {

	addresses, err := m.cfg.Store.GetAllStaticAddresses(ctx)
	switch {
	case err != nil:
		return nil, nil, err

	case len(addresses) == 0:
		return nil, nil, nil

	case len(addresses) > 1:
		return nil, nil, fmt.Errorf("more than one address found")
	}

	staticAddress := addresses[0]

	// List all unspent utxos the wallet sees, regardless of the number of
	// confirmations.
	utxos, err := m.cfg.WalletKit.ListUnspent(
		ctx, minConfs, maxConfs,
	)
	if err != nil {
		return nil, nil, err
	}

	// Filter the list of lnd's unspent utxos for the pkScript of our static
	// address.
	var filteredUtxos []*lnwallet.Utxo
	for _, utxo := range utxos {
		if bytes.Equal(utxo.PkScript, staticAddress.PkScript) {
			filteredUtxos = append(filteredUtxos, utxo)
		}
	}

	taprootAddress, err := m.GetTaprootAddress(
		staticAddress.ClientPubkey, staticAddress.ServerPubkey,
		int64(staticAddress.Expiry),
	)
	if err != nil {
		return nil, nil, err
	}

	return taprootAddress, filteredUtxos, nil
}

// GetStaticAddressParameters returns the parameters of the static address.
func (m *Manager) GetStaticAddressParameters(ctx context.Context) (*Parameters,
	error) {

	params, err := m.cfg.Store.GetAllStaticAddresses(ctx)
	if err != nil {
		return nil, err
	}

	if len(params) == 0 {
		return nil, fmt.Errorf("no static address parameters found")
	}

	return params[0], nil
}

// GetStaticAddress returns a taproot address for the given client and server
// public keys and expiry.
func (m *Manager) GetStaticAddress(ctx context.Context) (*script.StaticAddress,
	error) {

	params, err := m.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}

	address, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(params.Expiry),
		params.ClientPubkey, params.ServerPubkey,
	)
	if err != nil {
		return nil, err
	}

	return address, nil
}

// ListUnspent returns a list of utxos at the static address.
func (m *Manager) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	_, utxos, err := m.ListUnspentRaw(ctx, minConfs, maxConfs)
	if err != nil {
		return nil, err
	}

	return utxos, nil
}
