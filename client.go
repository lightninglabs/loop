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
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/sweepbatcher"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
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
	// is too far in the future.
	ErrExpiryTooFar = errors.New("swap expiry too far")

	// ErrExpiryTooSoon is returned when the server proposes an expiry that
	// is too soon.
	ErrExpiryTooSoon = errors.New("swap expiry too soon")

	// ErrInsufficientBalance indicates insufficient confirmed balance to
	// publish a swap.
	ErrInsufficientBalance = errors.New("insufficient confirmed balance")

	// serverRPCTimeout is the maximum time a gRPC request to the server
	// should be allowed to take.
	serverRPCTimeout = 30 * time.Second

	// globalCallTimeout is the maximum time any call of the client to the
	// server is allowed to take, including the time it may take to get
	// and pay for an L402 token.
	globalCallTimeout = serverRPCTimeout + l402.PaymentTimeout

	// probeTimeout is the maximum time until a probe is allowed to take.
	probeTimeout = 3 * time.Minute

	// repushDelay is the delay of (re)adding a sweep to sweepbatcher after
	// a block is mined.
	repushDelay = 1 * time.Second

	// additionalDelay is the delay added on top of repushDelay inside the
	// sweepbatcher to publish a sweep transaction.
	additionalDelay = 1 * time.Second

	// MinerFeeEstimationFailed is a magic number that is returned in a
	// quote call as the miner fee if the fee estimation in lnd's wallet
	// failed because of insufficient funds.
	MinerFeeEstimationFailed btcutil.Amount = -1

	// defaultRFQExpiry is the default expiry time for RFQs.
	defaultRFQExpiry = 5 * time.Minute

	// defaultRFQMaxLimitMultiplier is the default maximum fee multiplier for
	// RFQs.
	defaultRFQMaxLimitMultiplier = 1.2
)

// Client performs the client side part of swaps. This interface exists to be
// able to implement a stub.
type Client struct {
	started uint32 // To be used atomically.
	errChan chan error

	// abandonChans allows for accessing a swap's abandon channel by
	// providing its swap hash. This map is used to look up the abandon
	// channel of a swap if the client requests to abandon it.
	abandonChans map[lntypes.Hash]chan struct{}

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

	// AssetClient is an instance of the assets client.
	AssetClient *assets.TapdClient

	// MaxL402Cost is the maximum price we are willing to pay to the server
	// for the token.
	MaxL402Cost btcutil.Amount

	// MaxL402Fee is the maximum that we are willing to pay in routing fees
	// to obtain the token.
	MaxL402Fee btcutil.Amount

	// LoopOutMaxParts defines the maximum number of parts that may be used
	// for a loop out swap. When greater than one, a multi-part payment may
	// be attempted.
	LoopOutMaxParts uint32

	// SkippedTxns is the list of existing HTLC txids to skip when starting
	// Loop. This should only be used if affected by the historical bug.
	SkippedTxns []string

	// TotalPaymentTimeout is the total amount of time until we time out
	// off-chain payments (used in loop out).
	TotalPaymentTimeout time.Duration

	// MaxPaymentRetries is the maximum times we retry an off-chain payment
	// (used in loop out).
	MaxPaymentRetries int

	// MaxStaticAddrHtlcFeePercentage is the percentage of the swap amount
	// that we allow the server to charge for the htlc transaction.
	// Although highly unlikely, this is a defense against the server
	// publishing the htlc without paying the swap invoice, forcing us to
	// sweep the timeout path.
	MaxStaticAddrHtlcFeePercentage float64

	// MaxStaticAddrHtlcBackupFeePercentage is the percentage of the swap
	// amount that we allow the server to charge for the htlc backup
	// transactions. This is a defense against the server publishing the
	// htlc backup without paying the swap invoice, forcing us to sweep the
	// timeout path. This value is elevated compared to
	// MaxStaticAddrHtlcFeePercentage since it serves the server as backup
	// transaction in case of fee spikes.
	MaxStaticAddrHtlcBackupFeePercentage float64
}

