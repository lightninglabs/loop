package withdraw

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestCalculateFundingTxValues tests the CalculateWithdrawalTxValues function
// with various channel funding scenarios.
func TestCalculateFundingTxValues(t *testing.T) {
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
			{
				Value: funding.MinChanFundingSize - 1,
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

	weightWithoutChange, err := WithdrawalTxWeight(
		len(allDeposits), nil, lnrpc.CommitmentType_ANCHORS, false,
	)
	require.NoError(t, err)

	feeWithoutChange := chanOpenFeeRate.FeeForWeight(
		weightWithoutChange,
	)

	weightWithChange, err := WithdrawalTxWeight(
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
			deposits:       deposits(1, 2, 3),
			fundMax:        true,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2, 3) - feeWithoutChange,
			wantChangeAmt:  0,
			wantErr:        "",
		},
		{
			name:           "local_amt",
			deposits:       deposits(1, 2, 3),
			localAmount:    sum(1, 2, 3) - 10_000,
			satPerVbyte:    1,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			wantFundingAmt: sum(1, 2, 3) - 10_000,
			wantChangeAmt:  10_000 - feeWithChange,
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
			name:           "minimum channel funding size",
			deposits:       deposits(3),
			fundMax:        true,
			satPerVbyte:    1,
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
			fundingAmt, changeAmt, err := CalculateWithdrawalTxValues(
				tc.deposits, tc.localAmount, feeRate, nil,
				tc.commitmentType,
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

// TestCalculateWithdrawalTxValuesCommitmentTypeParity ensures channel funding
// value calculations are identical whether we derive output type from
// commitment type or from an equivalent funding address type.
func TestCalculateWithdrawalTxValuesCommitmentTypeParity(t *testing.T) {
	t.Parallel()

	feeRate := chainfee.SatPerKVByte(1000).FeePerKWeight()
	deposits := []*deposit.Deposit{
		{Value: 500_000},
		{Value: 300_000},
	}

	p2wshAddr, err := btcutil.NewAddressWitnessScriptHash(
		make([]byte, 32), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	taprootAddr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	type testCase struct {
		name           string
		commitmentType lnrpc.CommitmentType
		addr           btcutil.Address
	}

	cases := []testCase{
		{
			name:           "anchors and p2wsh",
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			addr:           p2wshAddr,
		},
		{
			name:           "simple taproot and p2tr",
			commitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
			addr:           taprootAddr,
		},
	}

	selectedAmounts := []btcutil.Amount{
		0,       // fundmax/no change path
		600_000, // selected amount with potential change path
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, selected := range selectedAmounts {
				fundingByType, changeByType, err := CalculateWithdrawalTxValues(
					deposits, selected, feeRate, nil,
					tc.commitmentType,
				)
				require.NoError(t, err)

				fundingByAddr, changeByAddr, err := CalculateWithdrawalTxValues(
					deposits, selected, feeRate, tc.addr,
					lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
				)
				require.NoError(t, err)

				require.Equal(t, fundingByType, fundingByAddr)
				require.Equal(t, changeByType, changeByAddr)
			}
		})
	}
}
