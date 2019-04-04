package loop

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrSwapFeeTooHigh is returned when the swap invoice amount is too
	// high.
	ErrSwapFeeTooHigh = errors.New("swap fee too high")

	// ErrPrepayAmountTooHigh is returned when the prepay invoice amount is
	// too high.
	ErrPrepayAmountTooHigh = errors.New("prepay amount too high")

	// ErrSwapAmountTooLow is returned when the requested swap amount is
	// less than the server minimum.
	ErrSwapAmountTooLow = errors.New("swap amount too low")

	// ErrSwapAmountTooHigh is returned when the requested swap amount is
	// more than the server maximum.
	ErrSwapAmountTooHigh = errors.New("swap amount too high")

	// ErrExpiryTooSoon is returned when the server proposes an expiry that
	// is too soon for us.
	ErrExpiryTooSoon = errors.New("swap expiry too soon")

	// ErrExpiryTooFar is returned when the server proposes an expiry that
	// is too soon for us.
	ErrExpiryTooFar = errors.New("swap expiry too far")

	serverRPCTimeout = 30 * time.Second

	republishDelay = 10 * time.Second
)

// Client performs the client side part of swaps. This interface exists to be
// able to implement a stub.
type Client struct {
	started uint32 // To be used atomically.
	errChan chan error

	lndServices *lndclient.LndServices
	sweeper     *sweep.Sweeper
	executor    *executor

	resumeReady chan struct{}
	wg          sync.WaitGroup

	clientConfig
}

// NewClient returns a new instance to initiate swaps with.
func NewClient(dbDir string, serverAddress string, insecure bool,
	lnd *lndclient.LndServices) (*Client, func(), error) {

	store, err := loopdb.NewBoltSwapStore(dbDir, lnd.ChainParams)
	if err != nil {
		return nil, nil, err
	}

	swapServerClient, err := newSwapServerClient(serverAddress, insecure)
	if err != nil {
		return nil, nil, err
	}

	config := &clientConfig{
		LndServices: lnd,
		Server:      swapServerClient,
		Store:       store,
		CreateExpiryTimer: func(d time.Duration) <-chan time.Time {
			return time.NewTimer(d).C
		},
	}

	sweeper := &sweep.Sweeper{
		Lnd: lnd,
	}

	executor := newExecutor(&executorConfig{
		lnd:               lnd,
		store:             store,
		sweeper:           sweeper,
		createExpiryTimer: config.CreateExpiryTimer,
	})

	client := &Client{
		errChan:      make(chan error),
		clientConfig: *config,
		lndServices:  lnd,
		sweeper:      sweeper,
		executor:     executor,
		resumeReady:  make(chan struct{}),
	}

	cleanup := func() {
		swapServerClient.Close()
	}

	return client, cleanup, nil
}

// FetchLoopOutSwaps returns a list of all swaps currently in the database.
func (s *Client) FetchLoopOutSwaps() ([]*loopdb.LoopOut, error) {
	return s.Store.FetchLoopOutSwaps()
}

// FetchLoopInSwaps returns a list of all swaps currently in the database.
func (s *Client) FetchLoopInSwaps() ([]*loopdb.LoopIn, error) {
	return s.Store.FetchLoopInSwaps()
}

