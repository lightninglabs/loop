package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	staticDepositTestAmountArg     = "--amt=15000"
	staticDepositTestConfTargetArg = "--conf_target=3"
	staticDepositTestSweepAllArg   = "--sweepall"
	staticDepositTestForceArg      = "--force"
)

// TestStaticAddressDepositFundsAddress verifies the typed wallet options sent
// to the daemon and the funding result returned to the CLI.
func TestStaticAddressDepositFundsAddress(t *testing.T) {
	first := strings.Repeat("ab", 32) + ":1"
	second := strings.Repeat("cd", 32) + ":0"
	label := "Alice's wallet deposit"

	for _, tc := range []struct {
		name string
		args []string
		want *lnrpc.SendCoinsRequest
	}{
		{
			name: "fixed amount",
			args: []string{"--amt=100000", staticDepositTestConfTargetArg,
				"--min_confs=0", "--coin_selection_strategy=random",
				"--utxo=" + first, "--utxo=" + second, "--label=" + label},
			want: &lnrpc.SendCoinsRequest{
				Amount: 100000, TargetConf: 3, SpendUnconfirmed: true,
				CoinSelectionStrategy: lnrpc.CoinSelectionStrategy_STRATEGY_RANDOM,
				Outpoints: []*lnrpc.OutPoint{
					{TxidBytes: bytes.Repeat([]byte{0xab}, 32), TxidStr: first[:64], OutputIndex: 1},
					{TxidBytes: bytes.Repeat([]byte{0xcd}, 32), TxidStr: second[:64]},
				}, Label: label,
			},
		},
		{
			name: "sweep",
			args: []string{staticDepositTestSweepAllArg, "--sat_per_vbyte=5"},
			want: &lnrpc.SendCoinsRequest{
				SendAll: true, SatPerVbyte: 5, MinConfs: 1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &depositAddressClient{}
			args := append([]string{staticDepositTestForceArg}, tc.args...)
			resp, stderr, err := runStaticDeposit(
				t, t.Context(), client, args, "", false,
			)
			require.NoError(t, err)
			require.True(t, proto.Equal(tc.want, client.request))
			require.Equal(t, "funding-txid", resp.GetSendCoinsResponse().GetTxid())
			require.Contains(t, stderr, "WARNING")
			require.NotContains(t, stderr, "CONTINUE WITH NEW ADDRESS")
		})
	}
}

// TestStaticAddressDepositRejectsInvalidOptions prevents invalid input from
// consuming a receive address, including malformed and conflicting fee flags.
func TestStaticAddressDepositRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--amt=0"}, {"--amt=-1"}, {"--amt=14999"},
		{staticDepositTestAmountArg, "--sat_per_vbyte=101"},
		{staticDepositTestAmountArg, staticDepositTestSweepAllArg},
		{staticDepositTestAmountArg, "--sat_per_vbyte=1", staticDepositTestConfTargetArg},
		{staticDepositTestAmountArg, "--sat_per_vbyte=-1"}, {staticDepositTestAmountArg, "--conf_target=-1"},
		{staticDepositTestAmountArg, "--conf_target=2147483648"}, {staticDepositTestAmountArg, "--min_confs=2147483648"},
		{staticDepositTestAmountArg, "--sat_per_vbyte=9223372036854775808"},
		{staticDepositTestAmountArg, "--coin_selection_strategy=unknown"}, {staticDepositTestAmountArg, "--utxo=invalid"},
		{staticDepositTestAmountArg, "--label=" + strings.Repeat("x", 501)},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			client := &depositAddressClient{}
			args := append([]string{staticDepositTestForceArg}, args...)
			resp, stderr, err := runStaticDeposit(
				t, t.Context(), client, args, "", false,
			)
			require.Error(t, err)
			require.Nil(t, client.request)
			require.Nil(t, resp)
			require.Empty(t, stderr)
		})
	}
}

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
		{name: "declined payment", input: "y\nno\n", interactive: true, wantErr: true},
		{name: "force still confirms payment", input: "no\n", interactive: true, force: true, wantErr: true},
		{name: "accepted", input: "y\nyes\n", interactive: true, wantCalls: 1},
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
			client := &depositAddressClient{
				err: testCase.rpcErr,
			}
			args := []string{staticDepositTestAmountArg}
			if testCase.force {
				args = append(args, staticDepositTestForceArg)
			}
			resp, _, err := runStaticDeposit(
				t, ctx, client, args, testCase.input, testCase.interactive,
			)
			if testCase.wantErr {
				require.Error(t, err)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				require.Equal(t, "funding-txid", resp.GetSendCoinsResponse().GetTxid())
			}
			require.Equal(t, testCase.wantCalls, client.newCalls)
		})
	}
}

func runStaticDeposit(t *testing.T, ctx context.Context,
	client *depositAddressClient, args []string, input string,
	interactive bool) (*looprpc.FundStaticAddressResponse, string, error) {

	t.Helper()
	var stderr bytes.Buffer
	var resp *looprpc.FundStaticAddressResponse
	cmd := cloneCommandForReplay(depositStaticAddressCommand)
	cmd.Action = func(ctx context.Context, cmd *cli.Command) error {
		var err error
		resp, err = executeStaticAddressDeposit(
			ctx, cmd, client, strings.NewReader(input), &stderr,
			interactive, interactive,
		)
		return err
	}
	err := cmd.Run(ctx, append([]string{"deposit"}, args...))
	return resp, stderr.String(), err
}

type depositAddressClient struct {
	looprpc.SwapClientClient

	request  *lnrpc.SendCoinsRequest
	err      error
	newCalls int
}

func (c *depositAddressClient) FundStaticAddress(ctx context.Context,
	req *lnrpc.SendCoinsRequest, _ ...grpc.CallOption) (
	*looprpc.FundStaticAddressResponse, error) {

	c.newCalls++
	c.request = req
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return &looprpc.FundStaticAddressResponse{
		SendCoinsResponse: &lnrpc.SendCoinsResponse{Txid: "funding-txid"},
	}, nil
}
