package address

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// changeActivationWallet counts key allocation and import side effects.
type changeActivationWallet struct {
	lndclient.WalletKitClient

	derivations int
	imports     int
	importErr   error
}

// DeriveNextKey records a key allocation and delegates to the wallet.
func (w *changeActivationWallet) DeriveNextKey(ctx context.Context,
	family int32) (*keychain.KeyDescriptor, error) {

	w.derivations++
	return w.WalletKitClient.DeriveNextKey(ctx, family)
}

// ImportTaprootScript records an import and can fail it before wallet changes.
func (w *changeActivationWallet) ImportTaprootScript(ctx context.Context,
	tapscript *waddrmgr.Tapscript) (btcutil.Address, error) {

	w.imports++
	if w.importErr != nil {
		return nil, w.importErr
	}
	return w.WalletKitClient.ImportTaprootScript(ctx, tapscript)
}

// newChangeActivationTest initializes a root before observing change issuance.
func newChangeActivationTest(t *testing.T) (*ManagerTestContext,
	*changeActivationWallet) {

	t.Helper()
	h := NewAddressManagerTestContext(t)
	wallet := &changeActivationWallet{
		WalletKitClient: h.manager.cfg.WalletKit,
	}
	h.manager.cfg.WalletKit = wallet
	_, err := h.manager.EnsureStaticAddressRoot(t.Context())
	require.NoError(t, err)
	return h, wallet
}

// TestPrepareChangeAddressDefersActivation verifies that preparing a rejected
// attempt consumes a key without adding a stored address or wallet watch.
func TestPrepareChangeAddressDefersActivation(t *testing.T) {
	t.Parallel()

	h, wallet := newChangeActivationTest(t)
	params, err := h.manager.PrepareChangeAddress(t.Context())
	require.NoError(t, err)
	require.Zero(t, params.ID)
	require.EqualValues(
		t, swap.StaticAddressChangeKeyFamily, params.KeyLocator.Family,
	)
	require.Nil(t, h.manager.GetParameters(params.PkScript))
	addresses, err := h.manager.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	require.Equal(t, 1, wallet.imports)
	require.Equal(t, 2, wallet.derivations)

	// Activation must preserve the exact descriptor already sent to the
	// server, while allocating only the database ID.
	expected := *params
	expected.PkScript = bytes.Clone(params.PkScript)
	require.NoError(t, h.manager.ActivateChangeAddress(t.Context(), params))
	require.Positive(t, params.ID)
	expected.ID = params.ID
	require.Equal(t, expected, *params)
	require.Same(t, params, h.manager.GetParameters(params.PkScript))
	addresses, err = h.manager.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 2)
	require.Equal(t, params, addresses[1])
	require.Equal(t, 2, wallet.imports)
	require.Equal(t, 2, wallet.derivations)

	// Retrying an already active address must leave its published metadata
	// immutable while deposit lookups read it.
	var readers sync.WaitGroup
	readers.Go(func() {
		for range 1000 {
			active := h.manager.GetParameters(params.PkScript)
			if active.ID != expected.ID {
				t.Errorf("active address ID changed: %d",
					active.ID)
			}
		}
	})
	err = h.manager.ActivateChangeAddress(t.Context(), params)
	readers.Wait()
	require.NoError(t, err)
}

// TestNewChangeAddressStillActivatesImmediately preserves the withdrawal API's
// immediate persistence and wallet watch registration.
func TestNewChangeAddressStillActivatesImmediately(t *testing.T) {
	t.Parallel()

	h, wallet := newChangeActivationTest(t)
	params, err := h.manager.NewChangeAddress(t.Context())
	require.NoError(t, err)
	require.Positive(t, params.ID)
	require.Same(t, params, h.manager.GetParameters(params.PkScript))
	addresses, err := h.manager.GetAllAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, addresses, 2)
	require.Equal(t, params, addresses[1])
	require.Equal(t, 2, wallet.imports)
	require.Equal(t, 2, wallet.derivations)
}

