package loopd

import (
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

const Subsystem = "LOOPD"

var (
	logWriter   *build.RotatingLogWriter
	log         btclog.Logger
	interceptor signal.Interceptor
)

// SetupLoggers initializes all package-global logger variables.
func SetupLoggers(root *build.RotatingLogWriter, intercept signal.Interceptor) {
	genLogger := genSubLogger(root, intercept)

	logWriter = root
	log = build.NewSubLogger(Subsystem, genLogger)
	interceptor = intercept

	lnd.SetSubLogger(root, Subsystem, log)
	lnd.AddSubLogger(root, "LOOP", intercept, loop.UseLogger)
	lnd.AddSubLogger(root, "LNDC", intercept, lndclient.UseLogger)
	lnd.AddSubLogger(root, "STORE", intercept, loopdb.UseLogger)
	lnd.AddSubLogger(root, lsat.Subsystem, intercept, lsat.UseLogger)
	lnd.AddSubLogger(
		root, liquidity.Subsystem, intercept, liquidity.UseLogger,
	)
	lnd.AddSubLogger(root, fsm.Subsystem, intercept, fsm.UseLogger)
}

// genSubLogger creates a logger for a subsystem. We provide an instance of
// a signal.Interceptor to be able to shutdown in the case of a critical error.
func genSubLogger(root *build.RotatingLogWriter,
	interceptor signal.Interceptor) func(string) btclog.Logger {

	// Create a shutdown function which will request shutdown from our
	// interceptor if it is listening.
	shutdown := func() {
		if !interceptor.Listening() {
			return
		}

		interceptor.RequestShutdown()
	}

	// Return a function which will create a sublogger from our root
	// logger without shutdown fn.
	return func(tag string) btclog.Logger {
		return root.GenSubLogger(tag, shutdown)
	}
}
