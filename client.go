package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
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

	// ErrExpiryTooFar is returned when the server proposes an expiry that
	// is too soon for us.
	ErrExpiryTooFar = errors.New("swap expiry too far")

	// serverRPCTimeout is the maximum time a gRPC request to the server
	// should be allowed to take.
	serverRPCTimeout = 30 * time.Second

	// globalCallTimeout is the maximum time any call of the client to the
	// server is allowed to take, including the time it may take to get
	// and pay for an LSAT token.
	globalCallTimeout = serverRPCTimeout + lsat.PaymentTimeout

	republishDelay = 10 * time.Second

	// MinerFeeEstimationFailed is a magic number that is returned in a
	// quote call as the miner fee if the fee estimation in lnd's wallet
	// failed because of insufficient funds.
	MinerFeeEstimationFailed btcutil.Amount = -1
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

// ClientConfig is the exported configuration structure that is required to
// instantiate the loop client.
type ClientConfig struct {
	// ServerAddress is the loop server to connect to.
	ServerAddress string

	// ProxyAddress is the SOCKS proxy that should be used to establish the
	// connection.
	ProxyAddress string

	// SwapServerNoTLS skips TLS for the swap server connection when set.
	SwapServerNoTLS bool

	// TLSPathServer is the path to the TLS certificate that is required to
	// connect to the server.
	TLSPathServer string

	// Lnd is an instance of the lnd proxy.
	Lnd *lndclient.LndServices

	// MaxLsatCost is the maximum price we are willing to pay to the server
	// for the token.
	MaxLsatCost btcutil.Amount

	// MaxLsatFee is the maximum that we are willing to pay in routing fees
	// to obtain the token.
	MaxLsatFee btcutil.Amount

	// LoopOutMaxParts defines the maximum number of parts that may be used
	// for a loop out swap. When greater than one, a multi-part payment may
	// be attempted.
	LoopOutMaxParts uint32
}

// NewClient returns a new instance to initiate swaps with.
func NewClient(dbDir string, cfg *ClientConfig) (*Client, func(), error) {
	store, err := loopdb.NewBoltSwapStore(dbDir, cfg.Lnd.ChainParams)
	if err != nil {
		return nil, nil, err
	}
	lsatStore, err := lsat.NewFileStore(dbDir)
	if err != nil {
		return nil, nil, err
	}

	swapServerClient, err := newSwapServerClient(cfg, lsatStore)
	if err != nil {
		return nil, nil, err
	}

	config := &clientConfig{
		LndServices: cfg.Lnd,
		Server:      swapServerClient,
		Store:       store,
		LsatStore:   lsatStore,
		CreateExpiryTimer: func(d time.Duration) <-chan time.Time {
			return time.NewTimer(d).C
		},
	}

	sweeper := &sweep.Sweeper{
		Lnd: cfg.Lnd,
	}

	executor := newExecutor(&executorConfig{
		lnd:               cfg.Lnd,
		store:             store,
		sweeper:           sweeper,
		createExpiryTimer: config.CreateExpiryTimer,
		loopOutMaxParts:   cfg.LoopOutMaxParts,
	})

	client := &Client{
		errChan:      make(chan error),
		clientConfig: *config,
		lndServices:  cfg.Lnd,
		sweeper:      sweeper,
		executor:     executor,
		resumeReady:  make(chan struct{}),
	}

	cleanup := func() {
		swapServerClient.stop()
	}

	return client, cleanup, nil
}

