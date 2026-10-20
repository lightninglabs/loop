package deposit

import (
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

func TestAssetRegistrationSharesBitcoinExclusion(t *testing.T) {
	manager := &Manager{deposits: make(map[wire.OutPoint]*Deposit)}
	point := wire.OutPoint{Index: 1}
	utxo := &lnwallet.Utxo{OutPoint: point}
	require.Len(t, manager.filterNewDeposits([]*lnwallet.Utxo{utxo}), 1)
	failure := errors.New("disk unavailable")
	require.ErrorIs(t, manager.RegisterAssetDeposit(
		t.Context(), point.String(), func() error { return failure },
	), failure)
	require.Len(t, manager.filterNewDeposits([]*lnwallet.Utxo{utxo}), 1)
	require.NoError(t, manager.RegisterAssetDeposit(
		t.Context(), point.String(), func() error { return nil },
	))
	require.Empty(t, manager.filterNewDeposits([]*lnwallet.Utxo{utxo}))
	points := []wire.OutPoint{point}
	fsms, deposits := manager.toActiveDeposits(&points)
	require.Empty(t, fsms)
	require.Empty(t, deposits)

	bitcoin := wire.OutPoint{Index: 2}
	manager.deposits[bitcoin] = &Deposit{OutPoint: bitcoin}
	called := false
	err := manager.RegisterAssetDeposit(t.Context(), bitcoin.String(),
		func() error { called = true; return nil })
	require.Error(t, err)
	require.False(t, called)
}

func TestAssetRegistrationRejectsNoncanonicalOutpoint(t *testing.T) {
	manager := &Manager{}
	point := wire.OutPoint{Index: 1}
	point.Hash[0] = 0xab
	for _, value := range []string{
		strings.ToUpper(point.String()),
		strings.TrimSuffix(point.String(), ":1") + ":01",
	} {
		require.Error(t, manager.RegisterAssetDeposit(
			t.Context(), value, func() error {
				t.Fatal("persisted a noncanonical outpoint")
				return nil
			},
		))
	}
}
