package sweepbatcher

import (
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
)

// log_ is a logger that is initialized with no output filters. This
// means the package will not perform any logging by default until the
// caller requests it.
var log_ atomic.Pointer[btclog.Logger]

// log returns active logger.
func log() btclog.Logger {
	return *log_.Load()
}

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("SWEEP", nil))
}

// batchPrefixLogger returns a logger that prefixes all log messages with
// the ID.
func batchPrefixLogger(batchID string) btclog.Logger {
	return log().WithPrefix(fmt.Sprintf("[Batch %s]", batchID))
}

// UseLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseLogger(logger btclog.Logger) {
	log_.Store(&logger)
}

// debugf logs a message with level DEBUG.
func debugf(format string, params ...interface{}) {
	log().Debugf(format, params...)
}

// infof logs a message with level INFO.
func infof(format string, params ...interface{}) {
	log().Infof(format, params...)
}

// warnf logs a message with level WARN.
func warnf(format string, params ...interface{}) {
	log().Warnf(format, params...)
}

// errorf logs a message with level ERROR.
func errorf(format string, params ...interface{}) {
	log().Errorf(format, params...)
}
