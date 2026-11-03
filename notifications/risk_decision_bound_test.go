package notifications

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestManager_BoundedPendingRiskDecisions exercises the receive loop with a
// stream of distinct hashes. The replay cache must stay bounded regardless of
// whether a decision can be persisted.
func TestManager_BoundedPendingRiskDecisions(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		persist    bool
		persistErr error
	}{
		{
			name:       "unknown swaps",
			persist:    true,
			persistErr: errors.New("static loop in swap not found"),
		},
		{
			name:    "persisted decisions",
			persist: true,
		},
		{
			name: "no persistence callback",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			const numNotifications = 20 * maxPendingRiskDecisions
			recvChan := make(
				chan *swapserverrpc.SubscribeNotificationsResponse,
				numNotifications,
			)
			for i := range numNotifications {
				hash := riskDecisionTestHash(i)
				ntfn := staticLoopInRiskAcceptedNotification(hash)
				if i%2 != 0 {
					ntfn = staticLoopInRiskRejectedNotification(hash)
				}
				recvChan <- ntfn
			}
			close(recvChan)

			persistCalls := 0
			cfg := &Config{
				Client: &mockNotificationsClient{
					mockStream: &mockSubscribeNotificationsClient{
						recvChan: recvChan,
					},
				},
			}
			if testCase.persist {
				cfg.PersistStaticLoopInRiskDecision = func(
					_ context.Context, _ lntypes.Hash,
					_ bool) error {

					persistCalls++
					return testCase.persistErr
				}
			}
			mgr := NewManager(cfg)

			require.ErrorIs(t, mgr.subscribeNotifications(t.Context()),
				io.EOF)
			if testCase.persist {
				require.Equal(t, numNotifications, persistCalls)
			}

			mgr.Lock()
			require.Len(t, mgr.staticLoopInRiskPersisted,
				maxPendingRiskDecisions)
			require.Equal(t, maxPendingRiskDecisions,
				len(mgr.staticLoopInRiskAccepted)+
					len(mgr.staticLoopInRiskRejected))
			for hash, persisted := range mgr.staticLoopInRiskPersisted {
				_, accepted := mgr.staticLoopInRiskAccepted[hash]
				_, rejected := mgr.staticLoopInRiskRejected[hash]
				require.NotEqual(t, accepted, rejected)
				require.Equal(t, testCase.persistErr == nil,
					persisted)
			}
			mgr.Unlock()

			// A decision arriving before its subscriber still replays
			// even after the cache has reached capacity.
			acceptedHash := riskDecisionTestHash(numNotifications)
			mgr.handleNotification(t.Context(),
				staticLoopInRiskAcceptedNotification(acceptedHash))
			accepted := mgr.SubscribeStaticLoopInRiskAccepted(
				t.Context(), acceptedHash,
			)
			select {
			case ntfn := <-accepted:
				require.Equal(t, acceptedHash[:], ntfn.SwapHash)
			case <-time.After(time.Second):
				t.Fatal("accepted decision was not replayed at capacity")
			}

			rejectedHash := riskDecisionTestHash(numNotifications + 1)
			mgr.handleNotification(t.Context(),
				staticLoopInRiskRejectedNotification(rejectedHash))
			rejected := mgr.SubscribeStaticLoopInRiskRejected(
				t.Context(), rejectedHash,
			)
			select {
			case ntfn := <-rejected:
				require.Equal(t, rejectedHash[:], ntfn.SwapHash)
			case <-time.After(time.Second):
				t.Fatal("rejected decision was not replayed at capacity")
			}
		})
	}
}

// TestManager_RiskDecisionUpdatesAtCapacity verifies that replacing a cached
// decision does not evict another hash, including when its direction changes.
func TestManager_RiskDecisionUpdatesAtCapacity(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})
	for i := range maxPendingRiskDecisions {
		mgr.handleNotification(t.Context(),
			staticLoopInRiskAcceptedNotification(riskDecisionTestHash(i)))
	}

	hash := riskDecisionTestHash(0)
	for _, notification := range []*swapserverrpc.SubscribeNotificationsResponse{
		staticLoopInRiskAcceptedNotification(hash),
		staticLoopInRiskRejectedNotification(hash),
		staticLoopInRiskRejectedNotification(hash),
		staticLoopInRiskAcceptedNotification(hash),
	} {
		mgr.handleNotification(t.Context(), notification)
		require.Len(t, mgr.staticLoopInRiskPersisted,
			maxPendingRiskDecisions)
		for i := range maxPendingRiskDecisions {
			require.Contains(t, mgr.staticLoopInRiskPersisted,
				riskDecisionTestHash(i))
		}
		_, accepted := mgr.staticLoopInRiskAccepted[hash]
		_, rejected := mgr.staticLoopInRiskRejected[hash]
		require.Equal(t, notification.GetStaticLoopInRiskAccepted() != nil,
			accepted)
		require.NotEqual(t, accepted, rejected)
	}
}

// TestManager_RiskDecisionDeliveryAtCapacity ensures that cache eviction does
// not interfere with forwarding decisions to matching active subscribers.
func TestManager_RiskDecisionDeliveryAtCapacity(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{
		PersistStaticLoopInRiskDecision: func(_ context.Context,
			_ lntypes.Hash, _ bool) error {

			return errors.New("static loop in swap not found")
		},
	})
	for i := range maxPendingRiskDecisions {
		mgr.handleNotification(t.Context(),
			staticLoopInRiskAcceptedNotification(riskDecisionTestHash(i)))
	}

	hash := riskDecisionTestHash(maxPendingRiskDecisions)
	accepted := mgr.SubscribeStaticLoopInRiskAccepted(t.Context(), hash)
	rejected := mgr.SubscribeStaticLoopInRiskRejected(t.Context(), hash)
	mgr.handleNotification(t.Context(),
		staticLoopInRiskAcceptedNotification(hash))
	mgr.handleNotification(t.Context(),
		staticLoopInRiskRejectedNotification(hash))

	select {
	case ntfn := <-accepted:
		require.Equal(t, hash[:], ntfn.SwapHash)
	case <-time.After(time.Second):
		t.Fatal("accepted decision was not delivered at capacity")
	}
	select {
	case ntfn := <-rejected:
		require.Equal(t, hash[:], ntfn.SwapHash)
	case <-time.After(time.Second):
		t.Fatal("rejected decision was not delivered at capacity")
	}
}

func riskDecisionTestHash(index int) lntypes.Hash {
	var hash lntypes.Hash
	binary.BigEndian.PutUint64(hash[24:], uint64(index)+1)
	return hash
}
