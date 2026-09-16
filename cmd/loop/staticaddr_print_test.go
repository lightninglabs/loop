package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
)

const (
	staticDepositTestConfTargetArg = "--conf_target=3"
	staticDepositTestSweepAllArg   = "--sweepall"
	staticDepositTestForceArg      = "--force"
)

// TestStaticAddressDepositPrintsFundingCommand verifies address creation and
// explicit funding instructions without invoking a wallet-spending RPC.
func TestStaticAddressDepositPrintsFundingCommand(t *testing.T) {
	outpoint := strings.Repeat("ab", 32) + ":1"
	for _, testCase := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "amount and options",
			args: []string{
				"--amt=100000", staticDepositTestConfTargetArg, "--min_confs=0",
				"--coin_selection_strategy=random", "--utxo=" + outpoint,
				"--label=wallet deposit",
			},
			want: []string{
				"--amt=100000", staticDepositTestConfTargetArg, "--min_confs=0",
				"--coin_selection_strategy=random", "--utxo='" + outpoint + "'",
				"--label='wallet deposit'",
			},
		},
		{
			name: "sweep with fee",
			args: []string{staticDepositTestSweepAllArg, "--sat_per_vbyte=5"},
			want: []string{staticDepositTestSweepAllArg, "--sat_per_vbyte=5", "--min_confs=1"},
		},
		{
			name: "amount placeholder",
			want: []string{"--amt=AMOUNT_SAT", "Replace AMOUNT_SAT"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := &printingStaticAddressClient{address: "bcrt1paddress"}
			args := append([]string{staticDepositTestForceArg}, testCase.args...)
			resp, output, err := runPrintingStaticDeposit(
				t, t.Context(), client, args, "", false,
			)
			require.NoError(t, err)
			require.Equal(t, client.address, resp.Address)
			require.Equal(t, 1, client.newCalls)
			require.Contains(t, output, "WARNING")
			require.NotContains(t, output, "CONTINUE WITH NEW ADDRESS")
			require.Contains(t, output,
				"lncli --network='regtest' sendcoins --addr='"+client.address+"'")
			for _, want := range testCase.want {
				require.Contains(t, output, want)
			}
			require.NotContains(t, output, staticDepositTestForceArg)
			if strings.Contains(output, staticDepositTestSweepAllArg) {
				require.NotContains(t, output, "--amt=")
			}
		})
	}
}

// TestStaticAddressDepositRejectsInvalidOptions prevents invalid input from
// consuming a receive address, including malformed and conflicting fee flags.
func TestStaticAddressDepositRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--amt=0"}, {"--amt=-1"}, {"--amt=1", staticDepositTestSweepAllArg},
		{"--sat_per_vbyte=1", staticDepositTestConfTargetArg},
		{"--sat_per_vbyte=-1"}, {"--conf_target=-1"},
		{"--conf_target=2147483648"}, {"--min_confs=2147483648"},
		{"--sat_per_vbyte=9223372036854775808"},
		{"--coin_selection_strategy=unknown"}, {"--utxo=invalid"},
		{"--label=" + strings.Repeat("x", 501)},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			client := &printingStaticAddressClient{}
			args = append([]string{staticDepositTestForceArg}, args...)
			resp, output, err := runPrintingStaticDeposit(
				t, t.Context(), client, args, "", false,
			)
			require.Error(t, err)
			require.Nil(t, resp)
			require.Zero(t, client.newCalls)
			require.Empty(t, output)
		})
	}
}

// TestStaticAddressDepositWarningAndFailure keeps address acknowledgement
// separate from funding and emits no command after cancellation or RPC failure.
func TestStaticAddressDepositWarningAndFailure(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		input       string
		interactive bool
		force       bool
		cancel      bool
		rpcErr      error
		wantCalls   int
		wantErr     bool
	}{
		{name: "noninteractive needs force", wantErr: true},
		{name: "declined", input: "n\n", interactive: true, wantErr: true},
		{name: "accepted", input: "y\n", interactive: true, wantCalls: 1},
		{
			name: "RPC failure", force: true, wantCalls: 1, wantErr: true,
			rpcErr: errors.New("new address unavailable"),
		},
		{
			name: "cancellation", force: true, cancel: true,
			wantCalls: 1, wantErr: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if testCase.cancel {
				cancel()
			}
			client := &printingStaticAddressClient{
				address: "bcrt1paddress", err: testCase.rpcErr,
			}
			var args []string
			if testCase.force {
				args = append(args, staticDepositTestForceArg)
			}
			resp, output, err := runPrintingStaticDeposit(
				t, ctx, client, args, testCase.input, testCase.interactive,
			)
			if testCase.wantErr {
				require.Error(t, err)
				require.Nil(t, resp)
				require.NotContains(t, output, "lncli ")
			} else {
				require.NoError(t, err)
				require.Contains(t, output, "lncli ")
			}
			require.Equal(t, testCase.wantCalls, client.newCalls)
		})
	}
}

