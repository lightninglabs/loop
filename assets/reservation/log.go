package reservation

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
)

// Subsystem identifies reservation lifecycle logs.
const Subsystem = "ARESV"

var log btclog.Logger = build.NewSubLogger(Subsystem, nil)

// UseLogger sets the logger before starting reservation workers.
func UseLogger(logger btclog.Logger) {
	log = logger
}