// NewClient returns a new instance to initiate swaps with.
func NewClient(dbDir string, loopDB loopdb.SwapStore,
	sweeperDb sweepbatcher.BatcherStore, cfg *ClientConfig) (
	*Client, func(), error) {

	l402Store, err := l402.NewFileStore(dbDir)
	if err != nil {
		return nil, nil, err
	}

	swapServerClient, err := newSwapServerClient(cfg, l402Store)
	if err != nil {
		return nil, nil, err
	}

	config := &clientConfig{
		LndServices: cfg.Lnd,
		Server:      swapServerClient,
		Store:       loopDB,
		Conn:        swapServerClient.conn,
		L402Store:   l402Store,
		CreateExpiryTimer: func(d time.Duration) <-chan time.Time {
			return time.NewTimer(d).C
		},
		AssetClient:     cfg.AssetClient,
		LoopOutMaxParts: cfg.LoopOutMaxParts,
	}

	sweeper := &sweep.Sweeper{
		Lnd: cfg.Lnd,
	}

	verifySchnorrSig := func(pubKey *btcec.PublicKey, hash, sig []byte) error {
		schnorrSig, err := schnorr.ParseSignature(sig)
		if err != nil {
			return err
		}

		if !schnorrSig.Verify(hash, pubKey) {
			return fmt.Errorf("invalid signature")
		}

		return nil
	}

	sweepStore, err := sweepbatcher.NewSweepFetcherFromSwapStore(
		loopDB, cfg.Lnd.ChainParams,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("sweepbatcher."+
			"NewSweepFetcherFromSwapStore failed: %w", err)
	}

	// There is circular dependency between executor and sweepbatcher, as
	// executor stores sweepbatcher and sweepbatcher depends on
	// executor.height() though loopOutSweepFeerateProvider.
	var executor *executor

	// getHeight returns current height, according to executor.
	getHeight := func() int32 {
		if executor == nil {
			// This must not happen, because executor is set in this
			// function, before sweepbatcher.Run is called.
			log.Errorf("getHeight called when executor is nil")

			return 0
		}

		return executor.height()
	}

	loopOutSweepFeerateProvider := newLoopOutSweepFeerateProvider(
		sweeper, loopDB, cfg.Lnd.ChainParams, getHeight,
	)

	batcherOpts := []sweepbatcher.BatcherOption{
		// Disable 100 sats/kw fee bump every block and retarget feerate
		// every block according to the current mempool condition.
		sweepbatcher.WithCustomFeeRate(
			loopOutSweepFeerateProvider.GetMinFeeRate,
		),

		// Upon new block arrival, republishing is triggered in both
		// loopout.go code (waitForHtlcSpendConfirmedV2/ <-timerChan)
		// and in sweepbatcher code (batch.Run/case <-timerChan). The
		// former updates the fee rate which is used by the later by
		// calling AddSweep. Make sure they are ordered, add additional
		// delay time to sweepbatcher's handling. The delay used in
		// loopout.go is repushDelay.
		sweepbatcher.WithPublishDelay(
			repushDelay + additionalDelay,
		),
	}

	if len(cfg.SkippedTxns) != 0 {
		skippedTxns := make(map[chainhash.Hash]struct{})
		for _, txid := range cfg.SkippedTxns {
			txid, err := chainhash.NewHashFromStr(txid)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to parse "+
					"txid to skip %v: %w", txid, err)
			}
			skippedTxns[*txid] = struct{}{}
		}
		batcherOpts = append(batcherOpts, sweepbatcher.WithSkippedTxns(
			skippedTxns,
		))
	}

	batcher := sweepbatcher.NewBatcher(
		cfg.Lnd.WalletKit, cfg.Lnd.ChainNotifier, cfg.Lnd.Signer,
		swapServerClient.MultiMuSig2SignSweep, verifySchnorrSig,
		cfg.Lnd.ChainParams, sweeperDb, sweepStore, batcherOpts...,
	)

	executor = newExecutor(&executorConfig{
		lnd:                 cfg.Lnd,
		store:               loopDB,
		sweeper:             sweeper,
		batcher:             batcher,
		createExpiryTimer:   config.CreateExpiryTimer,
		loopOutMaxParts:     cfg.LoopOutMaxParts,
		totalPaymentTimeout: cfg.TotalPaymentTimeout,
		maxPaymentRetries:   cfg.MaxPaymentRetries,
		cancelSwap:          swapServerClient.CancelLoopOutSwap,
		verifySchnorrSig:    verifySchnorrSig,
	})

	client := &Client{
		errChan:      make(chan error),
		clientConfig: *config,
		lndServices:  cfg.Lnd,
		sweeper:      sweeper,
		executor:     executor,
		resumeReady:  make(chan struct{}),
		abandonChans: make(map[lntypes.Hash]chan struct{}),
	}

	cleanup := func() {
		swapServerClient.stop()
		loopDB.Close()
	}

	return client, cleanup, nil
}