// Run is a blocking call that executes all swaps. Any pending swaps are
// restored from persistent storage and resumed.  Subsequent updates will be
// sent through the passed in statusChan. The function can be terminated by
// cancelling the context.
func (s *Client) Run(ctx context.Context,
	statusChan chan<- SwapInfo) error {

	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return errors.New("swap client can only be started once")
	}

	// Log connected node.
	info, err := s.lndServices.Client.GetInfo(ctx)
	if err != nil {
		return fmt.Errorf("GetInfo error: %v", err)
	}
	logger.Infof("Connected to lnd node %v with pubkey %v",
		info.Alias, hex.EncodeToString(info.IdentityPubkey[:]),
	)

	// Setup main context used for cancelation.
	mainCtx, mainCancel := context.WithCancel(ctx)
	defer mainCancel()

	// Query store before starting event loop to prevent new swaps from
	// being treated as swaps that need to be resumed.
	pendingLoopOutSwaps, err := s.Store.FetchLoopOutSwaps()
	if err != nil {
		return err
	}

	pendingLoopInSwaps, err := s.Store.FetchLoopInSwaps()
	if err != nil {
		return err
	}

	// Start goroutine to deliver all pending swaps to the main loop.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.resumeSwaps(mainCtx, pendingLoopOutSwaps, pendingLoopInSwaps)

		// Signal that new requests can be accepted. Otherwise the new
		// swap could already have been added to the store and read in
		// this goroutine as being a swap that needs to be resumed.
		// Resulting in two goroutines executing the same swap.
		close(s.resumeReady)
	}()

	// Main event loop.
	err = s.executor.run(mainCtx, statusChan)

	// Consider canceled as happy flow.
	if err == context.Canceled {
		err = nil
	}

	if err != nil {
		logger.Errorf("Swap client terminating: %v", err)
	} else {
		logger.Info("Swap client terminating")
	}

	// Cancel all remaining active goroutines.
	mainCancel()

	// Wait for all to finish.
	logger.Debug("Wait for executor to finish")
	s.executor.waitFinished()

	logger.Debug("Wait for goroutines to finish")
	s.wg.Wait()

	logger.Info("Swap client terminated")

	return err
}

// resumeSwaps restarts all pending swaps from the provided list.
func (s *Client) resumeSwaps(ctx context.Context,
	loopOutSwaps []*loopdb.LoopOut, loopInSwaps []*loopdb.LoopIn) {

	swapCfg := &swapConfig{
		lnd:   s.lndServices,
		store: s.Store,
	}

	for _, pend := range loopOutSwaps {
		if pend.State().Type() != loopdb.StateTypePending {
			continue
		}
		swap, err := resumeLoopOutSwap(ctx, swapCfg, pend)
		if err != nil {
			logger.Errorf("resuming loop out swap: %v", err)
			continue
		}

		s.executor.initiateSwap(ctx, swap)
	}

	for _, pend := range loopInSwaps {
		if pend.State().Type() != loopdb.StateTypePending {
			continue
		}
		swap, err := resumeLoopInSwap(ctx, swapCfg, pend)
		if err != nil {
			logger.Errorf("resuming loop in swap: %v", err)
			continue
		}

		s.executor.initiateSwap(ctx, swap)
	}
}

// LoopOut initiates a loop out swap. It blocks until the swap is initiation
// with the swap server is completed (typically this takes only a short amount
// of time). From there on further status information can be acquired through
// the status channel returned from the Run call.
//
// When the call returns, the swap has been persisted and will be resumed
// automatically after restarts.
//
// The return value is a hash that uniquely identifies the new swap.
func (s *Client) LoopOut(globalCtx context.Context,
	request *OutRequest) (*lntypes.Hash, btcutil.Address, error) {

	logger.Infof("LoopOut %v to %v (channel: %v)",
		request.Amount, request.DestAddr,
		request.LoopOutChannel,
	)

	if err := s.waitForInitialized(globalCtx); err != nil {
		return nil, nil, err
	}

	// Create a new swap object for this swap.
	initiationHeight := s.executor.height()
	swapCfg := &swapConfig{
		lnd:    s.lndServices,
		store:  s.Store,
		server: s.Server,
	}
	swap, err := newLoopOutSwap(
		globalCtx, swapCfg, initiationHeight, request,
	)
	if err != nil {
		return nil, nil, err
	}

	// Post swap to the main loop.
	s.executor.initiateSwap(globalCtx, swap)

	// Return hash so that the caller can identify this swap in the updates
	// stream.
	return &swap.hash, swap.htlc.Address, nil
}

