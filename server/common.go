package server

import (
	"time"

	"github.com/lightninglabs/loop/server/serverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapInfoKit contains common swap info fields.
type SwapInfoKit struct {
	// Hash is the sha256 hash of the preimage that unlocks the htlcs. It
	// is used to uniquely identify this swap.
	Hash lntypes.Hash

	// LastUpdateTime is the time of the last update of this swap.
	LastUpdateTime time.Time
}

// Update summarizes an update from the swap server.
type Update struct {
	// State is the state that the server has sent us.
	State serverrpc.ServerSwapState

	// Timestamp is the time of the server state update.
	Timestamp time.Time
}