// GetConn returns the gRPC connection to the server.
func (s *Client) GetConn() *grpc.ClientConn {
	return s.clientConfig.Conn
}

// FetchSwaps returns all loop in and out swaps currently in the database.
func (s *Client) FetchSwaps(ctx context.Context) ([]*SwapInfo, error) {
	loopOutSwaps, err := s.Store.FetchLoopOutSwaps(ctx)
	if err != nil {
		return nil, err
	}

	loopInSwaps, err := s.Store.FetchLoopInSwaps(ctx)
	if err != nil {
		return nil, err
	}

	swaps := make([]*SwapInfo, 0, len(loopInSwaps)+len(loopOutSwaps))

	for _, swp := range loopOutSwaps {
		swapInfo := &SwapInfo{
			SwapType:      swap.TypeOut,
			SwapContract:  swp.Contract.SwapContract,
			SwapStateData: swp.State(),
			SwapHash:      swp.Hash,
			LastUpdate:    swp.LastUpdateTime(),
		}

		htlc, err := utils.GetHtlc(
			swp.Hash, &swp.Contract.SwapContract,
			s.lndServices.ChainParams,
		)
		if err != nil {
			return nil, err
		}

		switch htlc.OutputType {
		case swap.HtlcP2WSH:
			swapInfo.HtlcAddressP2WSH = htlc.Address

		case swap.HtlcP2TR:
			swapInfo.HtlcAddressP2TR = htlc.Address

		default:
			return nil, swap.ErrInvalidOutputType
		}

		if swp.Contract.AssetSwapInfo != nil {
			swapInfo.AssetSwapInfo = swp.Contract.AssetSwapInfo
		}

		swaps = append(swaps, swapInfo)
	}

	for _, swp := range loopInSwaps {
		swapInfo := &SwapInfo{
			SwapType:      swap.TypeIn,
			SwapContract:  swp.Contract.SwapContract,
			SwapStateData: swp.State(),
			SwapHash:      swp.Hash,
			LastUpdate:    swp.LastUpdateTime(),
			LastHop:       swp.Contract.LastHop,
		}

		htlc, err := utils.GetHtlc(
			swp.Hash, &swp.Contract.SwapContract,
			s.lndServices.ChainParams,
		)
		if err != nil {
			return nil, err
		}

		switch htlc.OutputType {
		case swap.HtlcP2WSH:
			swapInfo.HtlcAddressP2WSH = htlc.Address

		case swap.HtlcP2TR:
			swapInfo.HtlcAddressP2TR = htlc.Address

		default:
			return nil, swap.ErrInvalidOutputType
		}

		swaps = append(swaps, swapInfo)
	}

	return swaps, nil
}

