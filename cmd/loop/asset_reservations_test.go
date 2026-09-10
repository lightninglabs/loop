package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/loop/assets/reservation"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type reservationListClient struct {
	looprpc.AssetReservationsClient

	t              *testing.T
	wantState      string
	wantActiveOnly bool
	calls          int
}

func (c *reservationListClient) List(_ context.Context,
	req *looprpc.ListClientAssetReservationsRequest, _ ...grpc.CallOption) (
	*looprpc.ListClientAssetReservationsResponse, error) {

	c.t.Helper()
	require.Equal(c.t, c.wantState, req.State)
	require.Equal(c.t, c.wantActiveOnly, req.ActiveOnly)
	c.calls++
	page := &looprpc.ListClientAssetReservationsResponse{
		Reservations: []*looprpc.ClientAssetReservation{{Amount: uint64(c.calls)}},
	}
	if c.calls == 1 {
		require.Empty(c.t, req.AfterId)
		page.NextAfterId = make([]byte, 32)
		page.NextAfterId[0] = 1
	} else {
		require.Equal(c.t, 2, c.calls)
		require.Len(c.t, req.AfterId, 32)
		require.EqualValues(c.t, 1, req.AfterId[0])
	}
	return page, nil
}

func TestAssetReservationListStates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		state      string
		activeOnly bool
		conflict   bool
	}{
		{name: "all states"},
		{
			name: "single state", args: []string{"--state", "Ready"},
			state: "Ready",
		},
		{
			name: "active only", args: []string{"--active_only"},
			activeOnly: true,
		},
		{
			name:     "repeated state",
			args:     []string{"--state", "Ready", "--state", "AwaitApproval"},
			conflict: true,
		},
		{
			name:     "conflicting filters",
			args:     []string{"--state", "Ready", "--active_only"},
			conflict: true,
		},
		{
			name:     "explicit false still conflicts",
			args:     []string{"--state", "Ready", "--active_only=false"},
			conflict: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := assetCommand.Commands[0].Commands[1]
			require.Equal(t, "list", list.Name)
			flags, groups := cloneFlagsWithGroups(
				list.Flags, list.MutuallyExclusiveFlags,
			)
			client := &reservationListClient{
				t: t, wantState: tc.state, wantActiveOnly: tc.activeOnly,
			}
			cmd := &cli.Command{
				Name: "list", Flags: flags, MutuallyExclusiveFlags: groups,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := loadAssetReservations(ctx, client,
						cmd.String("state"), cmd.Bool("active_only"))
					if err != nil {
						return err
					}
					require.Len(t, result.Reservations, 2)
					require.Equal(t, 2, client.calls)
					return nil
				},
			}
			err := cmd.Run(t.Context(), append([]string{"list"}, tc.args...))
			if tc.conflict {
				require.Error(t, err)
				require.Zero(t, client.calls)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestReservationFeeInfo(t *testing.T) {
	for _, tc := range []struct {
		amount, fee uint64
		percent     string
	}{
		{100_000, 100, "0.100%"},
		{10_000, 354, "3.540%"},
		{354, 354, "100.000%"},
		{10, 20, "200.000%"},
		{math.MaxInt64, math.MaxInt64, "100.000%"},
	} {
		info := reservationFeeInfo(&looprpc.ClientAssetReservation{
			Amount: tc.amount, AssetFee: tc.fee,
		})
		require.Contains(t, info, tc.percent+" of principal")
		require.Contains(t, info, "Service fee paid upfront:")
		require.Contains(t, info, "routing and miner fees are separate")
	}
	require.Empty(t, reservationFeeInfo(&looprpc.ClientAssetReservation{}))
}

func TestReservationApprovalRoutingLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want uint64
	}{
		{
			name: "conventional default",
			want: 210_000,
		},
		{
			name: "explicit zero",
			args: []string{"--max_routing_fee=0"},
		},
		{
			name: "override",
			args: []string{"--max_routing_fee=5"},
			want: 5000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cli.Command{
				Name: "test",
				Flags: []cli.Flag{&cli.Uint64Flag{
					Name: "max_routing_fee",
				}},
				Action: func(_ context.Context, cmd *cli.Command) error {
					a, err := reservationApproval(cmd,
						&looprpc.ClientAssetReservation{
							QuoteHash:        make([]byte, 32),
							PrepayAmountMsat: 10_000_000,
							AssetFee:         10,
							MainProbe:        looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED,
						})
					require.NoError(t, err)
					require.Equal(t, tc.want, a.MaxRouteFeeMsat)
					require.Equal(t, make([]byte, 32), a.QuoteHash)
					return nil
				},
			}
			require.NoError(t, cmd.Run(t.Context(), append([]string{"test"},
				tc.args...)))
		})
	}
}

