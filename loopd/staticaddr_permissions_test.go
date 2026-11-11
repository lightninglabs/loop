package loopd

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
)

// TestStaticAddressPermissions exercises address creation authorization with
// signed macaroons, including URI grants issued before multi-address support.
func TestStaticAddressPermissions(t *testing.T) {
	t.Parallel()

	service, err := lndclient.NewMacaroonService(
		&lndclient.MacaroonServiceConfig{
			RootKeyStore:     bakery.NewMemRootKeyStore(),
			MacaroonLocation: "loop-test",
			StatelessInit:    true,
			RequiredPerms:    looprpc.RequiredPermissions,
		},
	)
	require.NoError(t, err)
	require.NoError(t, service.Start())
	t.Cleanup(func() {
		require.NoError(t, service.Stop())
	})
	intercept, _, err := service.Interceptors()
	require.NoError(t, err)

	const method = "/looprpc.SwapClient/NewStaticAddress"

	execute := bakery.Op{Entity: "swap", Action: "execute"}
	loopIn := bakery.Op{Entity: "loop", Action: "in"}
	addressURI := bakery.Op{Entity: "uri", Action: method}

	tests := []struct {
		name    string
		perms   []bakery.Op
		expired bool
		allowed bool
	}{
		{
			name:    "execute and loop in permissions",
			perms:   []bakery.Op{execute, loopIn},
			allowed: true,
		},
		{
			name:    "existing address URI",
			perms:   []bakery.Op{addressURI},
			allowed: true,
		},
		{
			name:  "missing loop in permission",
			perms: []bakery.Op{execute},
		},
		{
			name:  "missing execute permission",
			perms: []bakery.Op{loopIn},
		},
		{
			name: "unrelated URI",
			perms: []bakery.Op{{
				Entity: "uri", Action: "/looprpc.SwapClient/LoopIn",
			}},
		},
		{
			name:    "expired execute permissions",
			perms:   []bakery.Op{execute, loopIn},
			expired: true,
		},
		{
			name:    "expired address URI",
			perms:   []bakery.Op{addressURI},
			expired: true,
		},
		{
			name: "missing macaroon",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			if len(test.perms) > 0 {
				mac, err := service.NewMacaroon(
					ctx, macaroons.DefaultRootKeyID, test.perms...,
				)
				require.NoError(t, err)
				if test.expired {
					err := mac.AddCaveat(ctx,
						checkers.TimeBeforeCaveat(
							time.Now().Add(-time.Hour),
						), nil, nil,
					)
					require.NoError(t, err)
				}
				macBytes, err := mac.M().MarshalBinary()
				require.NoError(t, err)
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
					"macaroon", hex.EncodeToString(macBytes),
				))
			}

			req := &looprpc.NewStaticAddressRequest{}

			called := false
			_, err := intercept(ctx, req, &grpc.UnaryServerInfo{
				FullMethod: method,
			}, func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
			if test.allowed {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, test.allowed, called)
		})
	}
}
