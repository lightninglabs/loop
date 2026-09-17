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
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// addressPageSize bounds temporary database results during activation.
	addressPageSize int32 = 256

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
	// activeMu guards the runtime script index and activated root.
	activeMu sync.Mutex

	cfg *ManagerConfig

	// issuanceGate serializes side effects while allowing waiting callers
	// to cancel independently of the operation currently in progress.
	issuanceGate chan struct{}

	currentHeight atomic.Int32

	// activeStaticAddresses is the runtime index used to match wallet UTXOs
	// to locally known static address parameters. The DB remains the
	// durable source of truth; this map is rebuilt from the DB on startup
	// and updated after successful address issuance. Keys are raw PkScript
	// bytes converted to strings for map lookup, not encoded Bitcoin addresses.
	activeStaticAddresses map[string]*AddressParameters

	// rootAddress is the lowest-ID active address, set together with the
	// script index under the activeMu mutex only after successful activation.
	rootAddress *AddressParameters
}

// NewManager creates a new address manager.
func NewManager(cfg *ManagerConfig, currentHeight int32) (*Manager, error) {
	if currentHeight <= 0 {
		return nil, fmt.Errorf("invalid current height %d",
			currentHeight)
	}

	m := &Manager{
		cfg:                   cfg,
		issuanceGate:          make(chan struct{}, 1),
		activeStaticAddresses: make(map[string]*AddressParameters),
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

// loadActiveAddresses rebuilds the runtime map in ID-ordered database pages.
// It publishes the complete index and root only after all pages and wallet
// imports succeed. Callers serialize loading with issuance or run it at startup.
func (m *Manager) loadActiveAddresses(ctx context.Context) error {
	active := make(map[string]*AddressParameters)
	var (
		root          *AddressParameters
		walletScripts map[string]struct{}
		afterID       int32
	)
	for {
		page, err := m.cfg.Store.ListStaticAddresses(ctx, afterID, addressPageSize)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		if walletScripts == nil {
			walletScripts, err = m.walletAddressScripts(ctx)
			if err != nil {
				return err
			}
		}
		for _, param := range page {
			if param == nil || param.ID <= afterID {
				return fmt.Errorf("invalid static address page after ID %d", afterID)
			}
			if _, ok := walletScripts[string(param.PkScript)]; !ok {
				staticAddress, err := staticAddressFromParams(param)
				if err != nil {
					return err
				}
				if err := m.importAddressTapscript(ctx, staticAddress); err != nil {
					return err
				}
			}
			active[string(param.PkScript)] = param
			if root == nil {
				root = param
			}
			afterID = param.ID
		}
		if len(page) < int(addressPageSize) {
			break
		}
	}
	m.activeMu.Lock()
	m.activeStaticAddresses = active
	m.rootAddress = root
	m.activeMu.Unlock()
	return nil
}

// walletAddressScripts returns all scripts currently watched by lnd's
// imported account. Map keys are raw script bytes converted to strings,
// not encoded Bitcoin addresses. ListAddresses is available at Loop's minimum
// supported lnd version and reconciles every address with one read RPC.
func (m *Manager) walletAddressScripts(ctx context.Context) (
	map[string]struct{}, error) {

	rpcCtx, rpcTimeout, walletClient :=
		m.cfg.WalletKit.RawClientWithMacAuth(ctx)
	if walletClient == nil {
		return nil, fmt.Errorf("missing raw wallet kit client")
	}

	if rpcTimeout > 0 {
		var cancel context.CancelFunc
		rpcCtx, cancel = context.WithTimeout(rpcCtx, rpcTimeout)
		defer cancel()
	}

	resp, err := walletClient.ListAddresses(
		rpcCtx, &walletrpc.ListAddressesRequest{
			AccountName: waddrmgr.ImportedAddrAccountName,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list imported wallet addresses: %w", err)
	}

	scriptKeys := make(map[string]struct{})
	for _, account := range resp.GetAccountWithAddresses() {
		for _, property := range account.GetAddresses() {
			addr, err := btcutil.DecodeAddress(
				property.GetAddress(), m.cfg.ChainParams,
			)
			if err != nil {
				return nil, fmt.Errorf("decode imported wallet "+
					"address: %w", err)
			}
			if !addr.IsForNet(m.cfg.ChainParams) {
				return nil, fmt.Errorf("imported wallet address is for " +
					"the wrong network")
			}

			pkScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return nil, fmt.Errorf("derive imported wallet "+
					"address script: %w", err)
			}

			scriptKeys[string(pkScript)] = struct{}{}
		}
	}

	return scriptKeys, nil
}

// NewAddress creates the next externally visible receive static address.
//
// The first call also makes sure the legacy/root static address exists,
// because receive and change addresses are derived from the server pubkey and
// expiry returned for that root.
func (m *Manager) NewAddress(ctx context.Context) (*btcutil.AddressTaproot,
	int64, error) {

	addrParams, err := m.NewReceiveAddress(ctx)
	if err != nil {
		return nil, 0, err
	}

	address, err := m.GetTaprootAddress(
		addrParams.ClientPubkey, addrParams.ServerPubkey,
		int64(addrParams.Expiry),
	)
	if err != nil {
		return nil, 0, err
	}

	return address, int64(addrParams.Expiry), nil
}

// lockIssuance waits for exclusive issuance access without trapping canceled
// callers behind a slow dependency. Cancellation never releases another
// caller's gate or starts an overlapping initialization attempt.
func (m *Manager) lockIssuance(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.issuanceGate <- struct{}{}:
		// If acquisition raced with cancellation, avoid starting side effects.
		if err := ctx.Err(); err != nil {
			m.unlockIssuance()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlockIssuance releases the gate held by the current issuing caller.
func (m *Manager) unlockIssuance() {
	<-m.issuanceGate
}

// EnsureStaticAddressRoot loads or creates the legacy/root static address
// parameters. The root address is the only address that requires a
// ServerNewAddress call; all receive/change addresses derive client keys
// locally and reuse the root address's server pubkey and expiry.
func (m *Manager) EnsureStaticAddressRoot(ctx context.Context) (*AddressParameters,
	error) {

	m.activeMu.Lock()
	root := m.legacyParameters()
	m.activeMu.Unlock()
	if root != nil {
		return root, nil
	}

	if err := m.lockIssuance(ctx); err != nil {
		return nil, err
	}
	defer m.unlockIssuance()

	// Another caller may have created the root while we were waiting for the
	// issuance lock.
	m.activeMu.Lock()
	root = m.legacyParameters()
	m.activeMu.Unlock()
	if root != nil {
		return root, nil
	}

	err := m.loadActiveAddresses(ctx)
	if err != nil {
		return nil, err
	}
	m.activeMu.Lock()
	root = m.legacyParameters()
	m.activeMu.Unlock()
	if root != nil {
		return root, nil
	}

	// We are fetching a new L402 token from the server. The returned server
	// key and expiry are shared by all future client-derived addresses for
	// this L402.
	err = m.cfg.FetchL402(ctx)
	if err != nil {
		return nil, err
	}

	clientPubKey, err := m.cfg.WalletKit.DeriveNextKey(
		ctx, swap.StaticSingleAddressKeyFamily,
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
func (m *Manager) NewReceiveAddress(ctx context.Context) (*AddressParameters,
	error) {

	root, err := m.EnsureStaticAddressRoot(ctx)
	if err != nil {
		return nil, err
	}

	return m.newDerivedAddress(ctx, root, swap.StaticMultiAddressKeyFamily)
}

// NewChangeAddress derives, stores, imports and activates the next change
// family static address. Swap and withdrawal code calls this before submitting
// requests that require change.
func (m *Manager) NewChangeAddress(ctx context.Context) (*AddressParameters,
	error) {

	root, err := m.EnsureStaticAddressRoot(ctx)
	if err != nil {
		return nil, err
	}

	return m.newDerivedAddress(ctx, root, swap.StaticAddressChangeKeyFamily)
}

// newDerivedAddress derives a client key in the requested family and creates
// an address using the root address's server key, expiry and protocol version.
func (m *Manager) newDerivedAddress(ctx context.Context, root *AddressParameters,
	keyFamily int32) (*AddressParameters, error) {

	if err := m.lockIssuance(ctx); err != nil {
		return nil, err
	}
	defer m.unlockIssuance()

	clientPubKey, err := m.cfg.WalletKit.DeriveNextKey(ctx, keyFamily)
	if err != nil {
		return nil, err
	}

	return m.createAddressFromKey(
		ctx, clientPubKey, root.ServerPubkey, root.Expiry,
		root.ProtocolVersion,
	)
}

// createAddressFromKey persists the address before importing its wallet watch
// and adding it to the active script index.
func (m *Manager) createAddressFromKey(ctx context.Context,
	clientPubKey *keychain.KeyDescriptor, serverPubKey *btcec.PublicKey,
	expiry uint32, protocolVersion version.AddressProtocolVersion) (
	*AddressParameters, error) {

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

	addrParams := &AddressParameters{
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

	// Persist the address before importing it into lnd. In particular, the
	// server has already committed a root at this point, so retaining the
	// client key locator lets a later retry repair a failed wallet import
	// instead of deriving a different root key.
	err = m.cfg.Store.CreateStaticAddress(ctx, addrParams)
	if err != nil {
		return nil, err
	}

	addrParams.ID, err = m.cfg.Store.GetStaticAddressID(ctx, pkScript)
	if err != nil {
		return nil, err
	}

	err = m.importAddressTapscript(ctx, staticAddress)
	if err != nil {
		return nil, err
	}

	m.activeMu.Lock()
	m.activeStaticAddresses[string(pkScript)] = addrParams
	if m.rootAddress == nil || addrParams.ID < m.rootAddress.ID {
		m.rootAddress = addrParams
	}
	m.activeMu.Unlock()

	return addrParams, nil
}

// validateServerAddressParams validates the server-controlled static address
// parameters before they are committed into the address script or database.
func validateServerAddressParams(
	addrParams *staticaddressrpc.ServerAddressParameters) error {

	if addrParams == nil {
		return fmt.Errorf("missing server address parameters")
	}

	serverKey := addrParams.GetServerKey()
	if len(serverKey) == 0 {
		return fmt.Errorf("missing server public key")
	}
	if !btcec.IsCompressedPubKey(serverKey) {
		return fmt.Errorf("server public key is not a compressed " +
			"secp256k1 public key")
	}

	expiry := addrParams.GetExpiry()
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

// importAddressTapscript imports the address's timeout tree into the wallet.
// An existing import is accepted only when it identifies the same output key.
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
		// expected on restart. Lnd currently returns this as an untyped gRPC
		// error, so also match the expected output key.
		duplicateErr := fmt.Sprintf(
			"address for script hash/key %x already exists",
			schnorr.SerializePubKey(staticAddress.TaprootKey),
		)
		if strings.Contains(err.Error(), duplicateErr) {
			log.Infof("Static address tapscript already imported")
			return nil
		}

		return err
	}

	log.Infof("Imported static address taproot script to lnd wallet: %v",
		addr)

	return nil
}

// staticAddressFromParams reconstructs the spending script from one address's
// client key, server key and expiry.
func staticAddressFromParams(addrParams *AddressParameters) (*script.StaticAddress,
	error) {

	if addrParams == nil {
		return nil, fmt.Errorf("missing static address parameters")
	}

	return script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(addrParams.Expiry),
		addrParams.ClientPubkey, addrParams.ServerPubkey,
	)
}

// legacyParameters returns the cached active legacy/root address.
// The caller must hold the activeMu mutex.
func (m *Manager) legacyParameters() *AddressParameters {
	return m.rootAddress
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

// GetTaprootAddressFromScript encodes a canonical P2TR output script for the
// configured network without reconstructing its keys or script tree.
func (m *Manager) GetTaprootAddressFromScript(pkScript []byte) (
	*btcutil.AddressTaproot, error) {

	if !txscript.IsPayToTaproot(pkScript) {
		return nil, fmt.Errorf("invalid static address P2TR script: %x", pkScript)
	}

	return btcutil.NewAddressTaproot(pkScript[2:], m.cfg.ChainParams)
}

// ListUnspent returns wallet UTXOs matching any active static address
// within the requested confirmation range.
func (m *Manager) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	m.activeMu.Lock()
	empty := len(m.activeStaticAddresses) == 0
	m.activeMu.Unlock()
	if empty {
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
	m.activeMu.Lock()
	defer m.activeMu.Unlock()

	var filteredUtxos []*lnwallet.Utxo
	for _, utxo := range utxos {
		if _, ok := m.activeStaticAddresses[string(utxo.PkScript)]; ok {
			filteredUtxos = append(filteredUtxos, utxo)
		}
	}

	return filteredUtxos, nil
}

// GetStaticAddressParameters returns the legacy/root static-address
// parameters.
func (m *Manager) GetStaticAddressParameters(ctx context.Context) (
	*script.Parameters, error) {

	addrParams, err := m.GetLegacyParameters(ctx)
	if err != nil {
		return nil, err
	}

	if addrParams == nil {
		return nil, ErrNoStaticAddress
	}

	return addrParams, nil
}

// GetStaticAddress returns a taproot address for the given client and server
// public keys and expiry.
func (m *Manager) GetStaticAddress(ctx context.Context) (*script.StaticAddress,
	error) {

	addrParams, err := m.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}

	return staticAddressFromParams(addrParams)
}

// GetLegacyParameters returns the legacy/root static address parameters.
func (m *Manager) GetLegacyParameters(ctx context.Context) (*AddressParameters,
	error) {

	addrParams, err := m.cfg.Store.GetLegacyParameters(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return addrParams, nil
}

// GetParameters returns active static address parameters for a pkScript.
func (m *Manager) GetParameters(pkScript []byte) *AddressParameters {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()

	return m.activeStaticAddresses[string(pkScript)]
}

// GetStaticAddressID returns the database row ID for a static address script.
func (m *Manager) GetStaticAddressID(ctx context.Context,
	pkScript []byte) (int32, error) {

	return m.cfg.Store.GetStaticAddressID(ctx, pkScript)
}

// GetAllAddresses returns all persisted static address parameters.
func (m *Manager) GetAllAddresses(ctx context.Context) ([]*AddressParameters,
	error) {

	return m.cfg.Store.GetAllStaticAddresses(ctx)
}
