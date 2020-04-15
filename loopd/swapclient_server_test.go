package loopd

import "testing"

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
