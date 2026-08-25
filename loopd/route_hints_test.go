package loopd

import (
	"math"
	"testing"

	"github.com/lightninglabs/loop/swapserverrpc"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// TestUnmarshallRouteHintsValidation verifies malformed RPC route hints are
// rejected before they reach invoice creation.
func TestUnmarshallRouteHintsValidation(t *testing.T) {
	validHop := func() *swapserverrpc.HopHint {
		return &swapserverrpc.HopHint{
			NodeId:          mock_lnd.NewMockLnd().NodePubkey,
			ChanId:          1,
			CltvExpiryDelta: 80,
		}
	}

	testCases := []struct {
		name        string
		routeHints  []*swapserverrpc.RouteHint
		errContains string
	}{
		{
			name: "valid",
			routeHints: []*swapserverrpc.RouteHint{
				{
					HopHints: []*swapserverrpc.HopHint{
						validHop(),
					},
				},
			},
		},
		{
			name: "nil route hint",
			routeHints: []*swapserverrpc.RouteHint{
				nil,
			},
			errContains: "route hint 0 is nil",
		},
		{
			name: "empty route hint",
			routeHints: []*swapserverrpc.RouteHint{
				{},
			},
			errContains: "route hint 0 has no hop hints",
		},
		{
			name: "nil hop hint",
			routeHints: []*swapserverrpc.RouteHint{
				{
					HopHints: []*swapserverrpc.HopHint{nil},
				},
			},
			errContains: "hop hint 0 in route hint 0 is nil",
		},
		{
			name: "invalid node ID",
			routeHints: []*swapserverrpc.RouteHint{
				{
					HopHints: []*swapserverrpc.HopHint{
						{
							NodeId:          "invalid",
							ChanId:          1,
							CltvExpiryDelta: 80,
						},
					},
				},
			},
			errContains: "invalid hop hint 0 in route hint 0",
		},
		{
			name: "zero channel ID",
			routeHints: []*swapserverrpc.RouteHint{
				{
					HopHints: []*swapserverrpc.HopHint{
						{
							NodeId:          mock_lnd.NewMockLnd().NodePubkey,
							CltvExpiryDelta: 80,
						},
					},
				},
			},
			errContains: "hop hint 0 in route hint 0 has zero " +
				"channel ID",
		},
		{
			name: "zero CLTV delta",
			routeHints: []*swapserverrpc.RouteHint{
				{
					HopHints: []*swapserverrpc.HopHint{
						{
							NodeId: mock_lnd.NewMockLnd().NodePubkey,
							ChanId: 1,
						},
					},
				},
			},
			errContains: "hop hint 0 in route hint 0 has zero " +
				"CLTV expiry delta",
		},
		{
			name: "CLTV delta overflow",
			routeHints: []*swapserverrpc.RouteHint{
				{
					HopHints: []*swapserverrpc.HopHint{
						{
							NodeId:          mock_lnd.NewMockLnd().NodePubkey,
							ChanId:          1,
							CltvExpiryDelta: math.MaxUint16 + 1,
						},
					},
				},
			},
			errContains: "CLTV expiry delta exceeds 65535",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			routeHints, err := unmarshallRouteHints(
				testCase.routeHints,
			)
			if testCase.errContains != "" {
				require.ErrorContains(t, err, testCase.errContains)

				return
			}

			require.NoError(t, err)
			require.Len(t, routeHints, 1)
			require.Len(t, routeHints[0], 1)
			require.Equal(t, uint64(1), routeHints[0][0].ChannelID)
			require.Equal(
				t, uint16(80), routeHints[0][0].CLTVExpiryDelta,
			)
		})
	}
}
