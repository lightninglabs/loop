package loopd

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/stretchr/testify/require"
)

var (
	testnetAddr, _ = btcutil.NewAddressScriptHash(
		[]byte{123}, &chaincfg.TestNet3Params,
	)

	mainnetAddr, _ = btcutil.NewAddressScriptHash(
		[]byte{123}, &chaincfg.MainNetParams,
	)
)

// TestValidateConfTarget tests all failure and success cases for our conf
// target validation function, including the case where we replace a zero
// target with the default provided.
func TestValidateConfTarget(t *testing.T) {
	const (
		// Various input confirmation values for tests.
		zeroConf int32 = 0
		oneConf  int32 = 1
		twoConf  int32 = 2
		fiveConf int32 = 5

		// defaultConf is the default confirmation target we use for
		// all tests.
		defaultConf = 6
	)

	tests := []struct {
		name           string
		confTarget     int32
		expectedTarget int32
		expectErr      bool
	}{
		{
			name:           "zero conf, get default",
			confTarget:     zeroConf,
			expectedTarget: defaultConf,
			expectErr:      false,
		},
		{
			name:       "one conf, get error",
			confTarget: oneConf,
			expectErr:  true,
		},
		{
			name:           "two conf, ok",
			confTarget:     twoConf,
			expectedTarget: twoConf,
			expectErr:      false,
		},
		{
			name:           "five conf, ok",
			confTarget:     fiveConf,
			expectedTarget: fiveConf,
			expectErr:      false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			target, err := validateConfTarget(
				test.confTarget, defaultConf,
			)

			haveErr := err != nil
			if haveErr != test.expectErr {
				t.Fatalf("expected err: %v, got: %v",
					test.expectErr, err)
			}

			if target != test.expectedTarget {
				t.Fatalf("expected: %v, got: %v",
					test.expectedTarget, target)
			}
		})
	}
}

// TestValidateLoopInRequest tests validation of loop in requests.
func TestValidateLoopInRequest(t *testing.T) {
	tests := []struct {
		name           string
		external       bool
		confTarget     int32
		expectErr      bool
		expectedTarget int32
	}{
		{
			name:           "external and htlc conf set",
			external:       true,
			confTarget:     1,
			expectErr:      true,
			expectedTarget: 0,
		},
		{
			name:           "external and no conf",
			external:       true,
			confTarget:     0,
			expectErr:      false,
			expectedTarget: 0,
		},
		{
			name:           "not external, zero conf",
			external:       false,
			confTarget:     0,
			expectErr:      false,
			expectedTarget: loop.DefaultHtlcConfTarget,
		},
		{
			name:           "not external, bad conf",
			external:       false,
			confTarget:     1,
			expectErr:      true,
			expectedTarget: 0,
		},
		{
			name:           "not external, ok conf",
			external:       false,
			confTarget:     5,
			expectErr:      false,
			expectedTarget: 5,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			external := test.external
			conf, err := validateLoopInRequest(
				test.confTarget, external,
			)

			haveErr := err != nil
			if haveErr != test.expectErr {
				t.Fatalf("expected err: %v, got: %v",
					test.expectErr, err)
			}

			if conf != test.expectedTarget {
				t.Fatalf("expected: %v, got: %v",
					test.expectedTarget, conf)
			}
		})
	}
}

// TestValidateLoopOutRequest tests validation of loop out requests.
func TestValidateLoopOutRequest(t *testing.T) {
	tests := []struct {
		name           string
		chain          chaincfg.Params
		confTarget     int32
		destAddr       btcutil.Address
		label          string
		err            error
		expectedTarget int32
	}{
		{
			name:           "mainnet address with mainnet backend",
			chain:          chaincfg.MainNetParams,
			destAddr:       mainnetAddr,
			label:          "label ok",
			confTarget:     2,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:           "mainnet address with testnet backend",
			chain:          chaincfg.TestNet3Params,
			destAddr:       mainnetAddr,
			label:          "label ok",
			confTarget:     2,
			err:            errIncorrectChain,
			expectedTarget: 0,
		},
		{
			name:           "testnet address with testnet backend",
			chain:          chaincfg.TestNet3Params,
			destAddr:       testnetAddr,
			label:          "label ok",
			confTarget:     2,
			err:            nil,
			expectedTarget: 2,
		},
		{
			name:           "testnet address with mainnet backend",
			chain:          chaincfg.MainNetParams,
			destAddr:       testnetAddr,
			label:          "label ok",
			confTarget:     2,
			err:            errIncorrectChain,
			expectedTarget: 0,
		},
		{
			name:           "invalid label",
			chain:          chaincfg.MainNetParams,
			destAddr:       mainnetAddr,
			label:          labels.Reserved,
			confTarget:     2,
			err:            labels.ErrReservedPrefix,
			expectedTarget: 0,
		},
		{
			name:           "invalid conf target",
			chain:          chaincfg.MainNetParams,
			destAddr:       mainnetAddr,
			label:          "label ok",
			confTarget:     1,
			err:            errConfTargetTooLow,
			expectedTarget: 0,
		},
		{
			name:           "default conf target",
			chain:          chaincfg.MainNetParams,
			destAddr:       mainnetAddr,
			label:          "label ok",
			confTarget:     0,
			err:            nil,
			expectedTarget: 9,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			conf, err := validateLoopOutRequest(
				&test.chain, test.confTarget, test.destAddr,
				test.label,
			)
			require.True(t, errors.Is(err, test.err))
			require.Equal(t, test.expectedTarget, conf)
		})
	}
}
