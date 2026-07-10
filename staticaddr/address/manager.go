package address

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// maxStaticAddressCSVExpiry is the maximum CSV delay that we accept
	// from the server for a static address timeout path: 200 days at 144
	// blocks per day.
	maxStaticAddressCSVExpiry = uint32(200 * 144)
)

var (
	// ErrNoStaticAddress is returned when no static address parameters are
	// present in the store.
	ErrNoStaticAddress = errors.New("no static address parameters found")
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

	// activeStaticAddresses is the runtime index used to match wallet UTXOs
	// to locally known static address parameters. The DB remains the
	// durable source of truth; this map is rebuilt from the DB on startup
	// and updated after successful address issuance.
	activeStaticAddresses map[string]*Parameters
}

// NewManager creates a new address manager.
func NewManager(cfg *ManagerConfig, currentHeight int32) (*Manager, error) {
	if currentHeight <= 0 {
		return nil, fmt.Errorf("invalid current height %d",
			currentHeight)
	}

	m := &Manager{
		cfg:                   cfg,
		activeStaticAddresses: make(map[string]*Parameters),
	}
	m.currentHeight.Store(currentHeight)

	return m, nil
}

// Run runs the address manager.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	newBlockChan, newBlockErrChan, err :=
		m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx)

	if err != nil {
		return err
	}

	err = m.loadActiveAddresses(ctx)
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

// loadActiveAddresses rebuilds the runtime address map from the durable DB
// state and re-imports all scripts into lnd. Importing is intentionally
// idempotent so restart paths repair missing wallet watches before deposit
// discovery starts.
func (m *Manager) loadActiveAddresses(ctx context.Context) error {
	params, err := m.cfg.Store.GetAllStaticAddresses(ctx)
	if err != nil {
		return err
	}

	active := make(map[string]*Parameters, len(params))
	for _, param := range params {
		staticAddress, err := staticAddressFromParams(param)
		if err != nil {
			return err
		}

		err = m.importAddressTapscript(ctx, staticAddress)
		if err != nil {
			return err
		}

		active[string(param.PkScript)] = param
	}

	m.Lock()
	m.activeStaticAddresses = active
	m.Unlock()

	return nil
}

// NewAddress creates the next externally visible receive static address.
//
// The first call also makes sure the legacy/root static address seed exists,
// because receive and change addresses are derived from the server pubkey and
// expiry returned for that seed.
func (m *Manager) NewAddress(ctx context.Context) (*btcutil.AddressTaproot,
	int64, error) {

	params, err := m.NewReceiveAddress(ctx)
	if err != nil {
		return nil, 0, err
	}

	address, err := m.GetTaprootAddress(
		params.ClientPubkey, params.ServerPubkey, int64(params.Expiry),
	)
	if err != nil {
		return nil, 0, err
	}

	return address, int64(params.Expiry), nil
}

// EnsureStaticAddressSeed loads or creates the legacy/root static address
// parameters. The root address is the only address that requires a Nautilus
// ServerNewAddress call; all receive/change addresses derive client keys
// locally and reuse this server pubkey/expiry seed.
func (m *Manager) EnsureStaticAddressSeed(ctx context.Context) (*Parameters,
	error) {

	m.Lock()
	seed := m.legacyParameters()
	m.Unlock()
	if seed != nil {
		return seed, nil
	}

	m.Lock()
	defer m.Unlock()

	// Another caller may have created the seed while we were waiting for the
	// issuance lock.
	seed = m.legacyParameters()
	if seed != nil {
		return seed, nil
	}

	addresses, err := m.cfg.Store.GetAllStaticAddresses(ctx)
	if err != nil {
		return nil, err
	}
	if len(addresses) > 0 {
		for _, addr := range addresses {
			// Re-import existing rows so startup can repair a DB-only
			// address before deposit discovery depends on lnd's wallet
			// view.
			staticAddress, err := staticAddressFromParams(addr)
			if err != nil {
				return nil, err
			}

			err = m.importAddressTapscript(ctx, staticAddress)
			if err != nil {
				return nil, err
			}

			m.activeStaticAddresses[string(addr.PkScript)] = addr
		}

		return addresses[0], nil
	}

	// We are fetching a new L402 token from the server. The returned server
	// key/expiry is the static address seed for all future client-derived
	// addresses for this L402.
	err = m.cfg.FetchL402(ctx)
	if err != nil {
		return nil, err
	}

	clientPubKey, err := m.cfg.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		return nil, err
	}

	protocolVersion := version.CurrentRPCProtocolVersion()
	resp, err := m.cfg.AddressClient.ServerNewAddress(
		ctx, &staticaddressrpc.ServerNewAddressRequest{
			ProtocolVersion: protocolVersion,
			ClientKey:       clientPubKey.PubKey.SerializeCompressed(), //nolint:lll
		},
	)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, fmt.Errorf("missing server new address response")
	}

	serverParams := resp.GetParams()
	if err := validateServerAddressParams(serverParams); err != nil {
		return nil, err
	}

	serverPubKey, err := btcec.ParsePubKey(serverParams.GetServerKey())
	if err != nil {
		return nil, err
	}

	return m.createAddressFromKey(
		ctx, clientPubKey, serverPubKey, serverParams.Expiry,
		version.AddressProtocolVersion(protocolVersion),
	)
}

