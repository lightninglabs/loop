package tapsdk

import (
	"context"
	"errors"
	"testing"
	"time"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
	grpcpkg "google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type connectionProbe struct {
	invoke func(context.Context, string, any, any) error
	stream func(context.Context, string) error
}

func (c *connectionProbe) Invoke(ctx context.Context, method string,
	args, reply any, _ ...grpcpkg.CallOption) error {

	return c.invoke(ctx, method, args, reply)
}

func (c *connectionProbe) NewStream(ctx context.Context,
	_ *grpcpkg.StreamDesc, method string, _ ...grpcpkg.CallOption) (
	grpcpkg.ClientStream, error) {

	return nil, c.stream(ctx, method)
}

func TestClientFromConnCapabilities(t *testing.T) {
	t.Parallel()

	wallet := tapsdk.NewWallet(
		NewClientFromConn(nil, time.Second), tapsdk.NetworkRegtest,
	)
	_, ok := wallet.Client().(tapsdk.CustomAnchorWalletKitClient)
	require.True(t, ok)

	caps, err := wallet.CustomAnchorCapabilities(t.Context())
	require.NoError(t, err)
	require.Equal(t, tapsdk.DefaultTapdCustomAnchorCapabilities(), *caps)

	_, ok = wallet.Client().(interface {
		RegisterTransferWithIssuance(context.Context, tapsdk.AssetRef,
			tapsdk.AssetID, tapsdk.PubKey, tapsdk.Outpoint) (
			*tapsdk.RegisteredAsset, error)
	})
	require.True(t, ok, "grouped imports require issuance registration")
	require.NoError(t, wallet.Close())
}

func TestClientFromConnGroupedRegistration(t *testing.T) {
	t.Parallel()

	group := testKey(t, 1)

	ref := tapsdk.AssetRefFromGroupKey(group)
	issuance := tapsdk.AssetID{1}
	point := tapsdk.Outpoint{Txid: tapsdk.Hash{2}, Index: 3}
	called := false
	conn := &connectionProbe{
		invoke: func(_ context.Context, method string,
			args, reply any) error {

			called = true
			require.Equal(t, "/taprpc.TaprootAssets/RegisterTransfer",
				method)
			req, ok := args.(*taprpc.RegisterTransferRequest)
			require.True(t, ok)
			require.Equal(t, issuance[:], req.AssetId)
			require.Equal(t, group[:], req.GroupKey)
			require.Equal(t, group[:], req.ScriptKey)
			require.Equal(t, point.Txid[:], req.Outpoint.Txid)
			require.Equal(t, point.Index, req.Outpoint.OutputIndex)

			resp, ok := reply.(*taprpc.RegisterTransferResponse)
			require.True(t, ok)
			resp.RegisteredAsset = &taprpc.Asset{
				Amount: 1_000,
				AssetGenesis: &taprpc.GenesisInfo{
					AssetId: issuance[:],
				},
				ScriptKey: group[:],
				ChainAnchor: &taprpc.AnchorInfo{
					AnchorOutpoint: point.String(),
				},
			}
			return nil
		},
	}
	client := NewClientFromConn(conn, time.Second)
	proofs := client.(issuanceProofClient)
	registered, err := proofs.RegisterTransferWithIssuance(
		t.Context(), ref, issuance, group, point,
	)
	require.NoError(t, err)
	require.True(t, called)
	require.Equal(t, issuance, registered.IssuanceID)
	require.Equal(t, ref, registered.AssetRef)
	require.Equal(t, point, registered.Outpoint)
	require.Equal(t, uint64(1_000), registered.Amount)
}

func TestClientFromConnAuthentication(t *testing.T) {
	t.Parallel()

	for _, supplied := range []bool{false, true} {
		name := "connection credentials"
		if supplied {
			name = "context credentials"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			original := metadata.Pairs("trace", "transfer")
			if supplied {
				original.Set("macaroon", "credential")
			}
			ctx := metadata.NewOutgoingContext(t.Context(), original)
			check := func(ctx context.Context) {
				md, _ := metadata.FromOutgoingContext(ctx)
				require.Equal(t, original, md)
			}
			streamErr := errors.New("stream intercepted")
			conn := &connectionProbe{
				invoke: func(ctx context.Context, _ string,
					_, reply any) error {

					check(ctx)
					require.IsType(t, &taprpc.GetInfoResponse{}, reply)
					return nil
				},
				stream: func(ctx context.Context, _ string) error {
					check(ctx)
					return streamErr
				},
			}
			client := NewClientFromConn(conn, time.Second)
			_, err := client.GetInfo(ctx)
			require.NoError(t, err)

			_, _, err = client.SubscribeReceiveEvents(
				ctx, &tapsdk.SubscribeReceiveEventsRequest{},
			)
			require.ErrorIs(t, err, streamErr)
			md, _ := metadata.FromOutgoingContext(ctx)
			require.Equal(t, original, md)
		})
	}
}

func TestClientFromConnTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "default", want: defaultClientTimeout},
		{name: "negative", timeout: -1, want: defaultClientTimeout},
		{name: "explicit", timeout: time.Second, want: time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			conn := &connectionProbe{
				invoke: func(ctx context.Context, _ string,
					_, _ any) error {

					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					require.InDelta(t, test.want.Seconds(),
						time.Until(deadline).Seconds(), 1)
					return nil
				},
			}
			client := NewClientFromConn(conn, test.timeout)
			_, err := client.GetInfo(t.Context())
			require.NoError(t, err)
		})
	}
}

func TestClientConnectionOwnership(t *testing.T) {
	t.Parallel()

	conn, err := grpcpkg.NewClient(
		"passthrough:///unused",
		grpcpkg.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	wallet := tapsdk.NewWallet(
		NewClientFromConn(conn, time.Second), tapsdk.NetworkRegtest,
	)
	require.NoError(t, wallet.Close())
	require.NotEqual(t, connectivity.Shutdown, conn.GetState())
}
