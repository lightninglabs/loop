package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const staticAddressLabelTestAddress = "bcrt1pfu9g59aqtxd39653f76y4c8z7r3t9tmcvrvhl57a3dgj3epdwxdqcd9fpw"

// loopCLIRun describes a single offline CLI replay so label tests can assert
// the exact gRPC payload emitted by the command without requiring a live loopd.
type loopCLIRun struct {
	args   []string
	stdin  string
	events []grpcPayload
}

// protoEventSpec keeps replay request and response construction data together
// so label tests compare protobuf payloads instead of brittle JSON text.
type protoEventSpec struct {
	method string
	event  string
	msg    proto.Message
}

// TestLowConfDepositWarningConfirmedOnly verifies confirmed deposits below the
// conservative warning threshold are included in the warning text.
func TestLowConfDepositWarningConfirmedOnly(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "confirmed-low",
			ConfirmationHeight: 100,
			BlocksUntilExpiry:  140,
		},
		{
			Outpoint:           "confirmed-high",
			ConfirmationHeight: 95,
			BlocksUntilExpiry:  139,
		},
	}

	warning := lowConfDepositWarning(
		deposits, []string{"confirmed-low", "confirmed-high"}, 144,
	)

	require.Contains(t, warning, "confirmed-low (5 confirmations)")
	require.NotContains(t, warning, "confirmed-high")
}

// TestLowConfDepositWarningUnconfirmed verifies unconfirmed deposits get a
// warning that the swap may wait for confirmation-risk acceptance.
func TestLowConfDepositWarningUnconfirmed(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "mempool",
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  144,
		},
	}

	warning := lowConfDepositWarning(deposits, []string{"mempool"}, 144)

	require.Contains(t, warning, "mempool (unconfirmed)")
	require.True(
		t,
		strings.Contains(
			warning,
			"conservative 6-confirmation threshold",
		),
	)
	require.NotContains(t, warning, "executed immediately")
}

// TestWarningDepositOutpointsAutoSelectPrefersConfirmed verifies automatic
// warning selection keeps the loop-in preference for confirmed outputs.
func TestWarningDepositOutpointsAutoSelectPrefersConfirmed(t *testing.T) {
	t.Parallel()

	const csvExpiry = 1100

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "mempool-large",
			Value:              2_000_000,
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  csvExpiry,
		},
		{
			Outpoint:           "confirmed",
			Value:              1_500_000,
			ConfirmationHeight: 100,
			BlocksUntilExpiry:  csvExpiry - 5,
		},
	}

	selected := warningDepositOutpoints(deposits, nil, true, 1_000_000)

	require.Equal(t, []string{"confirmed"}, selected)
	require.Empty(t, lowConfDepositWarning(deposits, selected, csvExpiry))
}

// TestWarningDepositOutpointsAutoSelectIncludesNeededUnconfirmed verifies the
// warning path includes mempool deposits when they are needed for the target.
func TestWarningDepositOutpointsAutoSelectIncludesNeededUnconfirmed(t *testing.T) {
	t.Parallel()

	const csvExpiry = 1100

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "confirmed-small",
			Value:              500_000,
			ConfirmationHeight: 100,
			BlocksUntilExpiry:  csvExpiry - 5,
		},
		{
			Outpoint:           "mempool-large",
			Value:              2_000_000,
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  csvExpiry,
		},
	}

	selected := warningDepositOutpoints(deposits, nil, true, 1_000_000)

	require.Equal(
		t, []string{"confirmed-small", "mempool-large"}, selected,
	)

	warning := lowConfDepositWarning(deposits, selected, csvExpiry)
	require.Contains(t, warning, "mempool-large (unconfirmed)")
	require.NotContains(t, warning, "confirmed-small")
}