// NewReceiveAddress derives, stores, imports and activates the next receive
// family static address. It is used by `loop static new`.
func (m *Manager) NewReceiveAddress(ctx context.Context) (*Parameters, error) {
	seed, err := m.EnsureStaticAddressSeed(ctx)
	if err != nil {
		return nil, err
	}

	return m.newDerivedAddress(ctx, seed, swap.StaticMultiAddressKeyFamily)
}

// NewChangeAddress derives, stores, imports and activates the next change
// family static address. Swap and withdrawal code calls this before submitting
// requests that require change.
func (m *Manager) NewChangeAddress(ctx context.Context) (*Parameters, error) {
	seed, err := m.EnsureStaticAddressSeed(ctx)
	if err != nil {
		return nil, err
	}

	return m.newDerivedAddress(ctx, seed, swap.StaticAddressChangeKeyFamily)
}

func (m *Manager) newDerivedAddress(ctx context.Context, seed *Parameters,
	keyFamily int32) (*Parameters, error) {

	m.Lock()
	defer m.Unlock()

	clientPubKey, err := m.cfg.WalletKit.DeriveNextKey(ctx, keyFamily)
	if err != nil {
		return nil, err
	}

	return m.createAddressFromKey(
		ctx, clientPubKey, seed.ServerPubkey, seed.Expiry,
		seed.ProtocolVersion,
	)
}

func (m *Manager) createAddressFromKey(ctx context.Context,
	clientPubKey *keychain.KeyDescriptor, serverPubKey *btcec.PublicKey,
	expiry uint32, protocolVersion version.AddressProtocolVersion) (
	*Parameters, error) {

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(expiry), clientPubKey.PubKey,
		serverPubKey,
	)
	if err != nil {
		return nil, err
	}

	pkScript, err := staticAddress.StaticAddressScript()
	if err != nil {
		return nil, err
	}

	addrParams := &Parameters{
		ClientPubkey: clientPubKey.PubKey,
		ServerPubkey: serverPubKey,
		PkScript:     pkScript,
		Expiry:       expiry,
		KeyLocator: keychain.KeyLocator{
			Family: clientPubKey.Family,
			Index:  clientPubKey.Index,
		},
		ProtocolVersion:  protocolVersion,
		InitiationHeight: m.currentHeight.Load(),
	}

	// Import before persisting the address row. If lnd rejects the script
	// import, a later startup retry should still see a clean missing-address
	// state instead of a DB-only static address.
	err = m.importAddressTapscript(ctx, staticAddress)
	if err != nil {
		return nil, err
	}

	err = m.cfg.Store.CreateStaticAddress(ctx, addrParams)
	if err != nil {
		return nil, err
	}

	addrParams.ID, err = m.cfg.Store.GetStaticAddressID(ctx, pkScript)
	if err != nil {
		return nil, err
	}

	m.activeStaticAddresses[string(pkScript)] = addrParams

	return addrParams, nil
}

// validateServerAddressParams validates the server-controlled static address
// parameters before they are committed into the address script or database.
func validateServerAddressParams(
	params *staticaddressrpc.ServerAddressParameters) error {

	if params == nil {
		return fmt.Errorf("missing server address parameters")
	}

	serverKey := params.GetServerKey()
	if len(serverKey) == 0 {
		return fmt.Errorf("missing server public key")
	}
	if !btcec.IsCompressedPubKey(serverKey) {
		return fmt.Errorf("server public key is not a compressed " +
			"secp256k1 public key")
	}

	expiry := params.GetExpiry()
	switch {
	case expiry == 0:
		return fmt.Errorf("static address CSV expiry must be non-zero")

	case expiry&^wire.SequenceLockTimeMask != 0:
		return fmt.Errorf("static address expiry does not fit into "+
			"CSV: %x", expiry)

	case expiry > maxStaticAddressCSVExpiry:
		return fmt.Errorf("static address CSV expiry %v exceeds "+
			"maximum %v", expiry, maxStaticAddressCSVExpiry)
	}

	return nil
}

