package loopd

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestAssetLoopInConfigRequiresCompleteActivation(t *testing.T) {
	_, enabled, err := assetLoopInConfig(&Config{})
	require.NoError(t, err)
	require.False(t, enabled)
	_, public := btcec.PrivKeyFromBytes([]byte{1})
	key := hex.EncodeToString(public.SerializeCompressed())
	cfg := &Config{
		Tapd:            &assets.TapdConfig{Activate: true},
		AssetBitcoinURL: "http://127.0.0.1:18443", AssetPeer: key,
		AssetFeeSat: 1000, AssetMaxFeeSat: 2000,
	}
	_, enabled, err = assetLoopInConfig(cfg)
	require.NoError(t, err)
	require.True(t, enabled)
	cfg.Tapd.Activate = false
	_, _, err = assetLoopInConfig(cfg)
	require.Error(t, err)
	cfg.Tapd.Activate = true
	cfg.AssetPeer = strings.ToUpper(key)
	_, _, err = assetLoopInConfig(cfg)
	require.Error(t, err)
	cfg.AssetPeer = key
	cfg.AssetBitcoinURL = "relative-rpc"
	_, _, err = assetLoopInConfig(cfg)
	require.Error(t, err)
}

func TestAssetRPCIdentifierBoundary(t *testing.T) {
	_, public := btcec.PrivKeyFromBytes([]byte{1})
	id := [32]byte{1}
	group := public.SerializeCompressed()
	_, _, err := assetRequestIdentity(id[:], group)
	require.NoError(t, err)
	for _, invalid := range [][]byte{
		nil, make([]byte, 31), make([]byte, 32),
	} {
		_, _, err := assetRequestIdentity(invalid, group)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	_, _, err = assetRequestIdentity(id[:], public.SerializeUncompressed())
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestAssetRPCFailureDoesNotRevealInternalClass(t *testing.T) {
	old := log_.Load()
	setLogger(btclog.Disabled)
	t.Cleanup(func() { log_.Store(old) })
	want := assetOperationError(nil).Error()
	for _, cause := range []error{
		errors.New("owner mismatch"), errors.New("record absent"),
		errors.New("disk failure"),
		errors.New("deposit already reserved"),
	} {
		err := assetOperationError(cause)
		require.Equal(t, want, err.Error())
		require.Equal(t, codes.Unavailable, status.Code(err))
	}
}

// TestStaticAddressAssetParams keeps native quantities separate from Bitcoin
// options and validates the durable identity before any manager side effect.
func TestStaticAddressAssetParams(t *testing.T) {
	_, public := btcec.PrivKeyFromBytes([]byte{1})
	id := [32]byte{1}
	request := &looprpc.StaticAddressLoopInRequest{
		AssetParams: &looprpc.AssetLoopInParams{
			Outpoint:  strings.Repeat("ab", 32) + ":0",
			RequestId: id[:], GroupKey: public.SerializeCompressed(),
			Amount: 1 << 63, MaxAssetDebit: 1<<63 + 1,
			MaxAssetFee: 2,
		},
	}
	intent, err := assetLoopInRequest(request)
	require.NoError(t, err)
	require.Equal(t, id, intent.RequestID)
	require.Equal(t, request.AssetParams.Outpoint, intent.Outpoint)
	require.Equal(t, request.AssetParams.Amount, intent.Amount)
	require.Equal(t, request.AssetParams.MaxAssetDebit, intent.MaxDebit)
	require.Equal(t, request.AssetParams.MaxAssetFee, intent.MaxFee)
	require.Equal(t, request.AssetParams.GroupKey, intent.GroupKey[:])

	for _, test := range []struct {
		name string
		edit func(*looprpc.StaticAddressLoopInRequest)
	}{
		{"satoshi amount", func(r *looprpc.StaticAddressLoopInRequest) {
			r.Amount = 1000
		}},
		{"satoshi fee", func(r *looprpc.StaticAddressLoopInRequest) {
			r.MaxSwapFeeSatoshis = 1
		}},
		{"last hop", func(r *looprpc.StaticAddressLoopInRequest) {
			r.LastHop = public.SerializeCompressed()
		}},
		{"label", func(r *looprpc.StaticAddressLoopInRequest) {
			r.Label = "label"
		}},
		{"initiator", func(r *looprpc.StaticAddressLoopInRequest) {
			r.Initiator = "caller"
		}},
		{"route hints", func(r *looprpc.StaticAddressLoopInRequest) {
			r.RouteHints = []*swapserverrpc.RouteHint{{}}
		}},
		{"private", func(r *looprpc.StaticAddressLoopInRequest) {
			r.Private = true
		}},
		{"payment timeout", func(r *looprpc.StaticAddressLoopInRequest) {
			r.PaymentTimeoutSeconds = 10
		}},
		{"fast", func(r *looprpc.StaticAddressLoopInRequest) {
			r.Fast = true
		}},
		{"no outpoint", func(r *looprpc.StaticAddressLoopInRequest) {
			r.AssetParams.Outpoint = ""
		}},
		{"Bitcoin outpoint", func(r *looprpc.StaticAddressLoopInRequest) {
			r.Outpoints = []string{r.AssetParams.Outpoint}
		}},
		{"noncanonical outpoint", func(r *looprpc.StaticAddressLoopInRequest) {
			r.AssetParams.Outpoint = strings.ToUpper(
				r.AssetParams.Outpoint,
			)
		}},
		{"request identity", func(r *looprpc.StaticAddressLoopInRequest) {
			r.AssetParams.RequestId = make([]byte, 32)
		}},
		{"group", func(r *looprpc.StaticAddressLoopInRequest) {
			r.AssetParams.GroupKey = public.SerializeUncompressed()
		}},
		{"zero amount", func(r *looprpc.StaticAddressLoopInRequest) {
			r.AssetParams.Amount = 0
		}},
		{"debit limit", func(r *looprpc.StaticAddressLoopInRequest) {
			r.AssetParams.MaxAssetDebit = r.AssetParams.Amount - 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := proto.Clone(request).(*looprpc.StaticAddressLoopInRequest)
			test.edit(invalid)
			server := &swapClientServer{}
			_, err := server.StaticAddressLoopIn(t.Context(), invalid)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
}

// TestStaticAddressAssetRPCRouting verifies that explicit asset variants
// dispatch before Bitcoin managers are touched, including when disabled.
func TestStaticAddressAssetRPCRouting(t *testing.T) {
	old := log_.Load()
	setLogger(btclog.Disabled)
	t.Cleanup(func() { log_.Store(old) })
	server := &swapClientServer{}
	_, public := btcec.PrivKeyFromBytes([]byte{1})
	id := [32]byte{1}
	_, err := server.StaticAddressLoopIn(t.Context(),
		&looprpc.StaticAddressLoopInRequest{
			AssetParams: &looprpc.AssetLoopInParams{
				Outpoint:  strings.Repeat("ab", 32) + ":0",
				RequestId: id[:],
				GroupKey:  public.SerializeCompressed(),
				Amount:    1000, MaxAssetDebit: 1000,
			},
		})
	require.Equal(t, codes.Unavailable, status.Code(err))
	_, err = server.ListStaticAddressDeposits(t.Context(),
		&looprpc.ListStaticAddressDepositsRequest{AssetsOnly: true})
	require.Equal(t, codes.Unavailable, status.Code(err))
	_, err = server.ListStaticAddressSwaps(t.Context(),
		&looprpc.ListStaticAddressSwapsRequest{AssetsOnly: true})
	require.Equal(t, codes.Unavailable, status.Code(err))

	for _, request := range []*looprpc.ListStaticAddressDepositsRequest{
		{AssetsOnly: true, StateFilter: looprpc.DepositState_DEPOSITED},
		{AssetsOnly: true, Outpoints: []string{"bad-outpoint"}},
	} {
		_, err := server.ListStaticAddressDeposits(t.Context(), request)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
}