// TestStaticAddressDepositQuotesShellValues passes the printed command through
// a real POSIX shell whose lncli function only records its arguments. Metacharacters
// must remain literal label/address/network data, never shell commands.
func TestStaticAddressDepositQuotesShellValues(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}
	label := "it's $(printf INJECTED) `printf INJECTED`; echo INJECTED\nnext line"
	address := "address'; echo INJECTED; '"
	client := &printingStaticAddressClient{address: address}
	_, output, err := runPrintingStaticDeposit(
		t, t.Context(), client,
		[]string{staticDepositTestForceArg, "--amt=12345", "--label=" + label}, "", false,
	)
	require.NoError(t, err)
	_, command, found := strings.Cut(output, "To fund this address from lnd, run:\n  ")
	require.True(t, found)
	// NUL-separated arguments preserve embedded newlines for comparison.
	script := "lncli() { printf '%s\\000' \"$@\"; }\n" + command
	actual, err := exec.CommandContext(t.Context(), shell, "-c", script).Output()
	require.NoError(t, err)
	args := strings.Split(strings.TrimSuffix(string(actual), "\x00"), "\x00")
	require.Equal(t, []string{
		"--network=regtest", "sendcoins", "--addr=" + address,
		"--amt=12345", "--min_confs=1", "--label=" + label,
	}, args)
}

func runPrintingStaticDeposit(t *testing.T, ctx context.Context,
	client *printingStaticAddressClient, args []string, input string,
	interactive bool) (*looprpc.NewStaticAddressResponse, string, error) {

	t.Helper()
	var output bytes.Buffer
	var resp *looprpc.NewStaticAddressResponse
	// cli flags retain their IsSet state after a command run. Use fresh
	// instances so subtests do not accidentally inherit previous options.
	flags := []cli.Flag{&cli.StringFlag{Name: "network", Value: "regtest"}}
	for _, flag := range depositStaticAddressCommand.Flags {
		switch flag := flag.(type) {
		case *cli.BoolFlag:
			flags = append(flags, &cli.BoolFlag{
				Name: flag.Name, Aliases: flag.Aliases, Value: flag.Value,
			})
		case *cli.Int64Flag:
			flags = append(flags, &cli.Int64Flag{Name: flag.Name, Value: flag.Value})
		case *cli.Uint64Flag:
			flags = append(flags, &cli.Uint64Flag{Name: flag.Name, Value: flag.Value})
		case *cli.StringFlag:
			flags = append(flags, &cli.StringFlag{Name: flag.Name, Value: flag.Value})
		case *cli.StringSliceFlag:
			flags = append(flags, &cli.StringSliceFlag{Name: flag.Name})
		default:
			t.Fatalf("unexpected deposit flag %T", flag)
		}
	}
	cmd := &cli.Command{
		Name:  "deposit",
		Flags: flags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var err error
			resp, err = executeStaticAddressDeposit(
				ctx, cmd, client, strings.NewReader(input), &output, interactive,
			)
			return err
		},
	}
	err := cmd.Run(ctx, append([]string{"deposit"}, args...))
	return resp, output.String(), err
}

type printingStaticAddressClient struct {
	looprpc.SwapClientClient

	address  string
	err      error
	newCalls int
}

func (c *printingStaticAddressClient) NewStaticAddress(ctx context.Context,
	_ *looprpc.NewStaticAddressRequest, _ ...grpc.CallOption) (
	*looprpc.NewStaticAddressResponse, error) {

	c.newCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return &looprpc.NewStaticAddressResponse{Address: c.address}, nil
}