func (m *Manager) importAddressTapscript(ctx context.Context,
	staticAddress *script.StaticAddress) error {

	// Import the static address tapscript into our lnd wallet, so we can
	// track unspent outputs of it.
	tapScript := input.TapscriptFullTree(
		staticAddress.InternalPubKey, *staticAddress.TimeoutLeaf,
	)
	addr, err := m.cfg.WalletKit.ImportTaprootScript(ctx, tapScript)
	if err != nil {
		// Importing into an lnd instance that already knows the script is
		// expected on restart. Treat the duplicate import as success.
		if strings.Contains(err.Error(), "already exists") {
			log.Infof("Static address tapscript already imported")
			return nil
		}

		return err
	}

	log.Infof("Imported static address taproot script to lnd wallet: %v",
		addr)

	return nil
}

func staticAddressFromParams(params *Parameters) (*script.StaticAddress,
	error) {

	if params == nil {
		return nil, fmt.Errorf("missing static address parameters")
	}

	return script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(params.Expiry),
		params.ClientPubkey, params.ServerPubkey,
	)
}

func (m *Manager) legacyParameters() *Parameters {
	var legacy *Parameters
	for _, params := range m.activeStaticAddresses {
		if params == nil {
			continue
		}

		if legacy == nil || params.ID < legacy.ID {
			legacy = params
		}
	}

	return legacy
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
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	m.Lock()
	active := make(map[string]struct{}, len(m.activeStaticAddresses))
	for pkScript := range m.activeStaticAddresses {
		active[pkScript] = struct{}{}
	}
	m.Unlock()
	if len(active) == 0 {
		return nil, nil
	}

	// List all unspent utxos the wallet sees, regardless of the number of
	// confirmations.
	utxos, err := m.cfg.WalletKit.ListUnspent(
		ctx, minConfs, maxConfs,
	)
	if err != nil {
		return nil, err
	}

	// Filter the list of lnd's unspent utxos for any locally active static
	// address script.
	var filteredUtxos []*lnwallet.Utxo
	for _, utxo := range utxos {
		if _, ok := active[string(utxo.PkScript)]; ok {
			filteredUtxos = append(filteredUtxos, utxo)
		}
	}

	return filteredUtxos, nil
}

// GetStaticAddressParameters returns the legacy/root static-address
// parameters.
func (m *Manager) GetStaticAddressParameters(ctx context.Context) (
	*script.Parameters, error) {

	params, err := m.GetLegacyParameters(ctx)
	if err != nil {
		return nil, err
	}

	if params == nil {
		return nil, ErrNoStaticAddress
	}

	return params, nil
}

// GetStaticAddress returns a taproot address for the given client and server
// public keys and expiry.
func (m *Manager) GetStaticAddress(ctx context.Context) (*script.StaticAddress,
	error) {

	params, err := m.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}

	return staticAddressFromParams(params)
}

// ListUnspent returns a list of utxos at the static address.
func (m *Manager) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	return m.ListUnspentRaw(ctx, minConfs, maxConfs)
}

// GetLegacyParameters returns the legacy/root static address parameters.
func (m *Manager) GetLegacyParameters(ctx context.Context) (*Parameters,
	error) {

	params, err := m.cfg.Store.GetLegacyParameters(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return params, nil
}

// GetParameters returns active static address parameters for a pkScript.
func (m *Manager) GetParameters(pkScript []byte) *Parameters {
	m.Lock()
	defer m.Unlock()

	return m.activeStaticAddresses[string(pkScript)]
}

// GetStaticAddressID returns the database row ID for a static address script.
func (m *Manager) GetStaticAddressID(ctx context.Context,
	pkScript []byte) (int32, error) {

	return m.cfg.Store.GetStaticAddressID(ctx, pkScript)
}

// IsOurPkScript returns true if the pkScript belongs to an active static
// address.
func (m *Manager) IsOurPkScript(pkScript []byte) bool {
	return m.GetParameters(pkScript) != nil
}

// GetAllAddresses returns all persisted static address parameters.
func (m *Manager) GetAllAddresses(ctx context.Context) ([]*Parameters, error) {
	return m.cfg.Store.GetAllStaticAddresses(ctx)
}
