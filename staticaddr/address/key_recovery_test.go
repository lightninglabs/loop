package address

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// recoveryWallet models lnd's durable, independent family counters.
type recoveryWallet struct {
	lndclient.WalletKitClient

	next         map[keychain.KeyFamily]uint32
	calls        map[keychain.KeyFamily]int
	listCalls    int
	verifyCalls  int
	listErr      error
	deriveErr    error
	verifyErr    error
	wrongWallet  bool
	badNext      bool
	afterDerive  func()
	extraAccount *walletrpc.Account
}

// ListAccounts exposes next indices without incrementing them.
func (w *recoveryWallet) ListAccounts(ctx context.Context, name string,
	addrType walletrpc.AddressType) ([]*walletrpc.Account, error) {

	w.listCalls++
	if name != "" || addrType != walletrpc.AddressType_UNKNOWN {
		return nil, errors.New("filtered account request")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w.listErr != nil {
		return nil, w.listErr
	}
	var accounts []*walletrpc.Account
	for family, next := range w.next {
		accounts = append(accounts, &walletrpc.Account{
			DerivationPath:   fmt.Sprintf("m/1017'/1'/%d'", family),
			ExternalKeyCount: next,
		})
	}
	if w.extraAccount != nil {
		accounts = append(accounts, w.extraAccount)
	}
	return accounts, nil
}

// DeriveKey verifies identity without changing the next index.
func (w *recoveryWallet) DeriveKey(_ context.Context,
	locator *keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	w.verifyCalls++
	if w.verifyErr != nil {
		return nil, w.verifyErr
	}
	key := recoveryKey(locator.Family, locator.Index)
	if w.wrongWallet {
		_, key.PubKey = test.CreateKey(0)
	}
	return key, nil
}

// DeriveNextKey advances only the requested family's counter.
func (w *recoveryWallet) DeriveNextKey(_ context.Context, family int32) (
	*keychain.KeyDescriptor, error) {

	f := keychain.KeyFamily(family)
	w.calls[f]++
	if w.deriveErr != nil {
		return nil, w.deriveErr
	}
	key := recoveryKey(f, w.next[f])
	if w.badNext {
		key.Index = 0
		return key, nil
	}
	w.next[f]++
	if w.afterDerive != nil {
		w.afterDerive()
	}
	return key, nil
}

// recoveryKey provides deterministic keys for each family and index.
func recoveryKey(family keychain.KeyFamily, index uint32) *keychain.KeyDescriptor {
	_, pubKey := test.CreateKey(int32(family) + int32(index))
	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{Family: family, Index: index},
		PubKey:     pubKey,
	}
}

// recoveryAddress provides a persisted address's key metadata.
func recoveryAddress(family keychain.KeyFamily, index uint32) *AddressParameters {
	key := recoveryKey(family, index)
	return &AddressParameters{
		KeyLocator: key.KeyLocator, ClientPubkey: key.PubKey,
	}
}

// TestReconcileKeyIndices checks off-by-one boundaries, gaps, missing accounts,
// separate receive/change counters, and read-only normal restarts.
func TestReconcileKeyIndices(t *testing.T) {
	t.Parallel()

	receive := keychain.KeyFamily(swap.StaticMultiAddressKeyFamily)
	change := keychain.KeyFamily(swap.StaticAddressChangeKeyFamily)
	for _, tc := range []struct {
		name     string
		next     uint32
		missing  bool
		wantNext uint32
		wantKeys int
	}{
		{name: "behind", next: 2, wantNext: 6, wantKeys: 4},
		{name: "next equals last used", next: 5, wantNext: 6, wantKeys: 1},
		{name: "synchronized", next: 6, wantNext: 6},
		{name: "ahead", next: 9, wantNext: 9},
		{name: "missing account", missing: true, wantNext: 6, wantKeys: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wallet := &recoveryWallet{
				next:  map[keychain.KeyFamily]uint32{change: 3},
				calls: make(map[keychain.KeyFamily]int),
				// An unrelated scope must not mask the real counter.
				extraAccount: &walletrpc.Account{
					DerivationPath:   "m/84'/1'/42061'",
					ExternalKeyCount: 99,
				},
			}
			if !tc.missing {
				wallet.next[receive] = tc.next
			}
			manager := &Manager{cfg: &ManagerConfig{
				WalletKit:   wallet,
				ChainParams: &chaincfg.RegressionNetParams,
			}}
			addresses := staticAddressKeyMaxima{
				receive: recoveryAddress(receive, 5),
				change:  recoveryAddress(change, 2),
			}
			require.NoError(t, manager.reconcileKeyIndices(t.Context(), addresses))
			require.Equal(t, tc.wantNext, wallet.next[receive])
			require.Equal(t, tc.wantKeys, wallet.calls[receive])
			require.EqualValues(t, 3, wallet.next[change])
			require.Zero(t, wallet.calls[change])
			require.Equal(t, 1, wallet.listCalls)
			if tc.wantKeys == 0 {
				require.Zero(t, wallet.verifyCalls)
			} else {
				require.Equal(t, 1, wallet.verifyCalls)
			}

			// Repeating startup must not consume an additional key.
			require.NoError(t, manager.reconcileKeyIndices(t.Context(), addresses))
			require.Equal(t, tc.wantKeys, wallet.calls[receive])
			require.Len(t, addresses, 2)
		})
	}
}

