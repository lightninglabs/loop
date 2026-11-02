package tapsdk

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	feeRateVBytesPerKVByte     uint64 = 1000
	feeRateWeightUnitsPerVByte uint64 = 4
)

// ErrInvalidFeeRate is returned when a fee rate cannot be parsed or
// represented.
var ErrInvalidFeeRate = errors.New("invalid fee rate")

// FeeRate represents an on-chain fee rate.
//
// The SDK accepts and displays fee rates in sat/vB. Internally the value is
// stored as sat/kvB, which is equivalent to fixed-point sat/vB with three
// decimal places, without using Lightning msat terminology for on-chain fees.
//
//nolint:recvcheck // UnmarshalText must mutate the receiver.
type FeeRate struct {
	satPerKVByte uint64
}

// NewFeeRateSatPerVByte constructs a fee rate from whole sat/vB.
func NewFeeRateSatPerVByte(satPerVByte uint64) (FeeRate, error) {
	if satPerVByte > math.MaxUint64/feeRateVBytesPerKVByte {
		return FeeRate{}, fmt.Errorf("%w: %d sat/vB overflows",
			ErrInvalidFeeRate, satPerVByte)
	}

	return NewFeeRateSatPerKVByte(satPerVByte * feeRateVBytesPerKVByte),
		nil
}

// NewFeeRateSatPerKVByte constructs a fee rate from sat/kvB.
func NewFeeRateSatPerKVByte(satPerKVByte uint64) FeeRate {
	return FeeRate{
		satPerKVByte: satPerKVByte,
	}
}

// ParseFeeRateSatPerVByte parses a decimal sat/vB fee rate.
func ParseFeeRateSatPerVByte(value string) (FeeRate, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return FeeRate{}, fmt.Errorf("%w: empty sat/vB value",
			ErrInvalidFeeRate)
	}

	if strings.HasPrefix(value, "-") {
		return FeeRate{}, fmt.Errorf("%w: negative sat/vB value %q",
			ErrInvalidFeeRate, value)
	}

	whole, fraction, ok := strings.Cut(value, ".")
	if whole == "" {
		whole = "0"
	}

	wholeSats, err := parseFeeRatePart(whole, "whole")
	if err != nil {
		return FeeRate{}, err
	}

	if !ok {
		return NewFeeRateSatPerVByte(wholeSats)
	}

	if len(fraction) > 3 {
		return FeeRate{}, fmt.Errorf("%w: %q has more than three "+
			"decimal places", ErrInvalidFeeRate, value)
	}

	for len(fraction) < 3 {
		fraction += "0"
	}

	fractionSats, err := parseFeeRatePart(fraction, "fractional")
	if err != nil {
		return FeeRate{}, err
	}

	if wholeSats > math.MaxUint64/feeRateVBytesPerKVByte {
		return FeeRate{}, fmt.Errorf("%w: %q overflows",
			ErrInvalidFeeRate, value)
	}

	satPerKVByte := wholeSats * feeRateVBytesPerKVByte
	if fractionSats > math.MaxUint64-satPerKVByte {
		return FeeRate{}, fmt.Errorf("%w: %q overflows",
			ErrInvalidFeeRate, value)
	}

	return NewFeeRateSatPerKVByte(satPerKVByte + fractionSats), nil
}

// IsZero returns true if the fee rate is unset.
func (r FeeRate) IsZero() bool {
	return r.satPerKVByte == 0
}

// SatPerKVByte returns the fee rate in sat/kvB.
func (r FeeRate) SatPerKVByte() uint64 {
	return r.satPerKVByte
}

// SatPerKWeight returns the fee rate in sat/kWU, rounded up.
func (r FeeRate) SatPerKWeight() uint64 {
	return ceilDiv(r.satPerKVByte, feeRateWeightUnitsPerVByte)
}

// SatPerVByteFloor returns the fee rate in sat/vB, rounded down.
func (r FeeRate) SatPerVByteFloor() uint64 {
	return r.satPerKVByte / feeRateVBytesPerKVByte
}

// SatPerVByteCeil returns the fee rate in sat/vB, rounded up.
func (r FeeRate) SatPerVByteCeil() uint64 {
	return ceilDiv(r.satPerKVByte, feeRateVBytesPerKVByte)
}

// String returns the fee rate formatted as sat/vB.
func (r FeeRate) String() string {
	return formatFeeRateSatPerVByte(r.satPerKVByte) + " sat/vB"
}

// MarshalText formats the fee rate as decimal sat/vB.
func (r FeeRate) MarshalText() ([]byte, error) {
	return []byte(formatFeeRateSatPerVByte(r.satPerKVByte)), nil
}

// UnmarshalText parses a decimal sat/vB fee rate.
func (r *FeeRate) UnmarshalText(text []byte) error {
	feeRate, err := ParseFeeRateSatPerVByte(string(text))
	if err != nil {
		return err
	}

	*r = feeRate
	return nil
}

func parseFeeRatePart(value, name string) (uint64, error) {
	if value == "" {
		return 0, nil
	}

	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("%w: %s part %q contains "+
				"non-digit characters", ErrInvalidFeeRate, name,
				value)
		}
	}

	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s part %q: %v",
			ErrInvalidFeeRate, name, value, err)
	}

	return parsed, nil
}

func formatFeeRateSatPerVByte(satPerKVByte uint64) string {
	whole := satPerKVByte / feeRateVBytesPerKVByte
	fraction := satPerKVByte % feeRateVBytesPerKVByte
	if fraction == 0 {
		return strconv.FormatUint(whole, 10)
	}

	formattedFraction := fmt.Sprintf("%03d", fraction)
	formattedFraction = strings.TrimRight(formattedFraction, "0")

	return fmt.Sprintf("%d.%s", whole, formattedFraction)
}

func ceilDiv(numerator, denominator uint64) uint64 {
	if numerator == 0 {
		return 0
	}

	return 1 + (numerator-1)/denominator
}
