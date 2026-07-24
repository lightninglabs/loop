package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
)

// TestMonitorSwapStateKeepsRegularLoopInFailureReason preserves the generic
// failure suffix for regular loop-in swaps so terminal errors stay visible.
func TestMonitorSwapStateKeepsRegularLoopInFailureReason(t *testing.T) {
	swap := &looprpc.SwapStatus{
		Type:  looprpc.SwapType_LOOP_IN,
		State: looprpc.SwapState_FAILED,
		FailureReason: looprpc.
			FailureReason_FAILURE_REASON_TIMEOUT,
	}

	got := monitorSwapState(swap)
	require.Equal(t, "FAILED (FAILURE_REASON_TIMEOUT)", got)
}

// TestMonitorSwapStateLabelsStaticLoopInStages locks the precise static loop-in
// stage names so monitor output stays stable as the FSM evolves.
func TestMonitorSwapStateLabelsStaticLoopInStages(t *testing.T) {
	tests := []struct {
		name        string
		staticState looprpc.StaticAddressLoopInSwapState
		want        string
	}{
		{
			name:        "init htlc",
			staticState: looprpc.StaticAddressLoopInSwapState_INIT_HTLC,
			want:        "INIT_HTLC",
		},
		{
			name: "sign htlc",
			staticState: looprpc.
				StaticAddressLoopInSwapState_SIGN_HTLC_TX,
			want: "SIGN_HTLC_TX",
		},
		{
			name: "monitor invoice and htlc",
			staticState: looprpc.
				StaticAddressLoopInSwapState_MONITOR_INVOICE_HTLC_TX,
			want: "MONITOR_INVOICE_HTLC_TX",
		},
		{
			name: "unlock deposits",
			staticState: looprpc.
				StaticAddressLoopInSwapState_UNLOCK_DEPOSITS,
			want: "UNLOCK_DEPOSITS",
		},
		{
			name:        "payment received",
			staticState: looprpc.StaticAddressLoopInSwapState_PAYMENT_RECEIVED,
			want:        "PAYMENT_RECEIVED",
		},
		{
			name: "timeout sweep",
			staticState: looprpc.
				StaticAddressLoopInSwapState_SWEEP_STATIC_ADDRESS_HTLC_TIMEOUT,
			want: "SWEEP_STATIC_ADDRESS_HTLC_TIMEOUT",
		},
		{
			name: "monitor timeout sweep",
			staticState: looprpc.
				StaticAddressLoopInSwapState_MONITOR_HTLC_TIMEOUT_SWEEP,
			want: "MONITOR_HTLC_TIMEOUT_SWEEP",
		},
		{
			name: "timeout swept",
			staticState: looprpc.
				StaticAddressLoopInSwapState_HTLC_STATIC_ADDRESS_TIMEOUT_SWEPT,
			want: "HTLC_STATIC_ADDRESS_TIMEOUT_SWEPT",
		},
		{
			name:        "success",
			staticState: looprpc.StaticAddressLoopInSwapState_SUCCEEDED,
			want:        "SUCCEEDED",
		},
		{
			name: "succeeded transitioning failed",
			staticState: looprpc.
				StaticAddressLoopInSwapState_SUCCEEDED_TRANSITIONING_FAILED,
			want: "SUCCEEDED_TRANSITIONING_FAILED",
		},
		{
			name: "failed",
			staticState: looprpc.
				StaticAddressLoopInSwapState_FAILED_STATIC_ADDRESS_SWAP,
			want: "FAILED_STATIC_ADDRESS_SWAP",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			swap := &looprpc.SwapStatus{
				Type: looprpc.SwapType_STATIC_LOOP_IN,
				StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
					StaticLoopInState: test.staticState,
				},
			}

			got := monitorSwapState(swap)
			require.Equal(t, test.want, got)
		})
	}

	initSwap := &looprpc.SwapStatus{
		Type: looprpc.SwapType_STATIC_LOOP_IN,
		StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
			StaticLoopInState: looprpc.
				StaticAddressLoopInSwapState_INIT_HTLC,
		},
	}
	signSwap := &looprpc.SwapStatus{
		Type: looprpc.SwapType_STATIC_LOOP_IN,
		StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
			StaticLoopInState: looprpc.
				StaticAddressLoopInSwapState_SIGN_HTLC_TX,
		},
	}
	require.NotEqual(t, monitorSwapState(initSwap), monitorSwapState(signSwap))
}

// TestMonitorSwapStateLabelsUnknownStaticLoopIn ensures the absent static
// oneof maps to the UNKNOWN static loop-in label.
func TestMonitorSwapStateLabelsUnknownStaticLoopIn(t *testing.T) {
	swap := &looprpc.SwapStatus{
		Type: looprpc.SwapType_STATIC_LOOP_IN,
	}

	got := monitorSwapState(swap)
	require.Equal(t, "UNKNOWN_STATIC_ADDRESS_SWAP_STATE", got)
}

