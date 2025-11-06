package utils

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/status"
)

const (
	// chainNotifierStartupMessage is returned by lnd while the chain
	// notifier RPC sub-server is initialising.
	chainNotifierStartupMessage = "chain notifier RPC is still in the " +
		"process of starting"

	// chainNotifierRetryBackoff is the delay used between subscription
	// attempts while the chain notifier is still starting.
	chainNotifierRetryBackoff = 500 * time.Millisecond
)

// BlockEpochRegistrar represents the ability to subscribe to block epoch
// notifications.
type BlockEpochRegistrar interface {
	RegisterBlockEpochNtfn(ctx context.Context) (chan int32, chan error,
		error)
}

// RegisterBlockEpochNtfnWithRetry keeps retrying block epoch subscriptions as
// long as lnd reports that the chain notifier sub-server is still starting.
func RegisterBlockEpochNtfnWithRetry(ctx context.Context,
	registrar BlockEpochRegistrar) (chan int32, chan error, error) {

	for {
		blockChan, errChan, err := registrar.RegisterBlockEpochNtfn(ctx)
		if err == nil {
			return blockChan, errChan, nil
		}

		if !isChainNotifierStartingErr(err) {
			return nil, nil, err
		}

		log.Warnf("Chain notifier RPC not ready yet, retrying: %v",
			err)

		select {
		case <-time.After(chainNotifierRetryBackoff):
			continue

		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// isChainNotifierStartingErr checks whether an error indicates that lnd's chain
// notifier has not started yet.
func isChainNotifierStartingErr(err error) bool {
	if err == nil {
		return false
	}

	st, ok := status.FromError(err)
	if ok && strings.Contains(st.Message(), chainNotifierStartupMessage) {
		return true
	}

	return strings.Contains(err.Error(), chainNotifierStartupMessage)
}