// FetchSwaps returns all loop in and out swaps currently in the database.
func (s *Client) FetchSwaps() ([]*SwapInfo, error) {
	loopOutSwaps, err := s.Store.FetchLoopOutSwaps()
	if err != nil {
		return nil, err
	}

	loopInSwaps, err := s.Store.FetchLoopInSwaps()
	if err != nil {
		return nil, err
	}

	swaps := make([]*SwapInfo, 0, len(loopInSwaps)+len(loopOutSwaps))

	for _, swp := range loopOutSwaps {
		htlc, err := swap.NewHtlc(
			swap.HtlcV1, swp.Contract.CltvExpiry,
			swp.Contract.SenderKey, swp.Contract.ReceiverKey,
			swp.Hash, swap.HtlcP2WSH, s.lndServices.ChainParams,
		)
		if err != nil {
			return nil, err
		}

		swaps = append(swaps, &SwapInfo{
			SwapType:         swap.TypeOut,
			SwapContract:     swp.Contract.SwapContract,
			SwapStateData:    swp.State(),
			SwapHash:         swp.Hash,
			LastUpdate:       swp.LastUpdateTime(),
			HtlcAddressP2WSH: htlc.Address,
		})
	}

	for _, swp := range loopInSwaps {
		htlcNP2WSH, err := swap.NewHtlc(
			swap.HtlcV1, swp.Contract.CltvExpiry,
			swp.Contract.SenderKey, swp.Contract.ReceiverKey,
			swp.Hash, swap.HtlcNP2WSH, s.lndServices.ChainParams,
		)
		if err != nil {
			return nil, err
		}

		htlcP2WSH, err := swap.NewHtlc(
			swap.HtlcV1, swp.Contract.CltvExpiry,
			swp.Contract.SenderKey, swp.Contract.ReceiverKey,
			swp.Hash, swap.HtlcP2WSH, s.lndServices.ChainParams,
		)
		if err != nil {
			return nil, err
		}

		swaps = append(swaps, &SwapInfo{
			SwapType:          swap.TypeIn,
			SwapContract:      swp.Contract.SwapContract,
			SwapStateData:     swp.State(),
			SwapHash:          swp.Hash,
			LastUpdate:        swp.LastUpdateTime(),
			HtlcAddressP2WSH:  htlcP2WSH.Address,
			HtlcAddressNP2WSH: htlcNP2WSH.Address,
		})
	}

	return swaps, nil
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
	log.Infof("Connected to lnd node '%v' with pubkey %x (version %s)",
		s.lndServices.NodeAlias, s.lndServices.NodePubkey,
		lndclient.VersionString(s.lndServices.Version))

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
		log.Errorf("Swap client terminating: %v", err)
	} else {
		log.Info("Swap client terminating")
	}

	// Cancel all remaining active goroutines.
	mainCancel()

	// Wait for all to finish.
	log.Debug("Wait for executor to finish")
	s.executor.waitFinished()

	log.Debug("Wait for goroutines to finish")
	s.wg.Wait()

	log.Info("Swap client terminated")

	return err
}

