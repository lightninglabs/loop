package staticaddr

import (
	"context"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	// AddressClient is the client that communicates with the loop server
	// to manage static addresses.
	AddressClient staticaddressrpc.StaticAddressServerClient

	// SwapClient provides loop rpc functionality.
	SwapClient *loop.Client

	// Store is the database store that is used to store static address
	// related records.
	Store AddressStore

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params
}

// Manager manages the address state machines.
type Manager struct {
	cfg *ManagerConfig

	initChan chan struct{}

	sync.Mutex
}

// NewAddressManager creates a new address manager.
func NewAddressManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:      cfg,
		initChan: make(chan struct{}),
	}
}

// Run runs the address manager.
func (m *Manager) Run(ctx context.Context) error {
	log.Debugf("Starting address manager.")
	defer log.Debugf("Address manager stopped.")

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	<-ctx.Done()

	return nil
}

// NewAddress starts a new address creation flow.
func (m *Manager) NewAddress(ctx context.Context) (*btcutil.AddressTaproot,
	error) {

	// If there's already a static address in the database, we can return
	// it.
	m.Lock()
	addresses, err := m.cfg.Store.GetAllStaticAddresses(ctx)
	if err != nil {
		m.Unlock()

		return nil, err
	}
	if len(addresses) > 0 {
		clientPubKey := addresses[0].ClientPubkey
		serverPubKey := addresses[0].ServerPubkey
		expiry := int64(addresses[0].Expiry)

		m.Unlock()

		return m.getTaprootAddress(clientPubKey, serverPubKey, expiry)
	}
	m.Unlock()

	// We are fetching a new L402 token from the server. There is one static
	// address per L402 token allowed.
	err = m.cfg.SwapClient.Server.FetchL402(ctx)
	if err != nil {
		return nil, err
	}

	clientPubKey, err := m.cfg.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		return nil, err
	}

	// Send our clientPubKey to the server and wait for the server to
	// respond with he serverPubKey and the static address CSV expiry.
	protocolVersion := CurrentRPCProtocolVersion()
	resp, err := m.cfg.AddressClient.ServerNewAddress(
		ctx, &staticaddressrpc.ServerNewAddressRequest{
			ProtocolVersion: protocolVersion,
			ClientKey:       clientPubKey.PubKey.SerializeCompressed(), //nolint:lll
		},
	)
	if err != nil {
		return nil, err
	}

	serverParams := resp.GetParams()

	serverPubKey, err := btcec.ParsePubKey(serverParams.ServerKey)
	if err != nil {
		return nil, err
	}

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(serverParams.Expiry),
		clientPubKey.PubKey, serverPubKey,
	)
	if err != nil {
		return nil, err
	}

	pkScript, err := staticAddress.StaticAddressScript()
	if err != nil {
		return nil, err
	}

	// Create the static address from the parameters the server provided and
	// store all parameters in the database.
	addrParams := &AddressParameters{
		ClientPubkey: clientPubKey.PubKey,
		ServerPubkey: serverPubKey,
		PkScript:     pkScript,
		Expiry:       serverParams.Expiry,
		KeyLocator: keychain.KeyLocator{
			Family: clientPubKey.Family,
			Index:  clientPubKey.Index,
		},
		ProtocolVersion: AddressProtocolVersion(protocolVersion),
	}
	err = m.cfg.Store.CreateStaticAddress(ctx, addrParams)
	if err != nil {
		return nil, err
	}

	return m.getTaprootAddress(
		clientPubKey.PubKey, serverPubKey, int64(serverParams.Expiry),
	)
}

func (m *Manager) getTaprootAddress(clientPubkey,
	serverPubkey *btcec.PublicKey, expiry int64) (*btcutil.AddressTaproot,
	error) {

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

// WaitInitComplete waits until the address manager has completed its setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Address manager initiation complete.")
	<-m.initChan
}
