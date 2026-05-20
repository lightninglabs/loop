package deposit

import "testing"

// TestIsExpiredUnconfirmed checks that unconfirmed deposits don't start their
// expiry timer.
func TestIsExpiredUnconfirmed(t *testing.T) {
	deposit := &Deposit{
		ConfirmationHeight: 0,
	}

	if deposit.IsExpired(500, 100) {
		t.Fatal("unconfirmed deposit should not be expired")
	}
}
