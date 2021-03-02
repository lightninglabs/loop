package liquidity

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultSwapFeePPM is the default limit we place on swap fees,
	// expressed as parts per million of swap volume, 0.5%.
	defaultSwapFeePPM = 5000

	// defaultRoutingFeePPM is the default limit we place on routing fees
	// for the swap invoice, expressed as parts per million of swap volume,
	// 1%.
	defaultRoutingFeePPM = 10000

	// defaultRoutingFeePPM is the default limit we place on routing fees
	// for the prepay invoice, expressed as parts per million of prepay
	// volume, 0.5%.
	defaultPrepayRoutingFeePPM = 5000

	// defaultMaximumMinerFee is the default limit we place on miner fees
	// per swap. We apply a multiplier to this default fee to guard against
	// the case where we have broadcast the preimage, then fees spike and
	// we need to sweep the preimage.
	defaultMaximumMinerFee = 15000 * 100

	// defaultMaximumPrepay is the default limit we place on prepay
	// invoices.
	defaultMaximumPrepay = 30000

	// defaultSweepFeeRateLimit is the default limit we place on estimated
	// sweep fees, (750 * 4 /1000 = 3 sat/vByte).
	defaultSweepFeeRateLimit = chainfee.SatPerKWeight(750)
)

var (
	// ErrZeroMinerFee is returned if a zero maximum miner fee is set.
	ErrZeroMinerFee = errors.New("maximum miner fee must be non-zero")

	// ErrZeroSwapFeePPM is returned if a zero server fee ppm is set.
	ErrZeroSwapFeePPM = errors.New("swap fee PPM must be non-zero")

	// ErrZeroRoutingPPM is returned if a zero routing fee ppm is set.
	ErrZeroRoutingPPM = errors.New("routing fee PPM must be non-zero")

	// ErrZeroPrepayPPM is returned if a zero prepay routing fee ppm is set.
	ErrZeroPrepayPPM = errors.New("prepay routing fee PPM must be non-zero")

	// ErrZeroPrepay is returned if a zero maximum prepay is set.
	ErrZeroPrepay = errors.New("maximum prepay must be non-zero")

	// ErrInvalidSweepFeeRateLimit is returned if an invalid sweep fee limit
	// is set.
	ErrInvalidSweepFeeRateLimit = fmt.Errorf("sweep fee rate limit must "+
		"be > %v sat/vByte",
		satPerKwToSatPerVByte(chainfee.AbsoluteFeePerKwFloor))
)

// Compile time assertion that FeeCategoryLimit implements FeeLimit.
var _ FeeLimit = (*FeeCategoryLimit)(nil)

// FeeCategoryLimit is an implementation of the fee limit interface which sets
// a specific fee limit per fee category.
type FeeCategoryLimit struct {
	// MaximumPrepay is the maximum prepay amount we are willing to pay per
	// swap.
	MaximumPrepay btcutil.Amount

	// MaximumSwapFeePPM is the maximum server fee we are willing to pay per
	// swap expressed as parts per million of the swap volume.
	MaximumSwapFeePPM uint64

	// MaximumRoutingFeePPM is the maximum off-chain routing fee we
	// are willing to pay for off chain invoice routing fees per swap,
	// expressed as parts per million of the swap amount.
	MaximumRoutingFeePPM uint64

	// MaximumPrepayRoutingFeePPM is the maximum off-chain routing fee we
	// are willing to pay for off chain prepay routing fees per swap,
	// expressed as parts per million of the prepay amount.
	MaximumPrepayRoutingFeePPM uint64

	// MaximumMinerFee is the maximum on chain fee that we cap our miner
	// fee at in case where we need to claim on chain because we have
	// revealed the preimage, but fees have spiked. We will not initiate a
	// swap if we estimate that the sweep cost will be above our sweep
	// fee limit, and we use fee estimates at time of sweep to set our fees,
	// so this is just a sane cap covering the special case where we need to
	// sweep during a fee spike.
	MaximumMinerFee btcutil.Amount

	// SweepFeeRateLimit is the limit that we place on our estimated sweep
	// fee. A swap will not be suggested if estimated fee rate is above this
	// value.
	SweepFeeRateLimit chainfee.SatPerKWeight
}

