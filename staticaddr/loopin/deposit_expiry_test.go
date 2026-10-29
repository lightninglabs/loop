package loopin

import (
	"math"
	"testing"

	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/stretchr/testify/require"
)

// TestValidateDepositHtlcExpiry checks actual contract expiry, per-address CSV
// limits, next-block confirmation, and arithmetic at the height type bounds.
func TestValidateDepositHtlcExpiry(t *testing.T) {
	newDeposit := func(height int64, csv uint32) *deposit.Deposit {
		return &deposit.Deposit{
			ConfirmationHeight: height,
			AddressParams:      &script.Parameters{Expiry: csv},
		}
	}
	tests := []struct {
		name     string
		deposits []*deposit.Deposit
		htlc     int32
		start    uint32
		wantErr  bool
	}{
		{
			// The assumed 1,000-block contract fits this deposit,
			// but the actual 1,100-block contract does not.
			name: "actual longer than assumed", start: 1_000, htlc: 2_100,
			deposits: []*deposit.Deposit{newDeposit(800, 1_250)},
			wantErr:  true,
		},
		{
			name: "exact safety boundary", start: 1_000, htlc: 2_000,
			deposits: []*deposit.Deposit{newDeposit(800, 1_250)},
		},
		{
			name: "shorter actual contract", start: 1_000, htlc: 1_900,
			deposits: []*deposit.Deposit{newDeposit(800, 1_250)},
		},
		{
			name: "one short CSV in mixed set", htlc: 2_000,
			deposits: []*deposit.Deposit{
				newDeposit(800, 1_250), newDeposit(800, 1_249),
			},
			wantErr: true,
		},
		{
			name: "unconfirmed boundary", start: 1_000, htlc: 2_000,
			deposits: []*deposit.Deposit{newDeposit(0, 1_049)},
		},
		{
			name: "unconfirmed too short", start: 1_000, htlc: 2_000,
			deposits: []*deposit.Deposit{newDeposit(0, 1_048)},
			wantErr:  true,
		},
		{
			name: "signed HTLC sum does not wrap", htlc: math.MaxInt32,
			deposits: []*deposit.Deposit{
				newDeposit(math.MaxInt32-1_000, 1_049),
			},
			wantErr: true,
		},
		{
			name: "unsigned deposit sum does not wrap", htlc: math.MaxInt32,
			deposits: []*deposit.Deposit{newDeposit(math.MaxUint32, 1)},
		},
		{
			name: "unsigned CSV does not wrap", htlc: math.MaxInt32,
			deposits: []*deposit.Deposit{newDeposit(1, math.MaxUint32)},
		},
		{
			name: "next block does not wrap", start: math.MaxUint32,
			htlc: math.MaxInt32, deposits: []*deposit.Deposit{newDeposit(0, 1)},
		},
		{name: "no deposits", htlc: 2_000, wantErr: true},
		{name: "nil deposit", htlc: 2_000,
			deposits: []*deposit.Deposit{nil}, wantErr: true},
		{name: "missing address", htlc: 2_000,
			deposits: []*deposit.Deposit{{}}, wantErr: true},
		{name: "negative height", htlc: 2_000,
			deposits: []*deposit.Deposit{newDeposit(-1, 10_000)}, wantErr: true},
		{name: "invalid large height", htlc: 2_000,
			deposits: []*deposit.Deposit{newDeposit(math.MaxInt64, 1)}, wantErr: true},
		{name: "zero HTLC expiry", htlc: 0,
			deposits: []*deposit.Deposit{newDeposit(800, 1_250)}, wantErr: true},
		{name: "negative HTLC expiry", htlc: -1,
			deposits: []*deposit.Deposit{newDeposit(800, 1_250)}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDepositHtlcExpiry(test.deposits, test.htlc, test.start)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
