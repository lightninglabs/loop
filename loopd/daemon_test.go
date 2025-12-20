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
