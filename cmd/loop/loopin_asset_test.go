package main

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
)

type assetQuoteConn struct {
	daemonConn

	quote *looprpc.QuoteRequest
}

func (c *assetQuoteConn) Invoke(_ context.Context, _ string, req, _ any,
	_ ...grpc.CallOption) error {

	c.quote = req.(*looprpc.QuoteRequest)
	return errors.New("quote reached")
}

type assetQuoteTransport struct {
	grpcTransport

	conn *assetQuoteConn
}

func (t *assetQuoteTransport) Dial(*cli.Command) (daemonConn, func(), error) {
	return t.conn, func() {}, nil
}

// TestAssetLoopInCLI checks early validation and the edge used for the fee
// quote. Malformed options must not reach any quote or swap RPC.
func TestAssetLoopInCLI(t *testing.T) {
	_, pub := btcec.PrivKeyFromBytes([]byte{1})
	edge := hex.EncodeToString(pub.SerializeCompressed())
	_, otherPub := btcec.PrivKeyFromBytes([]byte{2})
	other := hex.EncodeToString(otherPub.SerializeCompressed())
	assetID := hex.EncodeToString(make([]byte, 32))
	base := []string{"loop", "in", "--amt", "500000", "--asset_id", assetID}
	for _, tc := range []struct {
		name   string
		args   []string
		err    string
		quoted bool
	}{
		{
			name: "short asset id",
			args: []string{"--asset_id", "00"},
			err:  "32 byte",
		},
		{
			name: "malformed asset id",
			args: []string{"--asset_id", "zz"},
			err:  "invalid asset id",
		},
		{
			name: "missing edge",
			err:  "asset_edge_node is required",
		},
		{
			name: "short edge",
			args: []string{"--asset_edge_node", "02"},
			err:  "33 byte",
		},
		{
			name: "missing minimum",
			args: []string{"--asset_edge_node", edge},
			err:  "min_asset_amount",
		},
		{
			name: "conflict",
			args: []string{
				"--asset_edge_node", edge, "--last_hop", other,
			},
			err: "must match",
		},
		{
			name: "both selectors",
			args: []string{"--group_key", edge},
			err:  "mutually exclusive",
		},
		{
			name: "edge quote",
			args: []string{
				"--asset_edge_node", edge,
				"--min_asset_amount", "4900",
			},
			err:    "quote reached",
			quoted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &assetQuoteConn{}
			restore := hookGrpc(&assetQuoteTransport{conn: conn})
			defer restore()
			cmd := newRootCommandForReplay()
			args := append(append([]string{}, base...), tc.args...)
			err := cmd.Run(t.Context(), args)
			require.ErrorContains(t, err, tc.err)
			if tc.quoted {
				require.NotNil(t, conn.quote.AssetLoopIn)
				require.EqualValues(t, 4900, conn.quote.AssetLoopIn.MinAssetAmount)
				require.Equal(
					t, pub.SerializeCompressed(),
					conn.quote.LoopInLastHop,
				)
			} else {
				require.Nil(t, conn.quote)
			}
		})
	}
}

// TestAssetGroupCLI ensures both CLI entry points send group-only selectors.
func TestAssetGroupCLI(t *testing.T) {
	_, pub := btcec.PrivKeyFromBytes([]byte{1})
	key := hex.EncodeToString(pub.SerializeCompressed())
	for _, prefix := range [][]string{{"loop", "in"}, {"loop", "quote", "in"}} {
		conn := &assetQuoteConn{}
		restore := hookGrpc(&assetQuoteTransport{conn: conn})
		args := append([]string{}, prefix...)
		args = append(args, "--group_key", key, "--asset_edge_node", key,
			"--min_asset_amount", "4900", "500000")
		err := newRootCommandForReplay().Run(t.Context(), args)
		restore()
		require.ErrorContains(t, err, "quote reached")
		require.NotNil(t, conn.quote.AssetLoopIn)
		require.Empty(t, conn.quote.AssetLoopIn.AssetId)
		require.Equal(t, pub.SerializeCompressed(), conn.quote.AssetLoopIn.GroupKey)
	}
}

// TestAssetQuoteDisplay checks the terms shown before swap confirmation.
func TestAssetQuoteDisplay(t *testing.T) {
	output := captureStdout(t, func() {
		printQuoteInResp(&looprpc.QuoteRequest{Amt: 500_000},
			&looprpc.InQuoteResponse{
				AssetInfo: &looprpc.AssetLoopInInfo{
					AssetAmount:    4964,
					MinAssetAmount: 4960,
					AssetRate: &looprpc.FixedPoint{
						Coefficient: "1000000",
					},
				},
			}, false)
	})
	require.Contains(t, output, "Expected asset output: 4964 units")
	require.Contains(t, output, "Minimum asset output: 4960 units")
	require.Contains(t, output, "Asset rate: 1000000 * 10^-0 units/BTC")
	require.Contains(t, output, "Execution requotes")
}