// TestReconcileKeyIndicesErrors ensures errors never masquerade as an empty
// wallet and invalid responses cannot keep recovery in an endless loop.
func TestReconcileKeyIndicesErrors(t *testing.T) {
	t.Parallel()

	family := keychain.KeyFamily(swap.StaticAddressChangeKeyFamily)
	for _, failure := range []string{
		"listing", "verification", "wallet mismatch", "derivation",
		"non advancing", "duplicate account",
	} {
		t.Run(failure, func(t *testing.T) {
			wallet := &recoveryWallet{
				next:  map[keychain.KeyFamily]uint32{family: 2},
				calls: make(map[keychain.KeyFamily]int),
			}
			sentinel := errors.New("wallet unavailable")
			switch failure {
			case "listing":
				wallet.listErr = sentinel
			case "verification":
				wallet.verifyErr = sentinel
			case "wallet mismatch":
				wallet.wrongWallet = true
			case "derivation":
				wallet.deriveErr = sentinel
			case "non advancing":
				wallet.badNext = true
			case "duplicate account":
				wallet.extraAccount = &walletrpc.Account{
					DerivationPath:   "m/1017'/1'/42062'",
					ExternalKeyCount: 100,
				}
			}
			manager := &Manager{cfg: &ManagerConfig{
				WalletKit:   wallet,
				ChainParams: &chaincfg.RegressionNetParams,
			}}
			err := manager.reconcileKeyIndices(t.Context(),
				staticAddressKeyMaxima{
					family: recoveryAddress(family, 5),
				})
			require.Error(t, err)
			require.EqualValues(t, 2, wallet.next[family])
			if wallet.listErr != nil || wallet.verifyErr != nil ||
				wallet.deriveErr != nil {

				require.ErrorIs(t, err, sentinel)
			}
			if wallet.deriveErr == nil && !wallet.badNext {
				require.Zero(t, wallet.calls[family])
			}
		})
	}
}

// TestReconcileKeyIndicesResume checks that cancellation preserves progress in
// lnd and the next startup completes without creating Loop address records.
func TestReconcileKeyIndicesResume(t *testing.T) {
	t.Parallel()

	family := keychain.KeyFamily(swap.StaticAddressChangeKeyFamily)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wallet := &recoveryWallet{
		next:        map[keychain.KeyFamily]uint32{family: 0},
		calls:       make(map[keychain.KeyFamily]int),
		afterDerive: cancel,
	}
	manager := &Manager{cfg: &ManagerConfig{
		WalletKit: wallet, ChainParams: &chaincfg.RegressionNetParams,
	}}
	addresses := staticAddressKeyMaxima{
		family: recoveryAddress(family, 3),
	}
	require.ErrorIs(t, manager.reconcileKeyIndices(ctx, addresses), context.Canceled)
	require.EqualValues(t, 1, wallet.next[family])
	wallet.afterDerive = nil
	require.NoError(t, manager.reconcileKeyIndices(t.Context(), addresses))
	require.EqualValues(t, 4, wallet.next[family])
	require.Equal(t, 4, wallet.calls[family])
}

// TestReconcileKeyIndicesNoAddresses avoids introducing wallet RPC
// dependencies when no static address has been persisted.
func TestReconcileKeyIndicesNoAddresses(t *testing.T) {
	t.Parallel()

	manager := &Manager{cfg: &ManagerConfig{}}
	require.NoError(t, manager.reconcileKeyIndices(t.Context(), nil))
	require.NoError(t, manager.reconcileKeyIndices(
		t.Context(), make(staticAddressKeyMaxima),
	))
}

// TestReconcileKeyIndicesLegacyFamily checks that a restored wallet advances
// the legacy family past both the root address and every static loop-in HTLC
// key, so neither the root key nor an earlier HTLC key is handed out again.
func TestReconcileKeyIndicesLegacyFamily(t *testing.T) {
	t.Parallel()

	legacy := keychain.KeyFamily(swap.StaticSingleAddressKeyFamily)
	for _, tc := range []struct {
		name      string
		next      uint32
		htlcIndex uint32
		hasHtlc   bool
		wantNext  uint32
		wantKeys  int
	}{
		{name: "restored root only", wantNext: 1, wantKeys: 1},
		{
			name: "restored with htlc keys", htlcIndex: 3,
			hasHtlc: true, wantNext: 4, wantKeys: 4,
		},
		{
			name: "htlc keys partially restored", next: 2,
			htlcIndex: 3, hasHtlc: true, wantNext: 4, wantKeys: 2,
		},
		{
			name: "synchronized", next: 4, htlcIndex: 3,
			hasHtlc: true, wantNext: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wallet := &recoveryWallet{
				next:  map[keychain.KeyFamily]uint32{legacy: tc.next},
				calls: make(map[keychain.KeyFamily]int),
			}
			store := &addressListStore{
				htlcIndex: tc.htlcIndex, hasHtlcIndex: tc.hasHtlc,
			}
			manager := &Manager{cfg: &ManagerConfig{
				Store: store, WalletKit: wallet,
				ChainParams: &chaincfg.RegressionNetParams,
			}}
			lastKeys := make(staticAddressKeyMaxima)
			require.NoError(t, lastKeys.observe(
				recoveryAddress(legacy, 0),
			))
			require.Len(t, lastKeys, 1)

			require.NoError(t, manager.reconcileKeyIndices(
				t.Context(), lastKeys,
			))
			require.Equal(t, tc.wantNext, wallet.next[legacy])
			require.Equal(t, tc.wantKeys, wallet.calls[legacy])

			// Identity is always verified against the root key.
			if tc.wantKeys == 0 {
				require.Zero(t, wallet.verifyCalls)
			} else {
				require.Equal(t, 1, wallet.verifyCalls)
			}

			// Repeating startup must not consume an additional key.
			require.NoError(t, manager.reconcileKeyIndices(
				t.Context(), lastKeys,
			))
			require.Equal(t, tc.wantKeys, wallet.calls[legacy])
		})
	}
}

