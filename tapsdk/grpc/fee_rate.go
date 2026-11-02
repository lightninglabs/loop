package grpc

import (
	"fmt"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
)

func feeRateSatPerKWeight(feeRate tapsdk.FeeRate) (uint32, error) {
	satPerKWeight := feeRate.SatPerKWeight()
	if satPerKWeight > uint64(^uint32(0)) {
		return 0, fmt.Errorf("fee rate %s overflows sat/kWU",
			feeRate)
	}

	return uint32(satPerKWeight), nil
}

func feeRateFromSatPerKWeight(satPerKWeight int32) tapsdk.FeeRate {
	if satPerKWeight <= 0 {
		return tapsdk.FeeRate{}
	}

	return tapsdk.NewFeeRateSatPerKVByte(uint64(satPerKWeight) * 4)
}
