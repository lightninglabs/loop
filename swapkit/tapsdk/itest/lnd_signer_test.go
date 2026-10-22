//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

const defaultAliceLndHost = "localhost:10009"

func newAliceLndAnchorSigner(t testing.TB) tapsdk.AnchorSigner {
	t.Helper()

	tlsPath := os.Getenv("LND_ALICE_TLS")
	if tlsPath == "" {
		tlsPath = extractDockerFile(
			t, "tap-sdk-lnd-alice", "/root/.lnd/tls.cert",
		)
	}

	macaroonPath := os.Getenv("LND_ALICE_MAC")
	if macaroonPath == "" {
		macaroonPath = extractDockerFile(
			t, "tap-sdk-lnd-alice",
			"/root/.lnd/data/chain/bitcoin/regtest/admin.macaroon",
		)
	}

	tlsCreds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	require.NoError(t, err)

	conn, err := grpc.NewClient(
		envOr("LND_ALICE_HOST", defaultAliceLndHost),
		grpc.WithTransportCredentials(tlsCreds),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	macBytes, err := os.ReadFile(macaroonPath)
	require.NoError(t, err)

	macHex := hex.EncodeToString(macBytes)
	walletKit := walletrpc.NewWalletKitClient(conn)

	return func(ctx context.Context, anchorPsbt []byte) ([]byte, error) {
		rpcCtx := metadata.AppendToOutgoingContext(
			ctx, "macaroon", macHex,
		)

		signed, err := walletKit.SignPsbt(
			rpcCtx, &walletrpc.SignPsbtRequest{
				FundedPsbt: anchorPsbt,
			},
		)
		if err != nil {
			return nil, err
		}
		if len(signed.SignedPsbt) == 0 {
			return nil, fmt.Errorf("lnd returned empty signed PSBT")
		}

		resp, err := walletKit.FinalizePsbt(
			rpcCtx, &walletrpc.FinalizePsbtRequest{
				FundedPsbt: signed.SignedPsbt,
			},
		)
		if err != nil {
			return nil, err
		}
		if len(resp.SignedPsbt) == 0 {
			return nil, fmt.Errorf("lnd returned empty signed PSBT")
		}

		return resp.SignedPsbt, nil
	}
}
