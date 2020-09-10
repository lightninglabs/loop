package server

import (
	"context"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// SwapServerClient is an interface for the swap server client which uses golang
// types rather than the raw rpc messages.
type SwapServerClient interface {
	GetLoopOutTerms(ctx context.Context) (
		*LoopOutTerms, error)

	GetLoopOutQuote(ctx context.Context, amt btcutil.Amount, expiry int32,
		swapPublicationDeadline time.Time) (
		*LoopOutQuote, error)

	GetLoopInTerms(ctx context.Context) (
		*LoopInTerms, error)

	GetLoopInQuote(ctx context.Context, amt btcutil.Amount) (
		*LoopInQuote, error)

	NewLoopOutSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount, expiry int32,
		receiverKey [33]byte,
		swapPublicationDeadline time.Time) (
		*NewLoopOutResponse, error)

	PushLoopOutPreimage(ctx context.Context,
		preimage lntypes.Preimage) error

	NewLoopInSwap(ctx context.Context,
		swapHash lntypes.Hash, amount btcutil.Amount,
		senderKey [33]byte, swapInvoice string, lastHop *route.Vertex) (
		*NewLoopInResponse, error)

	// SubscribeLoopOutUpdates subscribes to loop out server state.
	SubscribeLoopOutUpdates(ctx context.Context,
		hash lntypes.Hash) (<-chan *Update, <-chan error, error)

	// SubscribeLoopInUpdates subscribes to loop in server state.
	SubscribeLoopInUpdates(ctx context.Context,
		hash lntypes.Hash) (<-chan *Update, <-chan error, error)
}
