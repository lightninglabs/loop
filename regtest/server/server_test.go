package server

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

func TestLoopInCltvDeltaValidation(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		delta int32
		valid bool
	}{
		{name: "default", delta: 0, valid: true},
		{name: "minimum", delta: minLoopInCltvDelta, valid: true},
		{name: "maximum", delta: maxLoopInCltvDelta, valid: true},
		{name: "too short", delta: minLoopInCltvDelta - 1},
		{name: "too long", delta: maxLoopInCltvDelta + 1},
		{name: "negative", delta: -1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server, err := New(context.Background(), Config{
				Lnd: &lndclient.LndServices{
					ChainParams: &chaincfg.RegressionNetParams,
				},
				Bitcoin:         &loopOutTestBitcoin{},
				LoopInCltvDelta: testCase.delta,
			})
			if !testCase.valid {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			server.Stop()
		})
	}
}
