package loopd

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/assets"
	"github.com/stretchr/testify/require"
)

// TestValidateTapdMacaroonPath tests that validation updates the default tapd
// macaroon path for the configured network without changing an explicit path.
func TestValidateTapdMacaroonPath(t *testing.T) {
	customPath := filepath.Join(t.TempDir(), "custom.macaroon")
	defaultConfig := assets.DefaultTapdConfig()
	regtestConfig := assets.DefaultTapdConfigForNetwork(
		btcutil.AppDataDir("tapd", false), "regtest",
	)

	tests := []struct {
		name         string
		macaroonPath string
		expectedPath string
	}{
		{
			name:         "default path",
			macaroonPath: defaultConfig.MacaroonPath,
			expectedPath: regtestConfig.MacaroonPath,
		},
		{
			name:         "explicit path",
			macaroonPath: customPath,
			expectedPath: customPath,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Network = "regtest"
			cfg.LoopDir = t.TempDir()
			cfg.Tapd.MacaroonPath = test.macaroonPath

			require.NoError(t, Validate(&cfg))
			require.Equal(
				t, test.expectedPath, cfg.Tapd.MacaroonPath,
			)
		})
	}
}
