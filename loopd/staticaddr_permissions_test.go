package loopd

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
)

// TestStaticAddressFundingPermissions exercises the production interceptor with
// signed macaroons, including URI grants issued before funding was introduced.
func TestStaticAddressFundingPermissions(t *testing.T) {
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

	const (
		newMethod  = "/looprpc.SwapClient/NewStaticAddress"
		fundMethod = "/looprpc.SwapClient/FundStaticAddress"
	)

	execute := bakery.Op{Entity: "swap", Action: "execute"}
	loopIn := bakery.Op{Entity: "loop", Action: "in"}
	fund := bakery.Op{Entity: "wallet", Action: "fund"}
	oldURI := bakery.Op{Entity: "uri", Action: newMethod}
	fundURI := bakery.Op{Entity: "uri", Action: fundMethod}

	tests := []struct {
		name    string
		method  string
		perms   []bakery.Op
		expired bool
		allowed bool
	}{
		{
			name:    "existing execute permissions can create",
			method:  newMethod,
			perms:   []bakery.Op{execute, loopIn},
			allowed: true,
		},
		{
			name:    "existing address URI can create",
			method:  newMethod,
			perms:   []bakery.Op{oldURI},
			allowed: true,
		},
		{
			name:   "existing execute permissions cannot fund",
			method: fundMethod,
			perms:  []bakery.Op{execute, loopIn},
		},
		{
			name:   "existing address URI cannot fund",
			method: fundMethod,
			perms:  []bakery.Op{oldURI},
		},
		{
			name:   "combined old permissions cannot fund",
			method: fundMethod,
			perms:  []bakery.Op{execute, loopIn, oldURI},
		},
		{
			name:   "fund permission alone is insufficient",
			method: fundMethod,
			perms:  []bakery.Op{fund},
		},
		{
			name:   "missing loop in permission",
			method: fundMethod,
			perms:  []bakery.Op{execute, fund},
		},
		{
			name:   "missing execute permission",
			method: fundMethod,
			perms:  []bakery.Op{loopIn, fund},
		},
		{
			name:    "explicit funding permissions",
			method:  fundMethod,
			perms:   []bakery.Op{execute, loopIn, fund},
			allowed: true,
		},
		{
			name:    "explicit funding URI",
			method:  fundMethod,
			perms:   []bakery.Op{fundURI},
			allowed: true,
		},
		{
			name:    "expired funding permissions",
			method:  fundMethod,
			perms:   []bakery.Op{execute, loopIn, fund},
			expired: true,
		},
		{
			name:    "expired funding URI",
			method:  fundMethod,
			perms:   []bakery.Op{fundURI},
			expired: true,
		},
		{
			name:   "missing macaroon",
			method: fundMethod,
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

			// Use a spend-all request to ensure legacy credentials cannot
			// reach the handler even for the most powerful funding request.
			var req any = &looprpc.FundStaticAddressRequest{
				SendCoinsRequest: &lnrpc.SendCoinsRequest{SendAll: true},
			}
			if test.method == newMethod {
				req = &looprpc.NewStaticAddressRequest{}
			}

			called := false
			_, err := intercept(ctx, req, &grpc.UnaryServerInfo{
				FullMethod: test.method,
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
