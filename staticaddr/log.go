package staticaddr

import (
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/withdraw"
	"github.com/lightningnetwork/lnd/build"
)

// Subsystem defines the sub system name of this package.
const Subsystem = "SADDR"

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger(Subsystem, nil))
}

// UseLogger uses a specified Logger to output package logging info. This should
// be used in preference to SetLogWriter if the caller is also using btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
	address.UseLogger(log)
	deposit.UseLogger(log)
	withdraw.UseLogger(log)
}