// TestReconcileKeyIndicesLegacyFamilyErrors ensures invalid or unavailable HTLC
// key records abort startup before any key is consumed.
func TestReconcileKeyIndicesLegacyFamilyErrors(t *testing.T) {
	t.Parallel()

	legacy := keychain.KeyFamily(swap.StaticSingleAddressKeyFamily)
	sentinel := errors.New("database unavailable")
	for _, store := range []*addressListStore{
		{htlcErr: sentinel},
		{htlcIndex: hdkeychain.HardenedKeyStart, hasHtlcIndex: true},
	} {
		wallet := &recoveryWallet{
			next:  map[keychain.KeyFamily]uint32{legacy: 0},
			calls: make(map[keychain.KeyFamily]int),
		}
		manager := &Manager{cfg: &ManagerConfig{
			Store: store, WalletKit: wallet,
			ChainParams: &chaincfg.RegressionNetParams,
		}}
		err := manager.reconcileKeyIndices(t.Context(),
			staticAddressKeyMaxima{legacy: recoveryAddress(legacy, 0)})
		require.Error(t, err)
		if store.htlcErr != nil {
			require.ErrorIs(t, err, sentinel)
		}
		require.Zero(t, wallet.calls[legacy])
		require.Zero(t, wallet.next[legacy])
	}
}

// TestLoadActiveAddressesReconcilesKeys verifies that failed reconciliation
// cannot publish a partially ready address index, and a retry repairs both
// family counters without inserting any new addresses. Later rows with lower
// key indices must not replace the maxima for either family.
func TestLoadActiveAddressesReconcilesKeys(t *testing.T) {
	t.Parallel()

	params := []*AddressParameters{
		recoveryAddress(keychain.KeyFamily(swap.StaticMultiAddressKeyFamily), 5),
		recoveryAddress(keychain.KeyFamily(swap.StaticAddressChangeKeyFamily), 2),
		recoveryAddress(keychain.KeyFamily(swap.StaticMultiAddressKeyFamily), 1),
		recoveryAddress(keychain.KeyFamily(swap.StaticAddressChangeKeyFamily), 1),
	}
	for i, p := range params {
		p.ID = int32(i + 1)
		p.ServerPubkey = defaultServerPubkey
		p.Expiry = defaultExpiry
		addr, err := staticAddressFromParams(p)
		require.NoError(t, err)
		p.PkScript, err = addr.StaticAddressScript()
		require.NoError(t, err)
	}
	store := &addressListStore{addresses: params}
	wallet := &recoveryWallet{
		WalletKitClient: &addressListWallet{
			rawClient: &listAddressesClient{
				response: &walletrpc.ListAddressesResponse{},
			},
		},
		next:    make(map[keychain.KeyFamily]uint32),
		calls:   make(map[keychain.KeyFamily]int),
		listErr: errors.New("wallet listing unavailable"),
	}
	manager, err := NewManager(&ManagerConfig{
		Store: store, WalletKit: wallet,
		ChainParams: &chaincfg.RegressionNetParams,
	}, 1)
	require.NoError(t, err)
	require.ErrorIs(t, manager.loadActiveAddresses(t.Context()), wallet.listErr)
	require.Empty(t, manager.activeStaticAddresses)
	require.Nil(t, manager.rootAddress)

	wallet.listErr = nil
	require.NoError(t, manager.loadActiveAddresses(t.Context()))
	require.Len(t, manager.activeStaticAddresses, len(params))
	require.Len(t, store.addresses, len(params))
	require.EqualValues(t, 6, wallet.next[keychain.KeyFamily(
		swap.StaticMultiAddressKeyFamily,
	)])
	require.EqualValues(t, 3, wallet.next[keychain.KeyFamily(
		swap.StaticAddressChangeKeyFamily,
	)])
	for _, p := range params {
		require.Same(t, p, manager.GetParameters(p.PkScript))
	}
}
