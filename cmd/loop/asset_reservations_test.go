package main

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

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
	info := reservationProbeInfo(r)
	require.Contains(t, info, "route to destination found")
	require.Contains(t, info, "Main routing fee estimate: 10000 msat")
	require.Contains(t, info, "Approximate prepay routing fee: 11 msat")
	require.Contains(t, info, "fixed hop fees ignored")

	for _, tc := range []struct {
		result looprpc.ReservationProbeResult
		want   string
	}{
		{
			looprpc.ReservationProbeResult_RESERVATION_PROBE_FAILED,
			"no route to destination found",
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
