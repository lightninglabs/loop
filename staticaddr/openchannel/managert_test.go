package openchannel

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
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
		lnd          = test.NewMockLnd()
		feeEstimator = lnd.WalletKit

		chanOpenFeeRate = chainfee.SatPerKVByte(
			satPerVbyte * 1000,
		).FeePerKWeight()

		defaultChanOpenFeeRate, _ = feeEstimator.EstimateFeeRate(
			context.Background(), defaultConfTarget,
		)

		weightWithoutChange = chanOpenTxWeight(
			len(allDeposits), false, looprpc.CommitmentType_ANCHORS,
		)
		feeWithoutChange = chanOpenFeeRate.FeeForWeight(
			weightWithoutChange,
		)

		weightWithChange = chanOpenTxWeight(
			len(allDeposits), true, looprpc.CommitmentType_ANCHORS,
		)
		feeWithChange = chanOpenFeeRate.FeeForWeight(
			weightWithChange,
		)

		// defaultFeeWithoutChange = defaultChanOpenFeeRate.FeeForWeight(weightWithoutChange)
		defaultFeeWithChange = defaultChanOpenFeeRate.FeeForWeight(weightWithChange)

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

	cases := []struct {
		name           string
		deposits       []*deposit.Deposit
		localAmount    btcutil.Amount
		fundMax        bool
		satPerVbyte    uint64
		commitmentType looprpc.CommitmentType
		wantFundingAmt btcutil.Amount
		wantChangeAmt  btcutil.Amount
		wantErr        string
	}{
		{
			name:           "fundmax",
			deposits:       deposits(1, 2),
			fundMax:        true,
			satPerVbyte:    1,
			commitmentType: looprpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2) - feeWithoutChange,
			wantChangeAmt:  0,
			wantErr:        "",
		},
		{
			name:           "local_amt",
			deposits:       deposits(1, 2),
			localAmount:    sum(1, 2) - 50_000,
			satPerVbyte:    1,
			commitmentType: looprpc.CommitmentType_ANCHORS,
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
			commitmentType: looprpc.CommitmentType_ANCHORS,
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
			commitmentType: looprpc.CommitmentType_ANCHORS,
			wantFundingAmt: 0,
			wantChangeAmt:  0,
			wantErr:        "the change doesn't cover for fees",
		},
		{
			name:           "change doesn't cover for fees",
			deposits:       deposits(1, 2),
			fundMax:        true,
			satPerVbyte:    100_000,
			commitmentType: looprpc.CommitmentType_ANCHORS,
			wantFundingAmt: 0,
			wantChangeAmt:  0,
			wantErr:        "minimum channel funding size",
		},
		{
			name:           "satPerVbyte = 0 triggers fee estimator",
			deposits:       deposits(1, 2),
			localAmount:    sum(1, 2) - 50_000,
			satPerVbyte:    0,
			commitmentType: looprpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2) - 50_000,
			wantChangeAmt:  50_000 - defaultFeeWithChange,
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
			commitmentType: looprpc.CommitmentType_ANCHORS,
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
			commitmentType: looprpc.CommitmentType_ANCHORS,
			wantFundingAmt: 0,
			wantChangeAmt:  0,
			wantErr:        "is lower than the minimum",
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fundingAmt, changeAmt, err := calculateFundingTxValaues(
				ctx, tc.deposits, tc.localAmount, tc.fundMax,
				tc.satPerVbyte, tc.commitmentType, feeEstimator,
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
