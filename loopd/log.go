package loopd

import (
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/hyperloop"
	"github.com/lightninglabs/loop/instantout"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/notifications"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/sweepbatcher"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

const Subsystem = "LOOPD"

var (
	log         btclog.Logger
	interceptor signal.Interceptor
)

// SetupLoggers initializes all package-global logger variables.
func SetupLoggers(root *build.RotatingLogWriter, intercept signal.Interceptor) {
	genLogger := genSubLogger(root, intercept)

	log = build.NewSubLogger(Subsystem, genLogger)
	interceptor = intercept

	lnd.SetSubLogger(root, Subsystem, log)
	lnd.AddSubLogger(root, "LOOP", intercept, loop.UseLogger)
	lnd.AddSubLogger(root, "SWEEP", intercept, sweepbatcher.UseLogger)
	lnd.AddSubLogger(root, "LNDC", intercept, lndclient.UseLogger)
	lnd.AddSubLogger(root, "STORE", intercept, loopdb.UseLogger)
	lnd.AddSubLogger(root, l402.Subsystem, intercept, l402.UseLogger)
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
		root, hyperloop.Subsystem, intercept, hyperloop.UseLogger,
	)
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
