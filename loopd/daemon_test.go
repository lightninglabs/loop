package loopd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShouldReportManagerErr verifies that context cancellations are treated as
// non-fatal while other errors are reported.
func TestShouldReportManagerErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "context canceled",
			err:      context.Canceled,
			expected: false,
		},
		{
			name:     "wrapped context canceled",
			err:      fmt.Errorf("wrap: %w", context.Canceled),
			expected: false,
		},
		{
			name:     "other error",
			err:      errors.New("boom"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldReportManagerErr(tt.err)
			require.Equal(t, tt.expected, got)
		})
	}
}

// TestWaitForStaticAddressManager checks readiness, failures and shutdown while
// the wallet's key counters are being reconciled before dependent managers run.
func TestWaitForStaticAddressManager(t *testing.T) {
	t.Parallel()

	for _, outcome := range []string{"ready", "error", "cancel", "quit"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready := make(chan struct{})
			errs := make(chan error, 1)
			quit := make(chan struct{})
			var wantErr error
			switch outcome {
			case "ready":
				close(ready)
			case "error":
				wantErr = errors.New("key recovery failed")
				errs <- wantErr
			case "cancel":
				wantErr = context.Canceled
				cancel()
			case "quit":
				wantErr = context.Canceled
				close(quit)
			}
			err := waitForStaticAddressManager(ctx, ready, errs, quit)
			require.ErrorIs(t, err, wantErr)
		})
	}
}