// TestActivateChangeAddressRejectsInvalidParams checks that invalid prepared
// parameters cannot insert an address row or import a wallet watch.
func TestActivateChangeAddressRejectsInvalidParams(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		mutate func(*AddressParameters) *AddressParameters
	}{
		{
			name: "nil",
			mutate: func(*AddressParameters) *AddressParameters {
				return nil
			},
		}, {
			name: "missing client key",
			mutate: func(p *AddressParameters) *AddressParameters {
				p.ClientPubkey = nil
				return p
			},
		}, {
			name: "missing server key",
			mutate: func(p *AddressParameters) *AddressParameters {
				p.ServerPubkey = nil
				return p
			},
		}, {
			name: "wrong family",
			mutate: func(p *AddressParameters) *AddressParameters {
				p.KeyLocator.Family = keychain.KeyFamily(
					swap.StaticSingleAddressKeyFamily,
				)
				return p
			},
		}, {
			name: "script mismatch",
			mutate: func(p *AddressParameters) *AddressParameters {
				p.PkScript = []byte{0x51}
				return p
			},
		}, {
			name: "unrelated database ID",
			mutate: func(p *AddressParameters) *AddressParameters {
				p.ID = 100
				return p
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, wallet := newChangeActivationTest(t)
			params, err := h.manager.PrepareChangeAddress(
				t.Context(),
			)
			require.NoError(t, err)
			pkScript := bytes.Clone(params.PkScript)
			err = h.manager.ActivateChangeAddress(
				t.Context(), tc.mutate(params),
			)
			require.Error(t, err)
			require.Nil(t, h.manager.GetParameters(pkScript))
			addresses, err := h.manager.GetAllAddresses(t.Context())
			require.NoError(t, err)
			require.Len(t, addresses, 1)
			require.Equal(t, 1, wallet.imports)
		})
	}
}

// TestChangeActivationImportFailureRetainsAddress keeps failed imports out of
// the active index and repairs the same stored address on retry or restart.
func TestChangeActivationImportFailureRetainsAddress(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		name := "retry"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h, wallet := newChangeActivationTest(t)
			params, err := h.manager.PrepareChangeAddress(
				t.Context(),
			)
			require.NoError(t, err)
			wallet.importErr = errors.New("wallet import failed")
			err = h.manager.ActivateChangeAddress(
				t.Context(), params,
			)
			require.ErrorIs(t, err, wallet.importErr)
			require.Positive(t, params.ID)
			require.Nil(t, h.manager.GetParameters(params.PkScript))
			id := params.ID
			addresses, err := h.manager.GetAllAddresses(t.Context())
			require.NoError(t, err)
			require.Len(t, addresses, 2)

			wallet.importErr = nil
			manager := h.manager
			if restart {
				manager, err = NewManager(h.manager.cfg, 100)
				require.NoError(t, err)
				err = manager.loadActiveAddresses(t.Context())
				require.NoError(t, err)
			} else {
				err = manager.ActivateChangeAddress(
					t.Context(), params,
				)
				require.NoError(t, err)
				require.Equal(t, 2, wallet.derivations)
			}
			recovered := manager.GetParameters(params.PkScript)
			require.Equal(t, params, recovered)
			require.Equal(t, id, recovered.ID)
			addresses, err = manager.GetAllAddresses(t.Context())
			require.NoError(t, err)
			require.Len(t, addresses, 2)
			require.Equal(t, 3, wallet.imports)
			h.mockStaticAddressClient.AssertNumberOfCalls(
				t, "ServerNewAddress", 1,
			)
		})
	}
}

// TestPreparedChangeAddressesAllowInterleavedIssuance checks that a pending
// server request cannot hold the issuance gate or change another request's key.
func TestPreparedChangeAddressesAllowInterleavedIssuance(t *testing.T) {
	t.Parallel()

	h, _ := newChangeActivationTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	first, err := h.manager.PrepareChangeAddress(ctx)
	require.NoError(t, err)
	receive, err := h.manager.NewReceiveAddress(ctx)
	require.NoError(t, err)
	second, err := h.manager.PrepareChangeAddress(ctx)
	require.NoError(t, err)
	require.Greater(t, second.KeyLocator.Index, first.KeyLocator.Index)
	require.NotEqual(t, first.PkScript, second.PkScript)
	require.NoError(t, h.manager.ActivateChangeAddress(ctx, second))
	require.NoError(t, h.manager.ActivateChangeAddress(ctx, first))
	require.Greater(t, first.ID, second.ID)
	require.Same(t, first, h.manager.GetParameters(first.PkScript))
	require.Same(t, second, h.manager.GetParameters(second.PkScript))
	require.Same(t, receive, h.manager.GetParameters(receive.PkScript))
	addresses, err := h.manager.GetAllAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, addresses, 4)
}
