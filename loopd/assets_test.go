package loopd

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/assets"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAssetRuntimeConfigRequiresCompleteActivation(t *testing.T) {
	_, enabled, err := assetRuntimeConfig(&Config{})
	require.NoError(t, err)
	require.False(t, enabled)
	_, public := btcec.PrivKeyFromBytes([]byte{1})
	key := hex.EncodeToString(public.SerializeCompressed())
	cfg := &Config{
		Tapd:                  &assets.TapdConfig{Activate: true},
		AssetRuntimeSocket:    "/tmp/asset.sock",
		AssetRuntimeTokenFile: "/tmp/asset.token", AssetPeer: key,
	}
	_, enabled, err = assetRuntimeConfig(cfg)
	require.NoError(t, err)
	require.True(t, enabled)
	cfg.Tapd.Activate = false
	_, _, err = assetRuntimeConfig(cfg)
	require.Error(t, err)
	cfg.Tapd.Activate = true
	cfg.AssetPeer = strings.ToUpper(key)
	_, _, err = assetRuntimeConfig(cfg)
	require.Error(t, err)
	cfg.AssetPeer = key
	cfg.AssetRuntimeTokenFile = "relative.token"
	_, _, err = assetRuntimeConfig(cfg)
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