// NewFeeCategoryLimit created a new fee limit struct which sets individual
// fee limits per category.
func NewFeeCategoryLimit(swapFeePPM, routingFeePPM, prepayFeePPM uint64,
	minerFee, prepay btcutil.Amount,
	sweepLimit chainfee.SatPerKWeight) *FeeCategoryLimit {

	return &FeeCategoryLimit{
		MaximumPrepay:              prepay,
		MaximumSwapFeePPM:          swapFeePPM,
		MaximumRoutingFeePPM:       routingFeePPM,
		MaximumPrepayRoutingFeePPM: prepayFeePPM,
		MaximumMinerFee:            minerFee,
		SweepFeeRateLimit:          sweepLimit,
	}
}

func defaultFeeCategoryLimit() *FeeCategoryLimit {
	return NewFeeCategoryLimit(defaultSwapFeePPM, defaultRoutingFeePPM,
		defaultPrepayRoutingFeePPM, defaultMaximumMinerFee,
		defaultMaximumPrepay, defaultSweepFeeRateLimit)
}

// String returns the string representation of our fee category limits.
func (f *FeeCategoryLimit) String() string {
	return fmt.Sprintf("fee categories: maximum prepay: %v, maximum "+
		"miner fee: %v, maximum swap fee ppm: %v, maximum "+
		"routing fee ppm: %v, maximum prepay routing fee ppm: %v,"+
		"sweep fee limit: %v", f.MaximumPrepay, f.MaximumMinerFee,
		f.MaximumSwapFeePPM, f.MaximumRoutingFeePPM,
		f.MaximumPrepayRoutingFeePPM, f.SweepFeeRateLimit,
	)
}

func (f *FeeCategoryLimit) validate() error {
	// Check that we have non-zero fee limits.
	if f.MaximumSwapFeePPM == 0 {
		return ErrZeroSwapFeePPM
	}

	if f.MaximumRoutingFeePPM == 0 {
		return ErrZeroRoutingPPM
	}

	if f.MaximumPrepayRoutingFeePPM == 0 {
		return ErrZeroPrepayPPM
	}

	if f.MaximumPrepay == 0 {
		return ErrZeroPrepay
	}

	if f.MaximumMinerFee == 0 {
		return ErrZeroMinerFee
	}

	// Check that our sweep limit is above our minimum fee rate. We use
	// absolute fee floor rather than kw floor because we will allow users
	// to specify fee rate is sat/vByte and want to allow 1 sat/vByte.
	if f.SweepFeeRateLimit < chainfee.AbsoluteFeePerKwFloor {
		return ErrInvalidSweepFeeRateLimit
	}

	return nil
}

// mayLoopOut checks our estimated loop out sweep fee against our sweep limit.
func (f *FeeCategoryLimit) mayLoopOut(estimate chainfee.SatPerKWeight) error {
	if estimate > f.SweepFeeRateLimit {
		log.Debugf("Current fee estimate to sweep: %v sat/vByte "+
			"exceeds limit of: %v sat/vByte",
			satPerKwToSatPerVByte(estimate),
			satPerKwToSatPerVByte(f.SweepFeeRateLimit))

		return newReasonError(ReasonSweepFees)
	}

	return nil
}

// loopOutLimits checks whether the quote provided is within our fee limits.
func (f *FeeCategoryLimit) loopOutLimits(amount btcutil.Amount,
	quote *loop.LoopOutQuote) error {

	maxFee := ppmToSat(amount, f.MaximumSwapFeePPM)

	if quote.SwapFee > maxFee {
		log.Debugf("quoted swap fee: %v > maximum swap fee: %v",
			quote.SwapFee, maxFee)

		return newReasonError(ReasonSwapFee)
	}

	if quote.MinerFee > f.MaximumMinerFee {
		log.Debugf("quoted miner fee: %v > maximum miner "+
			"fee: %v", quote.MinerFee, f.MaximumMinerFee)

		return newReasonError(ReasonMinerFee)
	}

	if quote.PrepayAmount > f.MaximumPrepay {
		log.Debugf("quoted prepay: %v > maximum prepay: %v",
			quote.PrepayAmount, f.MaximumPrepay)

		return newReasonError(ReasonPrepay)
	}

	return nil
}

// loopOutFees returns the prepay and routing and miner fees we are willing to
// pay for a loop out swap.
func (f *FeeCategoryLimit) loopOutFees(amount btcutil.Amount,
	quote *loop.LoopOutQuote) (btcutil.Amount, btcutil.Amount,
	btcutil.Amount) {

	prepayMaxFee := ppmToSat(
		quote.PrepayAmount, f.MaximumPrepayRoutingFeePPM,
	)

	routeMaxFee := ppmToSat(amount, f.MaximumRoutingFeePPM)

	return prepayMaxFee, routeMaxFee, f.MaximumMinerFee
}
