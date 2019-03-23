package loop

import (
	"fmt"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lntypes"
)

// log is a logger that is initialized with no output filters.  This means the
// package will not perform any logging by default until the caller requests
// it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("LOOP", nil))
}

// DisableLog disables all library log output.  Logging output is disabled by
// default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.  This
// should be used in preference to SetLogWriter if the caller is also using
// btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}

// SwapLog logs with a short swap hash prefix.
type SwapLog struct {
	// Logger is the underlying based logger.
	Logger btclog.Logger

	// Hash is the hash the identifies the target swap.
	Hash lntypes.Hash
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

// ShortHash returns a shortened version of the hash suitable for use in
// logging.
func ShortHash(hash *lntypes.Hash) string {
	return hash.String()[:6]
}