// TestLogSwapHidesStaticLoopInCostForInFlightState proves in-flight static FSM
// state suppresses cost even with generic SUCCESS set deliberately.
func TestLogSwapHidesStaticLoopInCostForInFlightState(t *testing.T) {
	swap := &looprpc.SwapStatus{
		LastUpdateTime: time.Unix(1, 0).UnixNano(),
		Type:           looprpc.SwapType_STATIC_LOOP_IN,
		State:          looprpc.SwapState_SUCCESS,
		StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
			StaticLoopInState: looprpc.
				StaticAddressLoopInSwapState_INIT_HTLC,
		},
		Amt:              50_000,
		CostServer:       11,
		CostOnchain:      22,
		CostOffchain:     33,
		HtlcAddressP2Wsh: "bc1qstaticinflighttestaddress",
	}

	output := captureStdout(t, func() {
		logSwap(swap)
	})

	require.Contains(t, output, "STATIC_LOOP_IN INIT_HTLC 0.00050000 BTC")
	require.NotContains(t, output, "(cost:")
}

// TestLogSwapHidesCostForUnknownStaticLoopInState ensures unknown static
// states print the UNKNOWN label without a cost summary.
func TestLogSwapHidesCostForUnknownStaticLoopInState(t *testing.T) {
	swap := &looprpc.SwapStatus{
		LastUpdateTime: time.Unix(3, 0).UnixNano(),
		Type:           looprpc.SwapType_STATIC_LOOP_IN,
		StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
			StaticLoopInState: looprpc.
				StaticAddressLoopInSwapState_UNKNOWN_STATIC_ADDRESS_SWAP_STATE,
		},
		Amt:              50_000,
		CostServer:       11,
		CostOnchain:      22,
		CostOffchain:     33,
		HtlcAddressP2Wsh: "bc1qunknownstaticterminaltestaddress",
	}

	output := captureStdout(t, func() {
		logSwap(swap)
	})

	require.Contains(
		t, output, "STATIC_LOOP_IN UNKNOWN_STATIC_ADDRESS_SWAP_STATE 0.00050000 BTC",
	)
	require.NotContains(t, output, "(cost:")
}

// TestLogSwapShowsStaticLoopInCostForTerminalState preserves terminal cost
// output, including the P2WSH address, once the static loop-in is done.
func TestLogSwapShowsStaticLoopInCostForTerminalState(t *testing.T) {
	swap := &looprpc.SwapStatus{
		LastUpdateTime: time.Unix(2, 0).UnixNano(),
		Type:           looprpc.SwapType_STATIC_LOOP_IN,
		State:          looprpc.SwapState_INITIATED,
		StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
			StaticLoopInState: looprpc.
				StaticAddressLoopInSwapState_SUCCEEDED,
		},
		Amt:              50_000,
		CostServer:       11,
		CostOnchain:      22,
		CostOffchain:     33,
		HtlcAddressP2Wsh: "bc1qstaticterminaltestaddress",
	}

	output := captureStdout(t, func() {
		logSwap(swap)
	})

	require.Contains(
		t, output,
		"STATIC_LOOP_IN SUCCEEDED 0.00050000 BTC - P2WSH: bc1qstaticterminaltestaddress",
	)
	require.Contains(
		t, output,
		"(cost: server 11, onchain 22, offchain 33)",
	)
}

// TestLogSwapDisplaysStaticLoopInTypeAndStage ensures the monitor output names
// the static loop-in type and active FSM stage instead of collapsing them.
func TestLogSwapDisplaysStaticLoopInTypeAndStage(t *testing.T) {
	swap := &looprpc.SwapStatus{
		LastUpdateTime: time.Unix(1, 0).UnixNano(),
		Type:           looprpc.SwapType_STATIC_LOOP_IN,
		StaticLoopInStateOptional: &looprpc.SwapStatus_StaticLoopInState{
			StaticLoopInState: looprpc.
				StaticAddressLoopInSwapState_SIGN_HTLC_TX,
		},
		Amt:              50_000,
		HtlcAddressP2Wsh: "tb1q5cyxnuxmeuwuvkwfem96llyxf8duyshm56t8k8",
	}

	output := captureStdout(t, func() {
		logSwap(swap)
	})

	require.Contains(
		t, output, "STATIC_LOOP_IN SIGN_HTLC_TX 0.00050000 BTC",
	)
	require.Contains(
		t, output, "P2WSH: tb1q5cyxnuxmeuwuvkwfem96llyxf8duyshm56t8k8",
	)
	require.NotContains(t, output, "STATIC_LOOP_IN INIT_HTLC")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = writer

	fn()

	require.NoError(t, writer.Close())
	os.Stdout = originalStdout

	var buf bytes.Buffer
	_, err = io.Copy(&buf, reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	return strings.TrimSpace(buf.String())
}
