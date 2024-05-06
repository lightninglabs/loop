package withdraw

import (
	"github.com/btcsuite/btclog"
)

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests it.
var log btclog.Logger

// UseLogger uses a specified Logger to output package logging info. This should
// be used in preference to SetLogWriter if the caller is also using btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}
