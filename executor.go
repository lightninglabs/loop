package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/queue"
)

// executorConfig contains executor configuration data.
type executorConfig struct {
	lnd *lndclient.LndServices

	sweeper *sweep.Sweeper

	store loopdb.SwapStore

	createExpiryTimer func(expiry time.Duration) <-chan time.Time

	loopOutMaxParts uint32

	totalPaymentTimeout time.Duration

	maxPaymentRetries int

	cancelSwap func(ctx context.Context, details *outCancelDetails) error

	verifySchnorrSig func(pubKey *btcec.PublicKey, hash, sig []byte) error
}

// executor is responsible for executing swaps.
//
// TODO(roasbeef): rename to SubSwapper.
type executor struct {
	wg            sync.WaitGroup
	newSwaps      chan genericSwap
	currentHeight uint32
	ready         chan struct{}

	sync.Mutex

	executorConfig
}

// newExecutor returns a new swap executor instance.
func newExecutor(cfg *executorConfig) *executor {
	return &executor{
		executorConfig: *cfg,
		newSwaps:       make(chan genericSwap),
		ready:          make(chan struct{}),
	}
}

// run starts the executor event loop. It accepts and executes new swaps,
// providing them with required config data.
func (s *executor) run(mainCtx context.Context,
	statusChan chan<- SwapInfo,
	abandonChans map[lntypes.Hash]chan struct{}) error {

	var (
		err            error
		blockEpochChan <-chan int32
		blockErrorChan <-chan error
	)

	for {
		blockEpochChan, blockErrorChan, err =
			s.lnd.ChainNotifier.RegisterBlockEpochNtfn(mainCtx)

		if err == nil {
			break
		}

		if strings.Contains(err.Error(),
			"in the process of starting") {

			log.Warnf("LND chain notifier server not ready yet, " +
				"retrying with delay")

			// Give chain notifier some time to start and try to
			// re-attempt block epoch subscription.
			select {
			case <-time.After(500 * time.Millisecond):
				continue

			case <-mainCtx.Done():
				return err
			}
		}

		return err
	}

	// Before starting, make sure we have an up-to-date block height.
	// Otherwise, we might reveal a preimage for a swap that is already
	// expired.
	log.Infof("Wait for first block notification")

	var height int32
	setHeight := func(h int32) {
		height = h
		atomic.StoreUint32(&s.currentHeight, uint32(h))
	}

	select {
	case h := <-blockEpochChan:
		setHeight(h)
	case err := <-blockErrorChan:
		return err
	case <-mainCtx.Done():
		return mainCtx.Err()
	}

	// Start main event loop.
	log.Infof("Starting event loop at height %v", height)

	// Signal that executor being ready with an up-to-date block height.
	close(s.ready)

	// Use a map to administer the individual notification queues for the
	// swaps.
	blockEpochQueues := make(map[int]*queue.ConcurrentQueue)

	// On exit, stop all queue goroutines.
	defer func() {
		for _, queue := range blockEpochQueues {
			queue.Stop()
		}
	}()

	swapDoneChan := make(chan int)
	nextSwapID := 0

	for {
		select {
		case newSwap := <-s.newSwaps:
			queue := queue.NewConcurrentQueue(10)
			queue.Start()
			swapID := nextSwapID
			blockEpochQueues[swapID] = queue

			s.wg.Add(1)
			go func() {
				defer s.wg.Done()

				err := newSwap.execute(mainCtx, &executeConfig{
					statusChan:         statusChan,
					sweeper:            s.sweeper,
					blockEpochChan:     queue.ChanOut(),
					timerFactory:       s.executorConfig.createExpiryTimer,
					loopOutMaxParts:    s.executorConfig.loopOutMaxParts,
					totalPaymentTimout: s.executorConfig.totalPaymentTimeout,
					maxPaymentRetries:  s.executorConfig.maxPaymentRetries,
					cancelSwap:         s.executorConfig.cancelSwap,
					verifySchnorrSig:   s.executorConfig.verifySchnorrSig,
				}, height)
				if err != nil && !errors.Is(
					err, context.Canceled,
				) {

					log.Errorf("Execute error: %v", err)
				}

				// If a loop-in ended we have to remove its
				// abandon channel from our abandonChans map
				// since the swap finalized.
				if swap, ok := newSwap.(*loopInSwap); ok {
					s.Lock()
					delete(abandonChans, swap.hash)
					s.Unlock()
				}

				select {
				case swapDoneChan <- swapID:
				case <-mainCtx.Done():
				}
			}()

			nextSwapID++

		case doneID := <-swapDoneChan:
			queue, ok := blockEpochQueues[doneID]
			if !ok {
				return fmt.Errorf(
					"swap id %v not found in queues",
					doneID)
			}
			queue.Stop()
			delete(blockEpochQueues, doneID)

		case h := <-blockEpochChan:
			setHeight(h)
			for _, queue := range blockEpochQueues {
				select {
				case queue.ChanIn() <- h:
				case <-mainCtx.Done():
					return mainCtx.Err()
				}
			}

		case err := <-blockErrorChan:
			return fmt.Errorf("block error: %v", err)

		case <-mainCtx.Done():
			return mainCtx.Err()
		}
	}
}

// initiateSwap delivers a new swap to the executor main loop.
func (s *executor) initiateSwap(ctx context.Context,
	swap genericSwap) {

	select {
	case s.newSwaps <- swap:
	case <-ctx.Done():
		return
	}
}

// height returns the current height known to the swap server.
func (s *executor) height() int32 {
	return int32(atomic.LoadUint32(&s.currentHeight))
}

// waitFinished waits for all swap goroutines to finish.
func (s *executor) waitFinished() {
	s.wg.Wait()
}
