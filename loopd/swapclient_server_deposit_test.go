package loopd

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

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

// TestWithdrawAllDepositOutpoints checks `all` withdrawal handling for
// confirmed and unconfirmed deposits.
func TestWithdrawAllDepositOutpoints(t *testing.T) {
	t.Run("rejects unconfirmed", func(t *testing.T) {
		deposits := []*deposit.Deposit{
			{
				OutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{1},
					Index: 1,
				},
			},
			{
				OutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{2},
					Index: 2,
				},
				ConfirmationHeight: 123,
			},
		}

		_, err := withdrawAllDepositOutpoints(deposits)
		if err == nil {
			t.Fatal("expected unconfirmed deposit to fail all withdrawal")
		}
	})

	t.Run("returns all confirmed", func(t *testing.T) {
		first := wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 3,
		}
		second := wire.OutPoint{
			Hash:  chainhash.Hash{4},
			Index: 4,
		}
		deposits := []*deposit.Deposit{
			{
				OutPoint:           first,
				ConfirmationHeight: 123,
			},
			{
				OutPoint:           second,
				ConfirmationHeight: 124,
			},
		}

		outpoints, err := withdrawAllDepositOutpoints(deposits)
		if err != nil {
			t.Fatalf("expected confirmed deposits to succeed: %v", err)
		}
		if len(outpoints) != 2 {
			t.Fatalf("expected 2 outpoints, got %d", len(outpoints))
		}
		if outpoints[0] != first || outpoints[1] != second {
			t.Fatal("expected all confirmed outpoints to remain selected")
		}
	})
}