func TestReservationIDCanonical(t *testing.T) {
	for _, value := range []string{
		"", strings.Repeat("00", 32), strings.Repeat("AB", 32),
		strings.Repeat("aa", 31),
	} {
		_, err := reservationID(value)
		require.Error(t, err)
	}
	id, err := reservationID(strings.Repeat("ab", 32))
	require.NoError(t, err)
	require.Len(t, id, 32)
}

func TestReservationProbeInfo(t *testing.T) {
	r := &looprpc.ClientAssetReservation{
		MainProbe:                   looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED,
		MainProbeFeeMsat:            10_000,
		EstimatedPrepayRouteFeeMsat: 11,
	}
	r.ProbeFeeKnown = true
	info := reservationProbeInfo(r)
	require.Contains(t, info, "full asset amount reached the server")
	require.Contains(t, info, "Main routing fee estimate: 10000 msat")
	require.Contains(t, info, "Approximate prepay routing fee: 11 msat")
	require.Contains(t, info, "fixed hop fees ignored")

	r.ProbeFeeKnown = false
	require.Contains(t, reservationProbeInfo(r), "estimates are unavailable")
	require.Contains(t, reservationProbeInfo(r), "full asset amount reached")
	r.ProbeFeeKnown, r.MainProbeFeeMsat = true, 0
	require.Contains(t, reservationProbeInfo(r), "Main routing fee estimate: 0 msat")

	for _, tc := range []struct {
		result looprpc.ReservationProbeResult
		want   string
	}{
		{
			looprpc.ReservationProbeResult_RESERVATION_PROBE_FAILED,
			"full asset delivery was not verified",
		},
		{
			looprpc.ReservationProbeResult_RESERVATION_PROBE_TIMED_OUT,
			"timed out; routability is unknown",
		},
		{
			looprpc.ReservationProbeResult_RESERVATION_PROBE_UNSUPPORTED,
			"unsupported; routability is unknown",
		},
	} {
		r.MainProbe = tc.result
		info := reservationProbeInfo(r)
		require.Contains(t, info, tc.want)
		require.Contains(t, info, "Routing fee estimates are unavailable")
		require.NotContains(t, info, "Approximate prepay routing fee:")
	}
	r.MainProbe = looprpc.ReservationProbeResult_RESERVATION_PROBE_NOT_RUN
	r.SkipProbe = true
	require.Contains(t, reservationProbeInfo(r), "skipped by request")
	require.Contains(t, reservationProbeInfo(r), "estimates are unavailable")
}

func TestReservationApprovalRequiresProbeOrSkipProbe(t *testing.T) {
	for _, outcome := range []looprpc.ReservationProbeResult{
		looprpc.ReservationProbeResult_RESERVATION_PROBE_NOT_RUN,
		looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED,
		looprpc.ReservationProbeResult_RESERVATION_PROBE_FAILED,
		looprpc.ReservationProbeResult_RESERVATION_PROBE_TIMED_OUT,
		looprpc.ReservationProbeResult_RESERVATION_PROBE_UNSUPPORTED,
	} {
		t.Run(outcome.String(), func(t *testing.T) {
			for _, skipProbe := range []bool{false, true} {
				cmd := &cli.Command{
					Name: "test",
					Flags: []cli.Flag{
						&cli.BoolFlag{
							Name: "skip_probe",
						},
						&cli.BoolFlag{
							Name: "yes",
						},
					},
					Action: func(_ context.Context, cmd *cli.Command) error {
						a, err := reservationApproval(cmd,
							&looprpc.ClientAssetReservation{
								QuoteHash:        make([]byte, 32),
								PrepayAmountMsat: 10_000_000,
								MainProbe:        outcome,
							})
						if !skipProbe && outcome !=
							looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED {

							require.ErrorContains(t, err, "--skip_probe")
							require.Nil(t, a)
							return nil
						}
						require.NoError(t, err)
						require.Equal(t, skipProbe, a.SkipProbe)
						require.EqualValues(t, 210_000, a.MaxRouteFeeMsat)
						return nil
					},
				}
				// Suppressing the prompt does not bypass probe approval.
				args := []string{"test", "--yes"}
				if skipProbe {
					args = append(args, "--skip_probe")
				}
				require.NoError(t, cmd.Run(t.Context(), args))
			}
		})
	}
}

