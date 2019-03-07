package loop

import (
	"time"

	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
)

// clientConfig contains config items for the swap client.
type clientConfig struct {
	LndServices       *lndclient.LndServices
	Server            swapServerClient
	Store             loopdb.SwapStore
	CreateExpiryTimer func(expiry time.Duration) <-chan time.Time
}
