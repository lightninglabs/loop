package client

import (
	"time"

	"github.com/lightninglabs/nautilus/lndclient"
)

// clientConfig contains config items for the swap client.
type clientConfig struct {
	LndServices       *lndclient.LndServices
	Server            swapServerClient
	Store             swapClientStore
	CreateExpiryTimer func(expiry time.Duration) <-chan time.Time
}
