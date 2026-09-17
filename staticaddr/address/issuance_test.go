package address

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIssuanceWaiterCancellation verifies that canceling a queued root or
// derived-address request leaves the current issuer undisturbed and retryable.
func TestIssuanceWaiterCancellation(t *testing.T) {
	for _, derived := range []bool{false, true} {
		name := "root"
		if derived {
			name = "derived"
		}
		t.Run(name, func(t *testing.T) {
			fixture := NewAddressManagerTestContext(t)
			manager := fixture.manager
			issue := manager.EnsureStaticAddressRoot
			if derived {
				_, err := issue(t.Context())
				require.NoError(t, err)
				issue = manager.NewReceiveAddress
			}
			started, release := make(chan struct{}), make(chan struct{})
			manager.cfg.WalletKit = &blockingImportWallet{
				WalletKitClient: fixture.mockLnd.WalletKit,
				started:         started, release: release,
			}
			leaderDone := make(chan error, 1)
			go func() {
				_, err := issue(t.Context())
				leaderDone <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("issuer did not start")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			waiterDone := make(chan error, 1)
			go func() {
				_, err := issue(ctx)
				waiterDone <- err
			}()
			select {
			case err := <-waiterDone:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(time.Second):
				t.Fatal("canceled waiter blocked")
			}
			close(release)
			require.NoError(t, <-leaderDone)
			addresses, err := manager.GetAllAddresses(t.Context())
			require.NoError(t, err)
			expected := 1
			if derived {
				expected = 2
			}
			require.Len(t, addresses, expected)

			// A canceled waiter must not retain or release someone else's gate.
			manager.cfg.WalletKit = fixture.mockLnd.WalletKit
			_, err = issue(t.Context())
			require.NoError(t, err)
			fixture.mockStaticAddressClient.AssertNumberOfCalls(t, "ServerNewAddress", 1)
		})
	}
}

// TestConcurrentRootIssuance verifies that first callers share one durable root
// even though they independently acquire the retryable issuance gate.
func TestConcurrentRootIssuance(t *testing.T) {
	fixture := NewAddressManagerTestContext(t)
	type result struct {
		root *AddressParameters
		err  error
	}
	results := make(chan result, 8)
	for range cap(results) {
		go func() {
			root, err := fixture.manager.EnsureStaticAddressRoot(t.Context())
			results <- result{root: root, err: err}
		}()
	}
	var first *AddressParameters
	for range cap(results) {
		got := <-results
		require.NoError(t, got.err)
		if first == nil {
			first = got.root
		}
		require.Same(t, first, got.root)
	}
	fixture.mockStaticAddressClient.AssertNumberOfCalls(t, "ServerNewAddress", 1)
}