// Run is a blocking call that executes all swaps. Any pending swaps are
// restored from persistent storage and resumed.  Subsequent updates will be
// sent through the passed in statusChan. The function can be terminated by
// cancelling the context.
func (s *Client) Run(ctx context.Context, statusChan chan<- SwapInfo) error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return errors.New("swap client can only be started once")
	}

	// Log connected node.
	log.Infof("Connected to lnd node '%v' with pubkey %s (version %s)",
		s.lndServices.NodeAlias, s.lndServices.NodePubkey,
		lndclient.VersionString(s.lndServices.Version))

	// Setup main context used for cancellation.
	mainCtx, mainCancel := context.WithCancel(ctx)
	defer mainCancel()

	// Query store before starting event loop to prevent new swaps from
	// being treated as swaps that need to be resumed.
	pendingLoopOutSwaps, err := s.Store.FetchLoopOutSwaps(mainCtx)
	if err != nil {
		return err
	}

	pendingLoopInSwaps, err := s.Store.FetchLoopInSwaps(mainCtx)
	if err != nil {
		return err
	}

	// Start goroutine to deliver all pending swaps to the main loop.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.resumeSwaps(mainCtx, pendingLoopOutSwaps, pendingLoopInSwaps)

		// Signal that new requests can be accepted. Otherwise, the new
		// swap could already have been added to the store and read in
		// this goroutine as being a swap that needs to be resumed.
		// Resulting in two goroutines executing the same swap.
		close(s.resumeReady)
	}()

	// Main event loop.
	err = s.executor.run(mainCtx, statusChan, s.abandonChans)

	// Consider canceled as happy flow.
	if errors.Is(err, context.Canceled) {
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

	swapCfg := newSwapConfig(s.lndServices, s.Store, s.Server, s.AssetClient)

	for _, pend := range loopOutSwaps {
		if pend.State().State.Type() != loopdb.StateTypePending {
			continue
		}
		swap, err := resumeLoopOutSwap(swapCfg, pend)
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

		// Store the swap's abandon channel so that the client can
		// abandon the swap by providing the swap hash.
		s.executor.Lock()
		s.abandonChans[swap.hash] = swap.abandonChan
		s.executor.Unlock()

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

	if request.AssetId != nil {
		if request.AssetPrepayRfqId == nil ||
			request.AssetSwapRfqId == nil {

			return nil, errors.New("asset prepay and swap rfq ids " +
				"must be set when using an asset id")
		}

		// Verify that if we have an asset id set, we have a valid asset
		// client to use.
		if s.AssetClient == nil {
			return nil, errors.New("asset client must be set " +
				"when using an asset id")
		}

		log.Infof("LoopOut %v sats to %v with asset %x",
			request.Amount, request.DestAddr, request.AssetId,
		)
	} else {
		log.Infof("LoopOut %v to %v (channels: %v)",
			request.Amount, request.DestAddr,
			request.OutgoingChanSet,
		)
	}

	if err := s.waitForInitialized(globalCtx); err != nil {
		return nil, err
	}

	// Calculate htlc expiry height.
	terms, err := s.Server.GetLoopOutTerms(globalCtx, request.Initiator)
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
	swapCfg := newSwapConfig(
		s.lndServices, s.Store, s.Server, s.AssetClient,
	)

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
		SwapHash:      swap.hash,
		HtlcAddress:   swap.htlc.Address,
		ServerMessage: initResult.serverMessage,
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

	if request.AssetRFQRequest != nil {
		rfqReq := request.AssetRFQRequest
		if rfqReq.AssetId == nil || rfqReq.AssetEdgeNode == nil {
			return nil, errors.New("both asset edge node and " +
				"asset id must be set")
		}
	}

	terms, err := s.Server.GetLoopOutTerms(ctx, request.Initiator)
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
		request.Initiator,
	)
	if err != nil {
		return nil, err
	}

	log.Infof("Offchain swap destination: %x", quote.SwapPaymentDest)

	minerFee, err := s.getLoopOutSweepFee(ctx, request.SweepConfTarget)
	if err != nil {
		return nil, err
	}

	loopOutQuote := &LoopOutQuote{
		SwapFee:         quote.SwapFee,
		MinerFee:        minerFee,
		PrepayAmount:    quote.PrepayAmount,
		SwapPaymentDest: quote.SwapPaymentDest,
	}

	// If we use an Asset we'll rfq to get the asset amounts to use for
	// the swap.
	if request.AssetRFQRequest != nil {
		rfq, err := s.getAssetRfq(ctx, loopOutQuote, request)
		if err != nil {
			return nil, err
		}

		loopOutQuote.LoopOutRfq = rfq
	}

	return loopOutQuote, nil
}

// getLoopOutSweepFee is a helper method to estimate the loop out htlc sweep
// fee to a p2wsh address.
func (s *Client) getLoopOutSweepFee(ctx context.Context, confTarget int32) (
	btcutil.Amount, error) {

	// Generate dummy p2wsh address for fee estimation. The p2wsh address
	// type is chosen because it adds the most weight of all output types
	// and we want the quote to return a worst case value.
	wsh := [32]byte{}
	p2wshAddress, err := btcutil.NewAddressWitnessScriptHash(
		wsh[:], s.lndServices.ChainParams,
	)
	if err != nil {
		return 0, err
	}

	scriptVersion := utils.GetHtlcScriptVersion(
		loopdb.CurrentProtocolVersion(),
	)

	htlc := swap.QuoteHtlcP2TR
	if scriptVersion != swap.HtlcV3 {
		htlc = swap.QuoteHtlcP2WSH
	}

	label := "loopout-quote"

	return s.sweeper.GetSweepFee(
		ctx, htlc.AddSuccessToEstimator, p2wshAddress, confTarget,
		label,
	)
}

