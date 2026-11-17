package utils

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// MaxFeeToAmountRatio is the maximum fee to total amount ratio allowed
	// for a sweep transaction.
	MaxFeeToAmountRatio = 0.2
)

// ClampSweepFee caps a fee to a percentage of the provided total amount and
// verifies the resulting fee rate is not below the minimum relay fee. It
// returns the clamped fee, whether it was clamped, or an error if the clamped
// fee would fall below the minimum relay fee.
func ClampSweepFee(fee btcutil.Amount, totalAmount btcutil.Amount,
	ratio float64, minRelayFeeRate chainfee.SatPerKWeight,
	weight lntypes.WeightUnit) (btcutil.Amount, bool, error) {

	maxFeeAmount := btcutil.Amount(float64(totalAmount) * ratio)

	clampedFee := fee
	clamped := false
	if fee > maxFeeAmount {
		clampedFee = maxFeeAmount
		clamped = true
	}

	if minRelayFeeRate > 0 {
		clampedFeeRate := chainfee.NewSatPerKWeight(clampedFee, weight)
		if clampedFeeRate < minRelayFeeRate {
			return 0, clamped, fmt.Errorf("clamped fee rate %v is "+
				"less than minimum relay fee %v",
				clampedFeeRate, minRelayFeeRate)
		}
	}

	return clampedFee, clamped, nil
}

// FeeForWeight calculates a fee rounded down to whole satoshis without
// overflowing the intermediate product. It rejects negative rates, zero
// weights, and fees that cannot be represented as a btcutil.Amount.
func FeeForWeight(rate chainfee.SatPerKWeight,
	weight lntypes.WeightUnit) (btcutil.Amount, error) {

	if rate < 0 || weight == 0 {
		return 0, errors.New("invalid transaction fee parameters")
	}

	fee := new(big.Int).Mul(
		big.NewInt(int64(rate)),
		new(big.Int).SetUint64(uint64(weight)),
	)
	fee.Quo(fee, big.NewInt(1000))
	if !fee.IsInt64() {
		return 0, errors.New("transaction fee overflows int64")
	}

	return btcutil.Amount(fee.Int64()), nil
}
