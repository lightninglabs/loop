package loop

import (
	"time"

	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"google.golang.org/grpc"
)

// clientConfig contains config items for the swap client.
type clientConfig struct {
	LndServices       *lndclient.LndServices
	Server            swapServerClient
	Conn              *grpc.ClientConn
	Store             loopdb.SwapStore
	LsatStore         lsat.Store
	CreateExpiryTimer func(expiry time.Duration) <-chan time.Time
	LoopOutMaxParts   uint32
}
