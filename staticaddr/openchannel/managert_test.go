package openchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/withdraw"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestCalculateFundingTxValaues tests the calculateFundingTxValaues function
// with various scenarios.
func TestCalculateFundingTxValaues(t *testing.T) {
	var (
		dustLimit   = lnwallet.DustLimitForSize(input.P2TRSize)
		satPerVbyte = 1
		allDeposits = []*deposit.Deposit{
			{
				Value: 100_000,
			},
			{
				Value: 200_000,
			},
		}

		chanOpenFeeRate = chainfee.SatPerKVByte(
			satPerVbyte * 1000,
		).FeePerKWeight()

		deposits = func(idxs ...int) []*deposit.Deposit {
			var selectedDeposits []*deposit.Deposit
			for _, i := range idxs {
				selectedDeposits = append(
					selectedDeposits, allDeposits[i-1],
				)
			}

			return selectedDeposits
		}

		sum = func(idxs ...int) btcutil.Amount {
			var total btcutil.Amount
			for _, i := range idxs {
				total += allDeposits[i-1].Value
			}

			return total
		}
	)

	weightWithoutChange, err := withdraw.WithdrawalTxWeight(
		len(allDeposits), nil, lnrpc.CommitmentType_ANCHORS, false,
	)
	require.NoError(t, err)

	feeWithoutChange := chanOpenFeeRate.FeeForWeight(
		weightWithoutChange,
	)

	weightWithChange, err := withdraw.WithdrawalTxWeight(
		len(allDeposits), nil, lnrpc.CommitmentType_ANCHORS, true,
	)
	require.NoError(t, err)

	feeWithChange := chanOpenFeeRate.FeeForWeight(
		weightWithChange,
	)

	cases := []struct {
		name           string
		deposits       []*deposit.Deposit
		localAmount    btcutil.Amount
		fundMax        bool
		satPerVbyte    uint64
		commitmentType lnrpc.CommitmentType
		wantFundingAmt btcutil.Amount
		wantChangeAmt  btcutil.Amount
		wantErr        string
	}{
		{
			name:           "fundmax",
			deposits:       deposits(1, 2),
			fundMax:        true,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2) - feeWithoutChange,
			wantChangeAmt:  0,
			wantErr:        "",
		},
		{
			name:           "local_amt",
			deposits:       deposits(1, 2),
			localAmount:    sum(1, 2) - 50_000,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2) - 50_000,
			wantChangeAmt:  50_000 - feeWithChange,
			wantErr:        "",
		},
		{
			name:           "change to miners",
			deposits:       deposits(1, 2),
			localAmount:    sum(1, 2) - dustLimit + 1,
			fundMax:        false,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2) - dustLimit + 1,
			wantChangeAmt:  0,
			wantErr:        "",
		},
		{
			name:           "change doesn't cover for fees",
			deposits:       deposits(1, 2),
			localAmount:    sum(1, 2),
			fundMax:        false,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: 0,
			wantChangeAmt:  0,
			wantErr:        "the change doesn't cover for fees",
		},
		{
			name:           "change doesn't cover for fees",
			deposits:       deposits(1, 2),
			fundMax:        true,
			satPerVbyte:    100_000,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: 0,
			wantChangeAmt:  0,
			wantErr:        "minimum channel funding size",
		},
		{
			name:           "satPerVbyte = 0 means no fee",
			deposits:       deposits(1, 2),
			localAmount:    sum(1, 2) - 50_000,
			satPerVbyte:    0,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2) - 50_000,
			wantChangeAmt:  50_000,
			wantErr:        "",
		},
		{
			name: "change >= input triggers efficiency error",
			deposits: []*deposit.Deposit{
				{Value: 40_000},
				{Value: 60_000},
			},
			localAmount:    40_000,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: 40_000,
			wantChangeAmt:  60_000 - feeWithChange,
			wantErr:        "is higher than an input value",
		},
		{
			name: "channel funding below minimum",
			deposits: []*deposit.Deposit{
				{Value: 30_000},
			},
			localAmount:    20_000 - 1,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: 0,
			wantChangeAmt:  0,
			wantErr:        "is lower than the minimum",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feeRate := chainfee.SatPerKVByte(
				tc.satPerVbyte * 1000,
			).FeePerKWeight()
			fundingAmt, changeAmt, err := withdraw.CalculateWithdrawalTxValaues(
				tc.deposits, tc.localAmount, tc.fundMax,
				feeRate, nil, tc.commitmentType,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantFundingAmt, fundingAmt)
				require.Equal(t, tc.wantChangeAmt, changeAmt)
			}
		})
	}
}