// LoopOutTerms returns the terms on which the server executes swaps.
func (s *Client) LoopOutTerms(ctx context.Context, initiator string) (
	*LoopOutTerms, error) {

	return s.Server.GetLoopOutTerms(ctx, initiator)
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
	swapCfg := newSwapConfig(s.lndServices, s.Store, s.Server, s.AssetClient)
	initResult, err := newLoopInSwap(
		globalCtx, swapCfg, initiationHeight, request,
	)
	if err != nil {
		return nil, err
	}
	swap := initResult.swap

	s.executor.Lock()
	s.abandonChans[swap.hash] = swap.abandonChan
	s.executor.Unlock()

	// Post swap to the main loop.
	s.executor.initiateSwap(globalCtx, swap)

	// Return hash so that the caller can identify this swap in the updates
	// stream.
	swapInfo := &LoopInSwapInfo{
		SwapHash:      swap.hash,
		ServerMessage: initResult.serverMessage,
	}

	if loopdb.CurrentProtocolVersion() < loopdb.ProtocolVersionHtlcV3 {
		swapInfo.HtlcAddressP2WSH = swap.htlcP2WSH.Address
	} else {
		swapInfo.HtlcAddressP2TR = swap.htlcP2TR.Address
	}

	return swapInfo, nil
}