// TestWarningDepositSelectionMatchesLoopInSelection verifies CLI warning
// selection matches the loop-in selector.
func TestWarningDepositSelectionMatchesLoopInSelection(t *testing.T) {
	t.Parallel()

	const (
		blockHeight  = uint32(10_000)
		csvExpiry    = uint32(1_200)
		targetAmount = int64(2_500_000)
	)

	type fixture struct {
		name               string
		value              int64
		confirmationHeight int64
	}

	fixtures := []fixture{
		{
			name:               "mempool-huge",
			value:              3_000_000,
			confirmationHeight: 0,
		},
		{
			name:               "confirmed-later-expiry",
			value:              2_000_000,
			confirmationHeight: 9_900,
		},
		{
			name:               "confirmed-earlier-expiry",
			value:              2_000_000,
			confirmationHeight: 9_890,
		},
		{
			name:               "confirmed-small",
			value:              600_000,
			confirmationHeight: 9_900,
		},
		{
			name:               "confirmed-too-close-to-expiry",
			value:              5_000_000,
			confirmationHeight: 9_849,
		},
	}

	rpcDeposits := make([]*looprpc.Deposit, 0, len(fixtures))
	loopInDeposits := make([]*deposit.Deposit, 0, len(fixtures))
	for idx, fixture := range fixtures {
		hash := chainhash.Hash{byte(idx + 1)}
		outpoint := wire.OutPoint{
			Hash:  hash,
			Index: uint32(idx),
		}

		blocksUntilExpiry := int64(0)
		if fixture.confirmationHeight > 0 {
			blocksUntilExpiry = fixture.confirmationHeight +
				int64(csvExpiry) - int64(blockHeight)
		}

		rpcDeposits = append(rpcDeposits, &looprpc.Deposit{
			Outpoint:           outpoint.String(),
			Value:              fixture.value,
			ConfirmationHeight: fixture.confirmationHeight,
			BlocksUntilExpiry:  blocksUntilExpiry,
		})
		loopInDeposits = append(loopInDeposits, &deposit.Deposit{
			OutPoint:           outpoint,
			Value:              btcutil.Amount(fixture.value),
			ConfirmationHeight: fixture.confirmationHeight,
		})
	}

	cliSelected := autoSelectedWarningOutpoints(
		rpcDeposits, targetAmount,
	)

	loopInSelected, err := loopin.SelectDeposits(
		btcutil.Amount(targetAmount), loopInDeposits, csvExpiry,
		blockHeight,
	)
	require.NoError(t, err)

	loopInSelectedOutpoints := make([]string, 0, len(loopInSelected))
	for _, selected := range loopInSelected {
		loopInSelectedOutpoints = append(
			loopInSelectedOutpoints, selected.OutPoint.String(),
		)
	}

	require.Equal(t, loopInSelectedOutpoints, cliSelected)
}

// TestStaticAddressNewSendsLabel locks the CLI contract that operator labels
// are sent as local metadata when a reusable static address is created.
func TestStaticAddressNewSendsLabel(t *testing.T) {
	run := loopCLIRun{
		args: []string{
			"loop", "static", "new", "--label", "treasury",
			"--network", "regtest",
		},
		stdin: "y\n",
		events: []grpcPayload{
			requestEvent(
				t, "/looprpc.SwapClient/NewStaticAddress",
				&looprpc.NewStaticAddressRequest{Label: "treasury"},
			),
			responseEvent(
				t, "/looprpc.SwapClient/NewStaticAddress",
				&looprpc.NewStaticAddressResponse{
					Address: staticAddressLabelTestAddress,
					Expiry:  14400,
					Label:   "treasury",
				},
			),
		},
	}

	stdout, err := runLoopCLI(t, run)

	require.NoError(t, err)
	require.Contains(t, stdout, "CONTINUE WITH NEW ADDRESS")
	require.Contains(t, stdout, `"label": "treasury"`)
}

// TestStaticAddressNewRejectsInvalidLabelBeforePromptAndRPC ensures invalid
// operator metadata is rejected locally before the user is prompted or loopd is
// contacted.
func TestStaticAddressNewRejectsInvalidLabelBeforePromptAndRPC(t *testing.T) {
	run := loopCLIRun{
		args: []string{
			"loop", "static", "new", "--label",
			labels.Reserved + " static", "--network", "regtest",
		},
	}

	stdout, err := runLoopCLI(t, run)

	require.ErrorIs(t, err, labels.ErrReservedPrefix)
	require.NotContains(t, stdout, "CONTINUE WITH NEW ADDRESS")
}

// TestStaticAddressUpdateLabelSendsLabel verifies relabeling an existing static
// address only changes the local metadata sent to loopd.
func TestStaticAddressUpdateLabelSendsLabel(t *testing.T) {
	run := updateLabelRun(t, "ops", false)

	stdout, err := runLoopCLI(t, run)

	require.NoError(t, err)
	require.Contains(t, stdout, `"label": "ops"`)
}

// TestStaticAddressUpdateLabelClearSendsEmptyLabel verifies clearing metadata is
// represented as an empty label rather than a separate protocol flag.
func TestStaticAddressUpdateLabelClearSendsEmptyLabel(t *testing.T) {
	run := updateLabelRun(t, "", true)

	stdout, err := runLoopCLI(t, run)

	require.NoError(t, err)
	require.Contains(t, stdout, `"label": ""`)
}

