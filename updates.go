package loop

import (
	"context"

	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
)

// subscribeAndLogUpdates subscribes to updates for a swap and logs them. This
// function will block, so should run as a goroutine. Note that our subscription
// does not survive server restarts; we will simply not have update logs if the
// server restarts during swap execution.
func subscribeAndLogUpdates(ctx context.Context, hash lntypes.Hash,
	log *swap.PrefixLog, subscribe func(context.Context,
		lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error)) {

	subscribeChan, errChan, err := subscribe(ctx, hash)
	if err != nil {
		log.Errorf("could not get swap subscription: %v", err)
		return
	}

	for {
		select {
		// Consume any updates and log them.
		case update := <-subscribeChan:
			log.Infof("Server update: %v received, "+
				"timestamp: %v", update.State, update.Timestamp)

		// If we get an error from the server, we check whether it is
		// due to server exit, or restart, and log this information
		// for the client. Otherwise, we just log non-nil errors.
		case err := <-errChan:
			switch err {
			case errServerSubscriptionComplete:
				log.Infof("swap subscription: %v", err)

			case nil:

			default:
				log.Errorf("swap subscription error: %v", err)
			}

			return

		// Exit if our context is cancelled.
		case <-ctx.Done():
			return
		}
	}
}