// approvalRecoveryClient models a committed approval whose RPC reply was lost.
type approvalRecoveryClient struct {
	looprpc.AssetReservationsClient

	t      *testing.T
	saved  *looprpc.ClientAssetReservation
	getErr error
	calls  []string
}

func (c *approvalRecoveryClient) Approve(context.Context,
	*looprpc.ApproveAssetReservationRequest, ...grpc.CallOption) (
	*looprpc.ClientAssetReservation, error) {

	c.calls = append(c.calls, "approve")
	return nil, errors.New("lost approval reply")
}

func (c *approvalRecoveryClient) Get(_ context.Context,
	selector *looprpc.ClientAssetReservationSelector, _ ...grpc.CallOption) (
	*looprpc.ClientAssetReservation, error) {

	c.calls = append(c.calls, "get")
	require.Equal(c.t, []byte{1}, selector.GetReservationId())
	return c.saved, c.getErr
}

func TestReservationApprovalRecovery(t *testing.T) {
	quoted := &looprpc.ClientAssetReservation{
		ReservationId: []byte{1}, AssetId: []byte{2}, Amount: 100,
	}
	for _, state := range []string{
		string(reservation.PayPrepay), string(reservation.WaitForDelivery),
		string(reservation.VerifyReservation), string(reservation.Ready),
		string(reservation.AwaitApproval), "unavailable",
	} {
		t.Run(state, func(t *testing.T) {
			client := &approvalRecoveryClient{
				t: t, saved: &looprpc.ClientAssetReservation{State: state},
			}
			if state == "unavailable" {
				client.getErr = errors.New("disconnected")
			}
			got, err := approveAssetReservation(t.Context(), client, quoted,
				&looprpc.ApproveAssetReservationRequest{})
			if state == string(reservation.AwaitApproval) || state == "unavailable" {
				require.ErrorContains(t, err, "daemon may still complete")
				require.ErrorContains(t, err, fmt.Sprintf(
					"--asset_id %x --amt 100 --reservation_id %x",
					quoted.AssetId, quoted.ReservationId))
			} else {
				require.NoError(t, err)
				require.Equal(t, client.saved, got)
			}
			require.Equal(t, []string{"approve", "get"}, client.calls)
		})
	}
}

func TestReservationProbeIsInternal(t *testing.T) {
	for _, cmd := range assetCommand.Commands[0].Commands {
		require.NotEqual(t, "probe", cmd.Name)
	}
}

func TestReservationWaitsForProbeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		outcome  looprpc.ReservationProbeResult
		reads    int
		canceled bool
	}{
		{"success awaits approval", string(reservation.AwaitApproval),
			looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED,
			0, false},
		{"skip awaits approval", string(reservation.AwaitApproval),
			looprpc.ReservationProbeResult_RESERVATION_PROBE_NOT_RUN,
			0, false},
		{"failed purchase ended", string(reservation.Canceled),
			looprpc.ReservationProbeResult_RESERVATION_PROBE_FAILED,
			0, true},
		{"wait for cancellation", string(reservation.CancelPrepay),
			looprpc.ReservationProbeResult_RESERVATION_PROBE_FAILED,
			1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := &looprpc.ClientAssetReservation{
				State: tc.state, MainProbe: tc.outcome,
			}
			client := &approvalRecoveryClient{
				t: t, saved: &looprpc.ClientAssetReservation{
					State: string(reservation.Canceled),
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err := waitAssetReservation(ctx, client,
				reservationSelectorID([]byte{1}), initial, true)
			if tc.canceled {
				require.ErrorContains(t, err, "Canceled")
			} else {
				require.NoError(t, err)
			}
			require.Len(t, client.calls, tc.reads)
		})
	}
}

// TestWaitAssetReservationRejected returns the historical funding error without
// another poll or an approval prompt.
func TestWaitAssetReservationRejected(t *testing.T) {
	for _, quoteOnly := range []bool{true, false} {
		_, err := waitAssetReservation(t.Context(), nil, nil,
			&looprpc.ClientAssetReservation{
				State: string(reservation.QuoteRejected),
			}, quoteOnly)
		require.Equal(t, codes.OutOfRange, status.Code(err))
		require.EqualError(t, err, "cannot initiate swap: rpc error: "+
			"code = OutOfRange desc = amount above current maximum")
	}
}
