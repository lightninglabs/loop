package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type riskDecisionPayloadNotification interface {
	proto.Message
	GetSwapHash() []byte
}

// TestManager_RiskDecisionPayloadBounded verifies that the replay cache and
// subscriber channels retain only an independently owned hash, regardless of
// unknown protobuf fields or excess capacity in the incoming hash slice.
func TestManager_RiskDecisionPayloadBounded(t *testing.T) {
	t.Parallel()

	for _, accepted := range []bool{true, false} {
		t.Run(staticLoopInRiskDecisionName(accepted), func(t *testing.T) {
			t.Parallel()

			for _, testCase := range []struct {
				name       string
				persistErr error
			}{
				{name: "persisted"},
				{
					name:       "unknown swap",
					persistErr: errors.New("swap not found"),
				},
			} {
				t.Run(testCase.name, func(t *testing.T) {
					t.Parallel()

					hash := lntypes.Hash{0x12, 0x34}
					persistCalls := 0
					mgr := NewManager(&Config{
						PersistStaticLoopInRiskDecision: func(
							_ context.Context, gotHash lntypes.Hash,
							gotAccepted bool) error {

							persistCalls++
							require.Equal(t, hash, gotHash)
							require.Equal(t, accepted, gotAccepted)
							return testCase.persistErr
						},
					})

					// A valid hash can alias a much larger allocation.
					backing := make([]byte, 1<<20)
					copy(backing, hash[:])
					var (
						response     *swapserverrpc.SubscribeNotificationsResponse
						source       riskDecisionPayloadNotification
						liveAccepted <-chan *swapserverrpc.
								ServerStaticLoopInRiskAcceptedNotification
						liveRejected <-chan *swapserverrpc.
								ServerStaticLoopInRiskRejectedNotification
					)
					if accepted {
						response = staticLoopInRiskAcceptedNotification(hash)
						message := response.GetStaticLoopInRiskAccepted()
						message.SwapHash = backing[:len(hash)]
						source = message
						liveAccepted = mgr.SubscribeStaticLoopInRiskAccepted(
							t.Context(), hash,
						)
					} else {
						response = staticLoopInRiskRejectedNotification(hash)
						message := response.GetStaticLoopInRiskRejected()
						message.SwapHash = backing[:len(hash)]
						source = message
						liveRejected = mgr.SubscribeStaticLoopInRiskRejected(
							t.Context(), hash,
						)
					}

					// Protobuf preserves unrecognized fields, including
					// arbitrarily large length-delimited values.
					unknown := protowire.AppendTag(nil, 100,
						protowire.BytesType)
					unknown = protowire.AppendBytes(unknown,
						make([]byte, 1<<20))
					source.ProtoReflect().SetUnknown(unknown)

					mgr.handleNotification(t.Context(), response)
					require.Equal(t, 1, persistCalls)
					require.Equal(t, unknown,
						[]byte(source.ProtoReflect().GetUnknown()))

					var cached riskDecisionPayloadNotification
					if accepted {
						cached = mgr.staticLoopInRiskAccepted[hash]
					} else {
						cached = mgr.staticLoopInRiskRejected[hash]
					}
					live := receiveRiskDecisionPayload(t,
						liveAccepted, liveRejected)

					// Mutating the original message must not alter the
					// cached or already delivered notification.
					backing[0] ^= 0xff
					assertRiskDecisionPayload(t, cached, hash)
					assertRiskDecisionPayload(t, live, hash)

					if accepted {
						liveAccepted = mgr.SubscribeStaticLoopInRiskAccepted(
							t.Context(), hash,
						)
					} else {
						liveRejected = mgr.SubscribeStaticLoopInRiskRejected(
							t.Context(), hash,
						)
					}
					replayed := receiveRiskDecisionPayload(t,
						liveAccepted, liveRejected)
					assertRiskDecisionPayload(t, replayed, hash)
				})
			}
		})
	}
}

func receiveRiskDecisionPayload(t *testing.T,
	accepted <-chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification,
	rejected <-chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification,
) riskDecisionPayloadNotification {

	t.Helper()

	select {
	case notification := <-accepted:
		return notification
	case notification := <-rejected:
		return notification
	case <-time.After(time.Second):
		t.Fatal("risk decision notification was not delivered")
		return nil
	}
}

func assertRiskDecisionPayload(t *testing.T,
	notification riskDecisionPayloadNotification, hash lntypes.Hash) {

	t.Helper()

	require.Equal(t, hash[:], notification.GetSwapHash())
	require.Equal(t, len(hash), cap(notification.GetSwapHash()))
	require.Empty(t, notification.ProtoReflect().GetUnknown())
}
