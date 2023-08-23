package sweepbatcher

import (
	"fmt"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
)

// log is a logger that is initialized with no output filters. This
// means the package will not perform any logging by default until the
// caller requests it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("SWEEP", nil))
}

// batchPrefixLogger returns a logger that prefixes all log messages with the ID.
func batchPrefixLogger(batchID string) btclog.Logger {
	return build.NewPrefixLog(fmt.Sprintf("[Batch %s]", batchID), log)
}

// UseLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}
