package main

import (
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/looprpc"
)

const (
	// feePPMBase converts ppm fee limits into satoshi portions.
	feePPMBase = 1_000_000

	// maxSwapFeeSatLimit is the largest absolute swap-fee cap accepted by
	// the CLI for static loop-ins.
	maxSwapFeeSatLimit = uint64(10_000_000)
)

// resolveMaxSwapFee computes the effective maximum swap fee (in satoshis) for a
// static address loop-in, applying user-supplied caps when present. If both a
// satoshi cap and a ppm cap are provided, the tighter (lower) of the two is
// used. When neither override is set the quoted fee from the server is returned
// unchanged.
//
// The function also performs an early check against the current quote: if the
// server-quoted fee already exceeds the resolved cap the caller receives an
// error so the swap can be rejected before confirmation.
func resolveMaxSwapFee(quoteReq *looprpc.QuoteRequest,
	quote *looprpc.InQuoteResponse, satIsSet bool, maxFeeSat uint64,
	ppmIsSet bool, maxFeePpm uint64) (btcutil.Amount, error) {

	// If a flag is used, make sure the value is within a reasonable range.
	if satIsSet && maxFeeSat == 0 {
		return 0, fmt.Errorf("--max_swap_fee_sat must be positive")
	}
	if satIsSet && maxFeeSat > maxSwapFeeSatLimit {
		return 0, fmt.Errorf("--max_swap_fee_sat must be <= %d",
			maxSwapFeeSatLimit)
	}
	if ppmIsSet && maxFeePpm == 0 {
		return 0, fmt.Errorf("--max_swap_fee_ppm must be positive")
	}
	if ppmIsSet && maxFeePpm > feePPMBase {
		return 0, fmt.Errorf("--max_swap_fee_ppm must be <= %d",
			feePPMBase)
	}

	// When no override is set, fall back to the quoted fee.
	if !satIsSet && !ppmIsSet {
		return btcutil.Amount(quote.SwapFeeSat), nil
	}

	// Determine the effective swap amount. For static loop-ins the user
	// may omit the amount, in which case the server derives it from the
	// selected deposits and returns it in QuotedAmt.
	swapAmt := quoteReq.Amt
	if swapAmt == 0 {
		swapAmt = quote.QuotedAmt
	}

	var ppmCapSat uint64

	if ppmIsSet {
		if swapAmt <= 0 {
			return 0, fmt.Errorf("swap amount %d invalid for "+
				"ppm fee cap", swapAmt)
		}

		ppmCapSat = ppmCapForSwapAmount(swapAmt, maxFeePpm)
		if ppmCapSat == 0 {
			return 0, fmt.Errorf("ppm cap rounds to 0 sat for "+
				"swap amount %d; use "+
				"--max_swap_fee_sat instead", swapAmt)
		}
	}

	// Pick the tighter cap when both are set.
	var resolvedSat uint64
	switch {
	case satIsSet && ppmIsSet:
		resolvedSat = min(maxFeeSat, ppmCapSat)

	case satIsSet:
		resolvedSat = maxFeeSat

	// maxFeePpm is bounded to feePPMBase, so the ppm-derived cap
	// cannot exceed swapAmt and therefore fits into uint64.
	default:
		resolvedSat = ppmCapSat
	}

	// Reject early if the quote already exceeds the cap.
	if quote.SwapFeeSat > int64(resolvedSat) {
		return 0, fmt.Errorf("quoted swap fee %d sat exceeds maximum "+
			"allowed %d sat", quote.SwapFeeSat, resolvedSat)
	}

	return btcutil.Amount(int64(resolvedSat)), nil
}

// ppmCapForSwapAmount converts a ppm fee limit to a satoshi cap for the given
// swap amount, rounding down to whole satoshis. Big integers are used
// internally to avoid intermediate multiplication overflow, but the returned
// value always fits into uint64 because callers bound maxFeePpm to
// feePPMBase.
func ppmCapForSwapAmount(swapAmt int64, maxFeePpm uint64) uint64 {
	swapAmtBig := new(big.Int).SetInt64(swapAmt)
	maxFeePpmBig := new(big.Int).SetUint64(maxFeePpm)

	capSat := new(big.Int).Quo(
		new(big.Int).Mul(swapAmtBig, maxFeePpmBig),
		big.NewInt(feePPMBase),
	)

	return capSat.Uint64()
}