// LoopOutQuote takes a LoopOut amount and returns a break down of estimated
// costs for the client. Both the swap server and the on-chain fee estimator
// are queried to get to build the quote response.
func (s *Client) LoopOutQuote(ctx context.Context,
	request *LoopOutQuoteRequest) (*LoopOutQuote, error) {

	terms, err := s.Server.GetLoopOutTerms(ctx)
	if err != nil {
		return nil, err
	}

	if request.Amount < terms.MinSwapAmount {
		return nil, ErrSwapAmountTooLow
	}

	if request.Amount > terms.MaxSwapAmount {
		return nil, ErrSwapAmountTooHigh
	}

	logger.Infof("Offchain swap destination: %x", terms.SwapPaymentDest)

	swapFee := swap.CalcFee(
		request.Amount, terms.SwapFeeBase, terms.SwapFeeRate,
	)

	minerFee, err := s.sweeper.GetSweepFee(
		ctx, swap.QuoteHtlc.AddSuccessToEstimator,
		request.SweepConfTarget,
	)
	if err != nil {
		return nil, err
	}

	return &LoopOutQuote{
		SwapFee:      swapFee,
		MinerFee:     minerFee,
		PrepayAmount: btcutil.Amount(terms.PrepayAmt),
	}, nil
}

// LoopOutTerms returns the terms on which the server executes swaps.
func (s *Client) LoopOutTerms(ctx context.Context) (
	*LoopOutTerms, error) {

	return s.Server.GetLoopOutTerms(ctx)
}

// waitForInitialized for swaps to be resumed and executor ready.
func (s *Client) waitForInitialized(ctx context.Context) error {
	select {
	case <-s.executor.ready:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-s.resumeReady:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// LoopIn initiates a loop in swap.
func (s *Client) LoopIn(globalCtx context.Context,
	request *LoopInRequest) (*lntypes.Hash, btcutil.Address, error) {

	logger.Infof("Loop in %v (channel: %v)",
		request.Amount,
		request.LoopInChannel,
	)

	if err := s.waitForInitialized(globalCtx); err != nil {
		return nil, nil, err
	}

	// Create a new swap object for this swap.
	initiationHeight := s.executor.height()
	swapCfg := swapConfig{
		lnd:    s.lndServices,
		store:  s.Store,
		server: s.Server,
	}
	swap, err := newLoopInSwap(
		globalCtx, &swapCfg, initiationHeight, request,
	)
	if err != nil {
		return nil, nil, err
	}

	// Post swap to the main loop.
	s.executor.initiateSwap(globalCtx, swap)

	// Return hash so that the caller can identify this swap in the updates
	// stream.
	return &swap.hash, swap.htlc.Address, nil
}

// LoopInQuote takes an amount and returns a break down of estimated
// costs for the client. Both the swap server and the on-chain fee estimator are
// queried to get to build the quote response.
func (s *Client) LoopInQuote(ctx context.Context,
	request *LoopInQuoteRequest) (*LoopInQuote, error) {

	// Retrieve current server terms to calculate swap fee.
	terms, err := s.Server.GetLoopInTerms(ctx)
	if err != nil {
		return nil, err
	}

	// Check amount limits.
	if request.Amount < terms.MinSwapAmount {
		return nil, ErrSwapAmountTooLow
	}

	if request.Amount > terms.MaxSwapAmount {
		return nil, ErrSwapAmountTooHigh
	}

	// Calculate swap fee.
	swapFee := terms.SwapFeeBase +
		request.Amount*btcutil.Amount(terms.SwapFeeRate)/
			btcutil.Amount(swap.FeeRateTotalParts)

	// Get estimate for miner fee.
	minerFee, err := s.lndServices.Client.EstimateFeeToP2WSH(
		ctx, request.Amount, request.HtlcConfTarget,
	)
	if err != nil {
		return nil, err
	}

	return &LoopInQuote{
		SwapFee:  swapFee,
		MinerFee: minerFee,
	}, nil
}

// LoopInTerms returns the terms on which the server executes swaps.
func (s *Client) LoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	return s.Server.GetLoopInTerms(ctx)
}
