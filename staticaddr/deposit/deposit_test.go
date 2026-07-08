package deposit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDepositIsExpiredUnconfirmed verifies that unconfirmed deposits do not
// expire because their CSV timeout has not started yet.
func TestDepositIsExpiredUnconfirmed(t *testing.T) {
	t.Parallel()

	d := &Deposit{}

	require.False(t, d.IsExpired(1_000, 144))
}
