package utils

import (
	"fmt"
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapLog logs with a short swap hash prefix.
type SwapLog struct {
	Logger btclog.Logger
	Hash   lntypes.Hash
}

// Infof formats message according to format specifier and writes to
// log with LevelInfo.
func (s *SwapLog) Infof(format string, params ...interface{}) {
	s.Logger.Infof(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// Warnf formats message according to format specifier and writes to
// to log with LevelError.
func (s *SwapLog) Warnf(format string, params ...interface{}) {
	s.Logger.Warnf(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// Errorf formats message according to format specifier and writes to
// to log with LevelError.
func (s *SwapLog) Errorf(format string, params ...interface{}) {
	s.Logger.Errorf(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)

}
