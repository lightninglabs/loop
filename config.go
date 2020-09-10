package loop

import (
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightninglabs/loop/server"
)

// clientConfig contains config items for the swap client.
type clientConfig struct {
	LndServices       *lndclient.LndServices
	Server            server.SwapServerClient
	Store             loopdb.SwapStore
	LsatStore         lsat.Store
	CreateExpiryTimer func(expiry time.Duration) <-chan time.Time
}
