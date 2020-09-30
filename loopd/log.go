package loopd

import (
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightningnetwork/lnd/build"
)

var (
	logWriter = build.NewRotatingLogWriter()

	log = build.NewSubLogger("LOOPD", logWriter.GenSubLogger)
)

func init() {
	setSubLogger("LOOPD", log, nil)
	addSubLogger("LOOP", loop.UseLogger)
	addSubLogger("LNDC", lndclient.UseLogger)
	addSubLogger("STORE", loopdb.UseLogger)
	addSubLogger(lsat.Subsystem, lsat.UseLogger)
	addSubLogger(liquidity.Subsystem, liquidity.UseLogger)
}

// addSubLogger is a helper method to conveniently create and register the
// logger of a sub system.
func addSubLogger(subsystem string, useLogger func(btclog.Logger)) {
	logger := build.NewSubLogger(subsystem, logWriter.GenSubLogger)
	setSubLogger(subsystem, logger, useLogger)
}

// setSubLogger is a helper method to conveniently register the logger of a sub
// system.
func setSubLogger(subsystem string, logger btclog.Logger,
	useLogger func(btclog.Logger)) {

	logWriter.RegisterSubLogger(subsystem, logger)
	if useLogger != nil {
		useLogger(logger)
	}
}
