package swap

import (
	"errors"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// FeeRateTotalParts defines the granularity of the fee rate.
	// Throughout the codebase, we'll use fix based arithmetic to compute
	// fees.
	FeeRateTotalParts = 1e6
)

// CalcFee returns the swap fee for a given swap amount.
func CalcFee(amount, feeBase btcutil.Amount, feeRate int64) btcutil.Amount {
	return feeBase + amount*btcutil.Amount(feeRate)/
		btcutil.Amount(FeeRateTotalParts)
}

// FeeRateAsPercentage converts a feerate to a percentage.
func FeeRateAsPercentage(feeRate int64) float64 {
	return float64(feeRate) / (FeeRateTotalParts / 100)
}

// DecodeInvoice gets the hash and the amount of an invoice.
// It requires an amount to be specified.
func DecodeInvoice(params *chaincfg.Params,
	payReq string) (lntypes.Hash, btcutil.Amount, error) {

	swapPayReq, err := zpay32.Decode(
		payReq, params,
	)
	if err != nil {
		return lntypes.Hash{}, 0, err
	}

	if swapPayReq.MilliSat == nil {
		return lntypes.Hash{}, 0, errors.New("no amount in invoice")
	}

	var hash lntypes.Hash
	copy(hash[:], swapPayReq.PaymentHash[:])

	return hash, swapPayReq.MilliSat.ToSatoshis(), nil
}