// LoopInQuote takes an amount and returns a breakdown of estimated costs for
// the client. Both the swap server and the on-chain fee estimator are queried
// to get to build the quote response.
func (s *Client) LoopInQuote(ctx context.Context,
	request *LoopInQuoteRequest) (*LoopInQuote, error) {

	// Retrieve current server terms to calculate swap fee.
	terms, err := s.Server.GetLoopInTerms(ctx, request.Initiator)
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

	// Private and routehints are mutually exclusive as setting private
	// means we retrieve our own routehints from the connected node.
	if len(request.RouteHints) != 0 && request.Private {
		return nil, fmt.Errorf("private and route_hints both set")
	}

	if request.Private {
		// If last_hop is set, we'll only add channels with peers
		// set to the last_hop parameter
		includeNodes := make(map[route.Vertex]struct{})
		if request.LastHop != nil {
			includeNodes[*request.LastHop] = struct{}{}
		}

		// Because the Private flag is set, we'll generate our own
		// set of hop hints and use that
		request.RouteHints, err = SelectHopHints(
			ctx, s.lndServices.Client, request.Amount,
			DefaultMaxHopHints, includeNodes,
		)
		if err != nil {
			return nil, err
		}
	}

	quote, err := s.Server.GetLoopInQuote(
		ctx, request.Amount, s.lndServices.NodePubkey, request.LastHop,
		request.RouteHints, request.Initiator, request.NumDeposits,
	)
	if err != nil {
		return nil, err
	}

	swapFee := quote.SwapFee

	// We don't calculate the on-chain fee if the HTLC is going to be
	// published externally.
	// We also don't calculate the on-chain fee if the loop in is funded by
	// static address deposits because we don't publish the HTLC on-chain.
	if request.ExternalHtlc || request.NumDeposits > 0 {
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
	minerFee, err := s.estimateFee(
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

// estimateFee is a helper method to estimate the total fee for paying the
// passed amount with the given conf target. It'll assume taproot destination
// if the protocol version indicates that we're using taproot htlcs.
func (s *Client) estimateFee(ctx context.Context, amt btcutil.Amount,
	confTarget int32) (btcutil.Amount, error) {

	var (
		address btcutil.Address
		err     error
	)
	// Generate a dummy address for fee estimation.
	witnessProg := [32]byte{}

	scriptVersion := utils.GetHtlcScriptVersion(
		loopdb.CurrentProtocolVersion(),
	)

	if scriptVersion != swap.HtlcV3 {
		address, err = btcutil.NewAddressWitnessScriptHash(
			witnessProg[:], s.lndServices.ChainParams,
		)
	} else {
		address, err = btcutil.NewAddressTaproot(
			witnessProg[:], s.lndServices.ChainParams,
		)
	}
	if err != nil {
		return 0, err
	}

	return s.lndServices.Client.EstimateFee(ctx, address, amt, confTarget)
}

// LoopInTerms returns the terms on which the server executes swaps.
func (s *Client) LoopInTerms(ctx context.Context, initiator string) (
	*LoopInTerms, error) {

	return s.Server.GetLoopInTerms(ctx, initiator)
}

// wrapGrpcError wraps the non-nil error provided with a message providing
// additional context, preserving the grpc code returned with the original
// error. If the original error has no grpc code, then codes.Unknown is used.
func wrapGrpcError(message string, err error) error {
	// Since our error is non-nil, we don't need to worry about a nil
	// grpcStatus, we'll just get an unknown one if no code was passed back.
	grpcStatus, _ := status.FromError(err)

	return status.Error(
		grpcStatus.Code(), fmt.Sprintf("%v: %v", message,
			grpcStatus.Message()),
	)
}

// Probe asks the server to probe a route to us given a requested amount and
// last hop. The server is free to discard frequent request to avoid abuse or if
// there's been a recent probe to us for the same amount.
func (s *Client) Probe(ctx context.Context, req *ProbeRequest) error {
	return s.Server.Probe(
		ctx, req.Amount, s.lndServices.NodePubkey, req.LastHop,
		req.RouteHints,
	)
}

// AbandonSwap sends a signal on the abandon channel of the swap identified by
// the passed swap hash. This will cause the swap to abandon itself.
func (s *Client) AbandonSwap(ctx context.Context,
	req *AbandonSwapRequest) error {

	if req == nil {
		return errors.New("no request provided")
	}

	s.executor.Lock()
	defer s.executor.Unlock()

	select {
	case s.abandonChans[req.SwapHash] <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	default:
		// This is to avoid writing to a full channel.
	}

	return nil
}

// getAssetRfq returns a prepay and swap rfq for the asset swap.
func (s *Client) getAssetRfq(ctx context.Context, quote *LoopOutQuote,
	request *LoopOutQuoteRequest) (*LoopOutRfq, error) {

	if s.AssetClient == nil {
		return nil, errors.New("asset client must be set " +
			"when trying to loop out with an asset")
	}
	rfqReq := request.AssetRFQRequest
	if rfqReq.Expiry == 0 {
		rfqReq.Expiry = time.Now().Add(defaultRFQExpiry).Unix()
	}

	if rfqReq.MaxLimitMultiplier == 0 {
		rfqReq.MaxLimitMultiplier = defaultRFQMaxLimitMultiplier
	}

	// First we'll get the prepay rfq.
	prepayRfq, err := s.AssetClient.GetRfqForAsset(
		ctx, quote.PrepayAmount, rfqReq.AssetId,
		rfqReq.AssetEdgeNode, rfqReq.Expiry,
		rfqReq.MaxLimitMultiplier,
	)
	if err != nil {
		return nil, err
	}

	prepayAssetRate, err := rpcutils.UnmarshalRfqFixedPoint(
		prepayRfq.BidAssetRate,
	)
	if err != nil {
		return nil, err
	}

	// The actual invoice swap amount is the requested amount plus
	// the swap fee minus the prepay amount.
	invoiceAmt := request.Amount + quote.SwapFee -
		quote.PrepayAmount

	swapRfq, err := s.AssetClient.GetRfqForAsset(
		ctx, invoiceAmt, rfqReq.AssetId,
		rfqReq.AssetEdgeNode, rfqReq.Expiry,
		rfqReq.MaxLimitMultiplier,
	)
	if err != nil {
		return nil, err
	}

	swapAssetRate, err := rpcutils.UnmarshalRfqFixedPoint(
		swapRfq.BidAssetRate,
	)
	if err != nil {
		return nil, err
	}

	// We'll also want the asset name to verify for the client.
	assetName, err := s.AssetClient.GetAssetName(
		ctx, rfqReq.AssetId,
	)
	if err != nil {
		return nil, err
	}

	return &LoopOutRfq{
		PrepayRfqId:       prepayRfq.Id,
		MaxPrepayAssetAmt: prepayRfq.AssetAmount,
		PrepayAssetRate:   prepayAssetRate,
		SwapRfqId:         swapRfq.Id,
		MaxSwapAssetAmt:   swapRfq.AssetAmount,
		SwapAssetRate:     swapAssetRate,
		AssetName:         assetName,
	}, nil
}
