package loop

import (
	"time"

	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
)

// clientConfig contains config items for the swap client.
type clientConfig struct {
	LndServices       *lndclient.LndServices
	Server            swapServerClient
	Store             loopdb.SwapStore
	LsatStore         lsat.Store
	CreateExpiryTimer func(expiry time.Duration) <-chan time.Time
	LoopOutMaxParts   uint32
}
