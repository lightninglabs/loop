package loopd

import (
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/notifications"
	"github.com/lightninglabs/loop/staticaddr"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/sweepbatcher"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

const Subsystem = "LOOPD"

var (
	log_        atomic.Pointer[btclog.Logger]
	interceptor signal.Interceptor
)

// log returns active logger.
func log() btclog.Logger {
	return *log_.Load()
}

// setLogger uses a specified Logger to output package logging info.
func setLogger(logger btclog.Logger) {
	log_.Store(&logger)
}

// tracef logs a message with level TRACE.
func tracef(format string, params ...interface{}) {
	log().Tracef(format, params...)
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

// SetupLoggers initializes all package-global logger variables.
func SetupLoggers(root *build.SubLoggerManager, intercept signal.Interceptor) {
	genLogger := genSubLogger(root, intercept)

	logger := build.NewSubLogger(Subsystem, genLogger)
	setLogger(logger)

	interceptor = intercept

	lnd.SetSubLogger(root, Subsystem, logger)
	lnd.AddSubLogger(root, "LOOP", intercept, loop.UseLogger)
	lnd.AddSubLogger(root, "SWEEP", intercept, sweepbatcher.UseLogger)
	lnd.AddSubLogger(root, "LNDC", intercept, lndclient.UseLogger)
	lnd.AddSubLogger(root, "STORE", intercept, loopdb.UseLogger)
	lnd.AddSubLogger(root, l402.Subsystem, intercept, l402.UseLogger)
	lnd.AddSubLogger(
		root, staticaddr.Subsystem, intercept, staticaddr.UseLogger,
	)
	lnd.AddSubLogger(
		root, liquidity.Subsystem, intercept, liquidity.UseLogger,
	)
	lnd.AddSubLogger(root, fsm.Subsystem, intercept, fsm.UseLogger)
	lnd.AddSubLogger(
		root, reservation.Subsystem, intercept, reservation.UseLogger,
	)
	lnd.AddSubLogger(
		root, instantout.Subsystem, intercept, instantout.UseLogger,
	)
	lnd.AddSubLogger(
		root, notifications.Subsystem, intercept, notifications.UseLogger,
	)
	lnd.AddSubLogger(
		root, sweep.Subsystem, intercept, sweep.UseLogger,
	)

	lnd.AddSubLogger(
		root, assets.Subsystem, intercept, assets.UseLogger,
	)
	lnd.AddSubLogger(
		root, utils.Subsystem, intercept, utils.UseLogger,
	)
}

// genSubLogger creates a logger for a subsystem. We provide an instance of
// a signal.Interceptor to be able to shut down in the case of a critical error.
func genSubLogger(root *build.SubLoggerManager,
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
