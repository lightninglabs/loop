package loopd

import "testing"

// TestDepositBlocksUntilExpiry checks blocks-until-expiry handling for
// confirmed and unconfirmed deposits.
func TestDepositBlocksUntilExpiry(t *testing.T) {
	t.Run("unconfirmed", func(t *testing.T) {
		if blocks := depositBlocksUntilExpiry(0, 144, 500); blocks != 144 {
			t.Fatalf("expected 144 blocks for unconfirmed deposit, got %d",
				blocks)
		}
	})

	t.Run("confirmed", func(t *testing.T) {
		if blocks := depositBlocksUntilExpiry(450, 144, 500); blocks != 94 {
			t.Fatalf("expected 94 blocks until expiry, got %d",
				blocks)
		}
	})
}