// resumeSwaps restarts all pending swaps from the provided list.
func (s *Client) resumeSwaps(ctx context.Context,
	loopOutSwaps []*loopdb.LoopOut, loopInSwaps []*loopdb.LoopIn) {

	swapCfg := newSwapConfig(s.lndServices, s.Store, s.Server)

	for _, pend := range loopOutSwaps {
		if pend.State().State.Type() != loopdb.StateTypePending {
			continue
		}
		swap, err := resumeLoopOutSwap(ctx, swapCfg, pend)
		if err != nil {
			log.Errorf("resuming loop out swap: %v", err)
			continue
		}

		s.executor.initiateSwap(ctx, swap)
	}

	for _, pend := range loopInSwaps {
		if pend.State().State.Type() != loopdb.StateTypePending {
			continue
		}
		swap, err := resumeLoopInSwap(ctx, swapCfg, pend)
		if err != nil {
			log.Errorf("resuming loop in swap: %v", err)
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
	request *OutRequest) (*LoopOutSwapInfo, error) {

	log.Infof("LoopOut %v to %v (channels: %v)",
		request.Amount, request.DestAddr, request.OutgoingChanSet,
	)

	if err := s.waitForInitialized(globalCtx); err != nil {
		return nil, err
	}

	// Calculate htlc expiry height.
	terms, err := s.Server.GetLoopOutTerms(globalCtx)
	if err != nil {
		return nil, err
	}

	initiationHeight := s.executor.height()
	request.Expiry, err = s.getExpiry(
		initiationHeight, terms, request.SweepConfTarget,
	)
	if err != nil {
		return nil, err
	}

	// Create a new swap object for this swap.
	swapCfg := newSwapConfig(s.lndServices, s.Store, s.Server)
	initResult, err := newLoopOutSwap(
		globalCtx, swapCfg, initiationHeight, request,
	)
	if err != nil {
		return nil, err
	}
	swap := initResult.swap

	// Post swap to the main loop.
	s.executor.initiateSwap(globalCtx, swap)

	// Return hash so that the caller can identify this swap in the updates
	// stream.
	return &LoopOutSwapInfo{
		SwapHash:         swap.hash,
		HtlcAddressP2WSH: swap.htlc.Address,
		ServerMessage:    initResult.serverMessage,
	}, nil
}

// getExpiry returns an absolute expiry height based on the sweep confirmation
// target, constrained by the server terms.
func (s *Client) getExpiry(height int32, terms *LoopOutTerms,
	confTarget int32) (int32, error) {

	switch {
	case confTarget < terms.MinCltvDelta:
		return height + terms.MinCltvDelta, nil

	case confTarget > terms.MaxCltvDelta:
		return 0, fmt.Errorf("confirmation target %v exceeds maximum "+
			"server cltv delta of %v", confTarget,
			terms.MaxCltvDelta)
	}

	return height + confTarget, nil
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

	height := s.executor.height()
	expiry, err := s.getExpiry(height, terms, request.SweepConfTarget)
	if err != nil {
		return nil, err
	}

	quote, err := s.Server.GetLoopOutQuote(
		ctx, request.Amount, expiry, request.SwapPublicationDeadline,
	)
	if err != nil {
		return nil, err
	}

	log.Infof("Offchain swap destination: %x", quote.SwapPaymentDest)

	swapFee := quote.SwapFee

	// Generate dummy p2wsh address for fee estimation. The p2wsh address
	// type is chosen because it adds the most weight of all output types
	// and we want the quote to return a worst case value.
	wsh := [32]byte{}
	p2wshAddress, err := btcutil.NewAddressWitnessScriptHash(
		wsh[:], s.lndServices.ChainParams,
	)
	if err != nil {
		return nil, err
	}

	minerFee, err := s.sweeper.GetSweepFee(
		ctx, swap.QuoteHtlc.AddSuccessToEstimator,
		p2wshAddress, request.SweepConfTarget,
	)
	if err != nil {
		return nil, err
	}

	return &LoopOutQuote{
		SwapFee:         swapFee,
		MinerFee:        minerFee,
		PrepayAmount:    quote.PrepayAmount,
		SwapPaymentDest: quote.SwapPaymentDest,
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
	request *LoopInRequest) (*LoopInSwapInfo, error) {

	log.Infof("Loop in %v (last hop: %v)",
		request.Amount,
		request.LastHop,
	)

	if err := s.waitForInitialized(globalCtx); err != nil {
		return nil, err
	}

	// Create a new swap object for this swap.
	initiationHeight := s.executor.height()
	swapCfg := newSwapConfig(s.lndServices, s.Store, s.Server)
	initResult, err := newLoopInSwap(
		globalCtx, swapCfg, initiationHeight, request,
	)
	if err != nil {
		return nil, err
	}
	swap := initResult.swap

	// Post swap to the main loop.
	s.executor.initiateSwap(globalCtx, swap)

	// Return hash so that the caller can identify this swap in the updates
	// stream.
	swapInfo := &LoopInSwapInfo{
		SwapHash:          swap.hash,
		HtlcAddressP2WSH:  swap.htlcP2WSH.Address,
		HtlcAddressNP2WSH: swap.htlcNP2WSH.Address,
		ServerMessage:     initResult.serverMessage,
	}
	return swapInfo, nil
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

	quote, err := s.Server.GetLoopInQuote(ctx, request.Amount)
	if err != nil {
		return nil, err
	}

	swapFee := quote.SwapFee

	// We don't calculate the on-chain fee if the HTLC is going to be
	// published externally.
	if request.ExternalHtlc {
		return &LoopInQuote{
			SwapFee:  swapFee,
			MinerFee: 0,
		}, nil
	}

	// Get estimate for miner fee. If estimating the miner fee for the
	// requested amount is not possible because lnd's wallet cannot
	// construct a sample TX, we just return zero instead of failing the
	// quote. The user interface should inform the user that fee estimation
	// was not possible.
	//
	// TODO(guggero): Thread through error code from lnd to avoid string
	// matching.
	minerFee, err := s.lndServices.Client.EstimateFeeToP2WSH(
		ctx, request.Amount, request.HtlcConfTarget,
	)
	if err != nil && strings.Contains(err.Error(), "insufficient funds") {
		return &LoopInQuote{
			SwapFee:   swapFee,
			MinerFee:  MinerFeeEstimationFailed,
			CltvDelta: quote.CltvDelta,
		}, nil
	}
	if err != nil {
		return nil, err
	}

	return &LoopInQuote{
		SwapFee:   swapFee,
		MinerFee:  minerFee,
		CltvDelta: quote.CltvDelta,
	}, nil
}

// LoopInTerms returns the terms on which the server executes swaps.
func (s *Client) LoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	return s.Server.GetLoopInTerms(ctx)
}
