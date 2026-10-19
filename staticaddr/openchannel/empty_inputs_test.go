package openchannel

import (
	"math"
	"testing"

	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestOpenChannelFundMaxRejectsEmptyInputs rejects a selection with no
// confirmed deposits before locking inputs or contacting the channel peer.
func TestOpenChannelFundMaxRejectsEmptyInputs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		deposits []*deposit.Deposit
	}{
		{name: "no deposits"},
		{name: "only unconfirmed", deposits: []*deposit.Deposit{{
			OutPoint: testOutPoint(1), Value: 200_000,
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			deposits := &mockDepositManager{
				activeDeposits: test.deposits,
			}
			manager := NewManager(&Config{DepositManager: deposits})
			outpoint, err := manager.OpenChannel(
				t.Context(), &lnrpc.OpenChannelRequest{
					FundMax: true, SatPerVbyte: 10,
				},
			)
			require.ErrorIs(t, err, ErrOpeningChannelUnavailableDeposits)
			require.Nil(t, outpoint)
			require.Empty(t, deposits.calls)
		})
	}
}

// TestOpenChannelFeeRateBounds rejects overflow before any services are used,
// and lets the largest representable conversion reach subsequent validation.
func TestOpenChannelFeeRateBounds(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		feeRate     uint64
		errContains string
	}{
		{
			name:        "signed conversion overflow",
			feeRate:     math.MaxInt64/1000 + 1,
			errContains: "fee rate is too high",
		},
		{
			name:        "unsigned multiplication overflow",
			feeRate:     math.MaxUint64 - 999,
			errContains: "fee rate is too high",
		},
		{
			name:        "largest representable conversion",
			feeRate:     math.MaxInt64 / 1000,
			errContains: "duplicate outpoint",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := testOutPoint(1)
			manager := NewManager(&Config{})
			outpoint, err := manager.OpenChannel(
				t.Context(), &lnrpc.OpenChannelRequest{
					FundMax: true, SatPerVbyte: test.feeRate,
					Outpoints: []*lnrpc.OutPoint{
						{TxidStr: op.Hash.String(), OutputIndex: op.Index},
						{TxidStr: op.Hash.String(), OutputIndex: op.Index},
					},
				},
			)
			require.ErrorContains(t, err, test.errContains)
			require.Nil(t, outpoint)
		})
	}
}