// TestStaticAddressUpdateLabelRejectsInvalidLabelBeforeRPC prevents reserved
// labels from reaching loopd, preserving the local validation boundary.
func TestStaticAddressUpdateLabelRejectsInvalidLabelBeforeRPC(t *testing.T) {
	run := loopCLIRun{
		args: []string{
			"loop", "static", "updatelabel",
			staticAddressLabelTestAddress, labels.Reserved + " static",
			"--network", "regtest",
		},
	}

	_, err := runLoopCLI(t, run)

	require.ErrorIs(t, err, labels.ErrReservedPrefix)
}

// TestStaticAddressUpdateLabelRejectsLabelAndClear guards the CLI contract that
// clearing and setting metadata are mutually exclusive user intents.
func TestStaticAddressUpdateLabelRejectsLabelAndClear(t *testing.T) {
	run := loopCLIRun{
		args: []string{
			"loop", "static", "updatelabel",
			staticAddressLabelTestAddress, "ops", "--clear",
			"--network", "regtest",
		},
	}

	_, err := runLoopCLI(t, run)

	require.ErrorContains(t, err, "cannot specify both label and --clear")
}

func TestStaticAddressUpdateLabelRequiresLabelOrClear(t *testing.T) {
	run := loopCLIRun{
		args: []string{
			"loop", "static", "updatelabel", staticAddressLabelTestAddress,
			"--network", "regtest",
		},
	}

	_, err := runLoopCLI(t, run)

	require.ErrorContains(t, err, "label is required; use --clear")
}

// updateLabelRun centralizes the relabel replay fixture so set and clear cases
// assert the same RPC method, address, and response shape.
func updateLabelRun(t *testing.T, label string, clearLabel bool) loopCLIRun {
	t.Helper()

	args := []string{
		"loop", "static", "updatelabel", staticAddressLabelTestAddress,
	}
	if clearLabel {
		args = append(args, "--clear")
	} else {
		args = append(args, label)
	}
	args = append(args, "--network", "regtest")

	return loopCLIRun{
		args: args,
		events: []grpcPayload{
			requestEvent(
				t, "/looprpc.SwapClient/UpdateStaticAddressLabel",
				&looprpc.UpdateStaticAddressLabelRequest{
					StaticAddress: staticAddressLabelTestAddress,
					Label:         label,
				},
			),
			responseEvent(
				t, "/looprpc.SwapClient/UpdateStaticAddressLabel",
				&looprpc.UpdateStaticAddressLabelResponse{
					StaticAddress: staticAddressLabelTestAddress,
					Label:         label,
				},
			),
		},
	}
}

// runLoopCLI drives the real command tree through deterministic stdio and gRPC
// replay so label tests exercise user-visible CLI behavior.
func runLoopCLI(t *testing.T, run loopCLIRun) (string, error) {
	t.Helper()

	var stdout bytes.Buffer
	stdoutUnhook, err := hookStdout(os.Stdout, &stdout, nil)
	require.NoError(t, err)

	stdinUnhook, err := hookStdin(
		os.Stdin, strings.NewReader(run.stdin), nil,
	)
	require.NoError(t, err)

	conn := &recordedClientConn{events: run.events}
	restoreTransport := hookGrpc(&replayTransport{conn: conn})

	previousDeterministic := forceDeterministicJSON
	forceDeterministicJSON = true

	cmd := newRootCommandForReplay()
	runErr := cmd.Run(context.Background(), run.args)

	forceDeterministicJSON = previousDeterministic
	restoreTransport()
	require.NoError(t, stdinUnhook())
	require.NoError(t, stdoutUnhook())
	require.NoError(t, conn.assertFullyConsumed())

	return stdout.String(), runErr
}

// requestEvent records the expected outbound protobuf call so label tests fail
// when the CLI sends the wrong local metadata to loopd.
func requestEvent(t *testing.T, method string, msg proto.Message) grpcPayload {
	t.Helper()

	return protoEvent(t, protoEventSpec{
		method: method,
		event:  "request",
		msg:    msg,
	})
}

// responseEvent records the daemon reply needed to complete replay without a
// live static-address backend.
func responseEvent(t *testing.T, method string, msg proto.Message) grpcPayload {
	t.Helper()

	return protoEvent(t, protoEventSpec{
		method: method,
		event:  "response",
		msg:    msg,
	})
}

// protoEvent marshals protobuf fixtures through the same JSON codec used by the
// replay transport so tests compare machine-consumed payloads.
func protoEvent(t *testing.T, spec protoEventSpec) grpcPayload {
	t.Helper()

	payload, err := protoMarshal.Marshal(spec.msg)
	require.NoError(t, err)

	return grpcPayload{
		Method:      spec.method,
		Event:       spec.event,
		MessageType: string(spec.msg.ProtoReflect().Descriptor().FullName()),
		Payload:     json.RawMessage(payload),
	}
}
