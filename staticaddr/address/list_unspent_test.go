package address

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// listingWallet lets tests control the wallet snapshot and confirmation bounds.
type listingWallet struct {
	lndclient.WalletKitClient

	list func(context.Context, int32, int32) ([]*lnwallet.Utxo, error)
}

// ListUnspent returns the configured wallet snapshot.
func (w *listingWallet) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32, _ ...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	return w.list(ctx, minConfs, maxConfs)
}

// TestListUnspentMatchesActiveScripts checks filtering across addresses and
// verifies that the map lock is not held while waiting for the wallet RPC.
func TestListUnspentMatchesActiveScripts(t *testing.T) {
	first := &AddressParameters{PkScript: []byte{1}}
	second := &AddressParameters{PkScript: []byte{2}}
	started := make(chan struct{})
	release := make(chan struct{})
	var minConfs, maxConfs int32
	wallet := &listingWallet{list: func(ctx context.Context, minimum, maximum int32) (
		[]*lnwallet.Utxo, error) {

		minConfs, maxConfs = minimum, maximum
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []*lnwallet.Utxo{
			{PkScript: first.PkScript}, {PkScript: second.PkScript},
			{PkScript: []byte{3}},
		}, nil
	}}
	manager, err := NewManager(&ManagerConfig{WalletKit: wallet}, 1)
	require.NoError(t, err)
	manager.activeStaticAddresses[string(first.PkScript)] = first
	done := make(chan struct{})
	var utxos []*lnwallet.Utxo
	var listErr error
	go func() {
		utxos, listErr = manager.ListUnspent(t.Context(), 2, 100)
		close(done)
	}()
	<-started

	updated := make(chan struct{})
	go func() {
		manager.activeMu.Lock()
		manager.activeStaticAddresses[string(second.PkScript)] = second
		manager.activeMu.Unlock()
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("wallet RPC holds the map lock")
	}
	close(release)
	<-done
	require.NoError(t, listErr)
	require.Len(t, utxos, 2)
	require.EqualValues(t, 2, minConfs)
	require.EqualValues(t, 100, maxConfs)
	require.Equal(t, first.PkScript, utxos[0].PkScript)
	require.Equal(t, second.PkScript, utxos[1].PkScript)
}
