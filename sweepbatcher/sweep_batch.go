package sweepbatcher

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	sweeppkg "github.com/lightninglabs/loop/sweep"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultFeeRateStep is the default value by which the batch tx's
	// fee rate is increased when an rbf is attempted.
	defaultFeeRateStep = chainfee.SatPerKWeight(100)

	// batchConfHeight is the default confirmation height of the batch
	// transaction.
	batchConfHeight = 3

	// maxFeeToSwapAmtRatio is the maximum fee to swap amount ratio that
	// we allow for a batch transaction.
	maxFeeToSwapAmtRatio = 0.2

	// MaxSweepsPerBatch is the maximum number of sweeps in a single batch.
	// It is needed to prevent sweep tx from becoming non-standard. Max
	// standard transaction is 400k wu, a non-cooperative input is 393 wu.
	MaxSweepsPerBatch = 1000
)

var (
	ErrBatchShuttingDown = errors.New("batch shutting down")
)

// sweep stores any data related to sweeping a specific outpoint.
type sweep struct {
	// swapHash is the hash of the swap that the sweep belongs to.
	// Multiple sweeps may belong to the same swap.
	swapHash lntypes.Hash

	// outpoint is the outpoint being swept.
	outpoint wire.OutPoint

	// value is the value of the outpoint being swept.
	value btcutil.Amount

	// confTarget is the confirmation target of the sweep.
	confTarget int32

	// timeout is the timeout of the swap that the sweep belongs to.
	timeout int32

	// initiationHeight is the height at which the swap was initiated.
	initiationHeight int32

	// htlc is the HTLC that is being swept.
	htlc swap.Htlc

	// preimage is the preimage of the HTLC that is being swept.
	preimage lntypes.Preimage

	// swapInvoicePaymentAddr is the payment address of the swap invoice.
	swapInvoicePaymentAddr [32]byte

	// htlcKeys is the set of keys used to sign the HTLC.
	htlcKeys loopdb.HtlcKeys

	// htlcSuccessEstimator is a function that estimates the weight of the
	// HTLC success script.
	htlcSuccessEstimator func(*input.TxWeightEstimator) error

	// protocolVersion is the protocol version of the swap that the sweep
	// belongs to.
	protocolVersion loopdb.ProtocolVersion

	// isExternalAddr is true if the sweep spends to a non-wallet address.
	isExternalAddr bool

	// destAddr is the destination address of the sweep.
	destAddr btcutil.Address

	// notifier is a collection of channels used to communicate the status
	// of the sweep back to the swap that requested it.
	notifier *SpendNotifier

	// minFeeRate is minimum fee rate that must be used by a batch of
	// the sweep. If it is specified, confTarget is ignored.
	minFeeRate chainfee.SatPerKWeight

	// nonCoopHint is set, if the sweep can not be spent cooperatively and
	// has to be spent using preimage. This is only used in fee estimations
	// when selecting a batch for the sweep to minimize fees.
	nonCoopHint bool

	// coopFailed is set, if we have tried to spend the sweep cooperatively,
	// but it failed. We try to spend a sweep cooperatively only once. This
	// status is not persisted in the DB.
	coopFailed bool
}

// batchState is the state of the batch.
type batchState uint8

const (
	// Open is the state in which the batch is able to accept new sweeps.
	Open batchState = 0

	// Closed is the state in which the batch is no longer able to accept
	// new sweeps.
	Closed batchState = 1

	// Confirmed is the state in which the batch transaction has reached the
	// configured conf height.
	Confirmed batchState = 2
)

// batchConfig is the configuration for a batch.
type batchConfig struct {
	// maxTimeoutDistance is the maximum timeout distance that 2 distinct
	// sweeps can have in the same batch.
	maxTimeoutDistance int32

	// batchConfTarget is the confirmation target of the batch transaction.
	batchConfTarget int32

	// clock provides methods to work with time and timers.
	clock clock.Clock

	// initialDelayProvider provides the delay of first batch publishing
	// after creation. It only affects newly created batches, not batches
	// loaded from DB, so publishing does happen in case of a daemon restart
	// (especially important in case of a crashloop). If a sweep is about to
	// expire (time until timeout is less that 2x initialDelay), then
	// waiting is skipped.
	initialDelayProvider InitialDelayProvider

	// batchPublishDelay is the delay between receiving a new block or
	// initial delay completion and publishing the batch transaction.
	batchPublishDelay time.Duration

	// noBumping instructs sweepbatcher not to fee bump itself and rely on
	// external source of fee rates (FeeRateProvider).
	noBumping bool

	// txLabeler is a function generating a transaction label. It is called
	// before publishing a batch transaction. Batch ID is passed to it.
	txLabeler func(batchID int32) string

	// customMuSig2Signer is a custom signer. If it is set, it is used to
	// create musig2 signatures instead of musig2SignSweep and signerClient.
	// Note that musig2SignSweep must be nil in this case, however signer
	// client must still be provided, as it is used for non-coop spendings.
	customMuSig2Signer SignMuSig2
}

// rbfCache stores data related to our last fee bump.
type rbfCache struct {
	// LastHeight is the last height at which we increased our feerate.
	LastHeight int32

	// FeeRate is the last used fee rate we used to publish a batch tx.
	FeeRate chainfee.SatPerKWeight

	// SkipNextBump instructs updateRbfRate to skip one fee bumping.
	// It is set upon updating FeeRate externally.
	SkipNextBump bool
}

// zeroSweepID is default value for sweep.primarySweepID and batchKit.primaryID.
var zeroSweepID wire.OutPoint

// batch is a collection of sweeps that are published together.
type batch struct {
	// id is the primary identifier of this batch.
	id int32

	// state is the current state of the batch.
	state batchState

	// primarySweepID is the outpoint of the primary sweep in the batch.
	primarySweepID wire.OutPoint

	// sweeps store the sweeps that this batch currently contains.
	sweeps map[wire.OutPoint]sweep

	// currentHeight is the current block height.
	currentHeight int32

	// spendChan is the channel over which spend notifications are received.
	spendChan chan *chainntnfs.SpendDetail

	// confChan is the channel over which confirmation notifications are
	// received.
	confChan chan *chainntnfs.TxConfirmation

	// reorgChan is the channel over which reorg notifications are received.
	reorgChan chan struct{}

	// testReqs is a channel where test requests are received.
	// This is used only in unit tests! The reason to have this is to
	// avoid data races in require.Eventually calls running in parallel
	// to the event loop. See method testRunInEventLoop().
	testReqs chan *testRequest

	// errChan is the channel over which errors are received.
	errChan chan error

	// batchTxid is the transaction that is currently being monitored for
	// confirmations.
	batchTxid *chainhash.Hash

	// batchPkScript is the pkScript of the batch transaction's output.
	batchPkScript []byte

	// batchAddress is the address of the batch transaction's output.
	batchAddress btcutil.Address

	// rbfCache stores data related to the RBF fee bumping mechanism.
	rbfCache rbfCache

	// callEnter is used to sequentialize calls to the batch handler's
	// main event loop.
	callEnter chan struct{}

	// callLeave is used to resume the execution flow of the batch handler's
	// main event loop.
	callLeave chan struct{}

	// stopping signals that the batch is stopping.
	stopping chan struct{}

	// finished signals that the batch has stopped and all child goroutines
	// have finished.
	finished chan struct{}

	// quit is owned by the parent batcher and signals that the batch must
	// stop.
	quit chan struct{}

	// wallet is the wallet client used to create and publish the batch
	// transaction.
	wallet lndclient.WalletKitClient

	// chainNotifier is the chain notifier client used to monitor the
	// blockchain for spends and confirmations.
	chainNotifier lndclient.ChainNotifierClient

	// signerClient is the signer client used to sign the batch transaction.
	signerClient lndclient.SignerClient

	// muSig2SignSweep includes all the required functionality to collect
	// and verify signatures by the swap server in order to cooperatively
	// sweep funds.
	muSig2SignSweep MuSig2SignSweep

	// verifySchnorrSig is a function that verifies a schnorr signature.
	verifySchnorrSig VerifySchnorrSig

	// publishErrorHandler is a function that handles transaction publishing
	// error. By default, it logs all errors as warnings, but "insufficient
	// fee" as Info.
	publishErrorHandler PublishErrorHandler

	// purger is a function that can take a sweep which is being purged and
	// hand it over to the batcher for further processing.
	purger Purger

	// store includes all the database interactions that are needed by the
	// batch.
	store BatcherStore

	// cfg is the configuration for this batch.
	cfg *batchConfig

	// log_ is the logger for this batch.
	log_ atomic.Pointer[btclog.Logger]

	wg sync.WaitGroup
}

// Purger is a function that takes a sweep request and feeds it back to the
// batcher main entry point. The name is inspired by its purpose, which is to
// purge the batch from sweeps that didn't make it to the confirmed tx.
type Purger func(ctx context.Context, sweepReq *SweepRequest) error

// batchKit is a kit of dependencies that are used to initialize a batch. This
// struct is only used as a wrapper for the arguments that are required to
// create a new batch.
type batchKit struct {
	id                  int32
	batchTxid           *chainhash.Hash
	batchPkScript       []byte
	state               batchState
	primaryID           wire.OutPoint
	sweeps              map[wire.OutPoint]sweep
	rbfCache            rbfCache
	wallet              lndclient.WalletKitClient
	chainNotifier       lndclient.ChainNotifierClient
	signerClient        lndclient.SignerClient
	musig2SignSweep     MuSig2SignSweep
	verifySchnorrSig    VerifySchnorrSig
	publishErrorHandler PublishErrorHandler
	purger              Purger
	store               BatcherStore
	log                 btclog.Logger
	quit                chan struct{}
}

// scheduleNextCall schedules the next call to the batch handler's main event
// loop. It returns a function that must be called when the call is finished.
func (b *batch) scheduleNextCall() (func(), error) {
	select {
	case b.callEnter <- struct{}{}:

	case <-b.quit:
		return func() {}, ErrBatcherShuttingDown

	case <-b.stopping:
		return func() {}, ErrBatchShuttingDown

	case <-b.finished:
		return func() {}, ErrBatchShuttingDown
	}

	return func() {
		b.callLeave <- struct{}{}
	}, nil
}

// NewBatch creates a new batch.
func NewBatch(cfg batchConfig, bk batchKit) *batch {
	return &batch{
		// We set the ID to a negative value to flag that this batch has
		// never been persisted, so it needs to be assigned a new ID.
		id:                  -1,
		state:               Open,
		sweeps:              make(map[wire.OutPoint]sweep),
		spendChan:           make(chan *chainntnfs.SpendDetail),
		confChan:            make(chan *chainntnfs.TxConfirmation, 1),
		reorgChan:           make(chan struct{}, 1),
		testReqs:            make(chan *testRequest),
		errChan:             make(chan error, 1),
		callEnter:           make(chan struct{}),
		callLeave:           make(chan struct{}),
		stopping:            make(chan struct{}),
		finished:            make(chan struct{}),
		quit:                bk.quit,
		batchTxid:           bk.batchTxid,
		wallet:              bk.wallet,
		chainNotifier:       bk.chainNotifier,
		signerClient:        bk.signerClient,
		muSig2SignSweep:     bk.musig2SignSweep,
		verifySchnorrSig:    bk.verifySchnorrSig,
		publishErrorHandler: bk.publishErrorHandler,
		purger:              bk.purger,
		store:               bk.store,
		cfg:                 &cfg,
	}
}

// NewBatchFromDB creates a new batch that already existed in storage.
func NewBatchFromDB(cfg batchConfig, bk batchKit) (*batch, error) {
	// Make sure the batch is not empty.
	if len(bk.sweeps) == 0 {
		// This should never happen, as this precondition is already
		// ensured in spinUpBatchFromDB.
		return nil, fmt.Errorf("empty batch is not allowed")
	}

	// Assign batchConfTarget to primary sweep's confTarget.
	for _, sweep := range bk.sweeps {
		if sweep.outpoint == bk.primaryID {
			cfg.batchConfTarget = sweep.confTarget
			break
		}
	}

	b := &batch{
		id:                  bk.id,
		state:               bk.state,
		primarySweepID:      bk.primaryID,
		sweeps:              bk.sweeps,
		spendChan:           make(chan *chainntnfs.SpendDetail),
		confChan:            make(chan *chainntnfs.TxConfirmation, 1),
		reorgChan:           make(chan struct{}, 1),
		testReqs:            make(chan *testRequest),
		errChan:             make(chan error, 1),
		callEnter:           make(chan struct{}),
		callLeave:           make(chan struct{}),
		stopping:            make(chan struct{}),
		finished:            make(chan struct{}),
		quit:                bk.quit,
		batchTxid:           bk.batchTxid,
		batchPkScript:       bk.batchPkScript,
		rbfCache:            bk.rbfCache,
		wallet:              bk.wallet,
		chainNotifier:       bk.chainNotifier,
		signerClient:        bk.signerClient,
		muSig2SignSweep:     bk.musig2SignSweep,
		verifySchnorrSig:    bk.verifySchnorrSig,
		publishErrorHandler: bk.publishErrorHandler,
		purger:              bk.purger,
		store:               bk.store,
		cfg:                 &cfg,
	}

	b.setLog(bk.log)

	return b, nil
}

// log returns current logger.
func (b *batch) log() btclog.Logger {
	return *b.log_.Load()
}

// setLog atomically replaces the logger.
func (b *batch) setLog(logger btclog.Logger) {
	b.log_.Store(&logger)
}

// Debugf logs a message with level DEBUG.
func (b *batch) Debugf(format string, params ...interface{}) {
	b.log().Debugf(format, params...)
}

// Infof logs a message with level INFO.
func (b *batch) Infof(format string, params ...interface{}) {
	b.log().Infof(format, params...)
}

// Warnf logs a message with level WARN.
func (b *batch) Warnf(format string, params ...interface{}) {
	b.log().Warnf(format, params...)
}

// Errorf logs a message with level ERROR.
func (b *batch) Errorf(format string, params ...interface{}) {
	b.log().Errorf(format, params...)
}

// checkSweepToAdd checks if a sweep can be added or updated in the batch. The
// caller must lock the event loop using scheduleNextCall. The function returns
// if the sweep already exists in the batch.
func (b *batch) checkSweepToAdd(_ context.Context, sweep *sweep) (bool, error) {
	// If the provided sweep is nil, we can't proceed with any checks, so
	// we just return early.
	if sweep == nil {
		return false, fmt.Errorf("the sweep is nil")
	}

	// Before we run through the acceptance checks, let's just see if this
	// sweep is already in our batch. In that case, just update the sweep.
	if _, ok := b.sweeps[sweep.outpoint]; ok {
		return true, nil
	}

	// Enforce MaxSweepsPerBatch. If there are already too many sweeps in
	// the batch, do not add another sweep to prevent the tx from becoming
	// non-standard.
	if len(b.sweeps) >= MaxSweepsPerBatch {
		return false, fmt.Errorf("the batch has already too many "+
			"sweeps %d >= %d", len(b.sweeps), MaxSweepsPerBatch)
	}

	// Since all the actions of the batch happen sequentially, we could
	// arrive here after the batch got closed because of a spend. In this
	// case we cannot add the sweep to this batch.
	if b.state != Open {
		return false, fmt.Errorf("the batch state (%v) is not open",
			b.state)
	}

	// If this batch contains a single sweep that spends to a non-wallet
	// address, or the incoming sweep is spending to non-wallet address,
	// we cannot add this sweep to the batch.
	for _, s := range b.sweeps {
		if s.isExternalAddr {
			return false, fmt.Errorf("the batch already has a "+
				"sweep %x with an external address",
				s.swapHash[:6])
		}

		if sweep.isExternalAddr {
			return false, fmt.Errorf("the batch is not empty and "+
				"new sweep %x has an external address",
				sweep.swapHash[:6])
		}
	}

	// Check the timeout of the incoming sweep against the timeout of all
	// already contained sweeps. If that difference exceeds the configured
	// maximum we cannot add this sweep.
	for _, s := range b.sweeps {
		timeoutDistance :=
			int32(math.Abs(float64(sweep.timeout - s.timeout)))

		if timeoutDistance > b.cfg.maxTimeoutDistance {
			return false, fmt.Errorf("too long timeout distance "+
				"between the batch and sweep %x: %d > %d",
				sweep.swapHash[:6], timeoutDistance,
				b.cfg.maxTimeoutDistance)
		}
	}

	// Everything is ok, the sweep can be added to the batch.
	return false, nil
}

// addSweeps tries to add sweeps to the batch. If this is the first sweep being
// added to the batch then it also sets the primary sweep ID. It returns if the
// sweeps were accepted to the batch.
func (b *batch) addSweeps(ctx context.Context, sweeps []*sweep) (bool, error) {
	done, err := b.scheduleNextCall()
	defer done()
	if err != nil {
		return false, err
	}

	// This must be a bug, so log a warning.
	if len(sweeps) == 0 {
		b.Warnf("An attempt to add zero sweeps.")

		return false, nil
	}

	// Track how many new and existing sweeps are among the sweeps.
	var numExisting, numNew int
	for _, s := range sweeps {
		existing, err := b.checkSweepToAdd(ctx, s)
		if err != nil {
			b.Infof("Failed to add sweep %v to batch %d: %v",
				s.outpoint, b.id, err)

			return false, nil
		}
		if existing {
			numExisting++
		} else {
			numNew++
		}
	}

	// Make sure the whole group is either new or existing. If this is not
	// the case, this might be a bug, so print a warning.
	if numExisting > 0 && numNew > 0 {
		b.Warnf("There are %d existing and %d new sweeps among the "+
			"group. They must not be mixed.", numExisting, numNew)

		return false, nil
	}

	// Make sure all the sweeps spend different outpoints.
	outpointsSet := make(map[wire.OutPoint]struct{}, len(sweeps))
	for _, s := range sweeps {
		if _, has := outpointsSet[s.outpoint]; has {
			b.Warnf("Multiple sweeps spend outpoint %v", s.outpoint)

			return false, nil
		}
		outpointsSet[s.outpoint] = struct{}{}
	}

	// Past this point we know that a new incoming sweep passes the
	// acceptance criteria and is now ready to be added to this batch.

	// For an existing group, update the sweeps in the batch.
	if numExisting == len(sweeps) {
		for _, s := range sweeps {
			oldSweep, ok := b.sweeps[s.outpoint]
			if !ok {
				return false, fmt.Errorf("sweep %v not found "+
					"in batch %d", s.outpoint, b.id)
			}

			// Preserve coopFailed value not to forget about
			// cooperative spending failure in this sweep.
			tmp := *s
			tmp.coopFailed = oldSweep.coopFailed

			// If the sweep was resumed from storage, and the swap
			// requested to sweep again, a new sweep notifier will
			// be created by the swap. By re-assigning to the
			// batch's sweep we make sure that everything, including
			// the notifier, is up to date.
			b.sweeps[s.outpoint] = tmp

			// If this is the primary sweep, we also need to update
			// the batch's confirmation target and fee rate.
			if b.primarySweepID == s.outpoint {
				b.cfg.batchConfTarget = s.confTarget
				b.rbfCache.SkipNextBump = true
			}

			// Update batch's fee rate to be greater than or equal
			// to minFeeRate of the sweep. Make sure batch's fee
			// rate does not decrease (otherwise it won't pass RBF
			// rules and won't be broadcasted) and that it is not
			// lower that minFeeRate of other sweeps (so it is
			// applied).
			if b.rbfCache.FeeRate < s.minFeeRate {
				b.rbfCache.FeeRate = s.minFeeRate
			}
		}

		return true, nil
	} else if numNew != len(sweeps) {
		// Sanity check: all the sweeps must be either existing or new.
		// We have checked this above, let's check here as well.
		return false, fmt.Errorf("bug in numExisting and numNew logic:"+
			" numExisting=%d, numNew=%d, len(sweeps)=%d, "+
			"len(b.sweeps)=%d", numExisting, numNew, len(sweeps),
			len(b.sweeps))
	}

	// Here is the code to add new sweeps to a batch.
	for _, s := range sweeps {
		// If this is the first sweep being added to the batch, make it
		// the primary sweep.
		if b.primarySweepID == zeroSweepID {
			b.primarySweepID = s.outpoint
			b.cfg.batchConfTarget = s.confTarget
			b.rbfCache.FeeRate = s.minFeeRate
			b.rbfCache.SkipNextBump = true

			// We also need to start the spend monitor for this new
			// primary sweep.
			err := b.monitorSpend(ctx, *s)
			if err != nil {
				return false, err
			}
		}

		// Make sure the sweep is not present in the batch. If it is
		// present, this is a bug, return an error to stop here.
		if _, has := b.sweeps[s.outpoint]; has {
			return false, fmt.Errorf("sweep %v is already present "+
				"in batch %d", s.outpoint, b.id)
		}

		// Add the sweep to the batch's sweeps.
		b.Infof("adding sweep %v, swap %x", s.outpoint, s.swapHash[:6])
		b.sweeps[s.outpoint] = *s

		// Update FeeRate. Max(s.minFeeRate) for all the sweeps of
		// the batch is the basis for fee bumps.
		if b.rbfCache.FeeRate < s.minFeeRate {
			b.rbfCache.FeeRate = s.minFeeRate
			b.rbfCache.SkipNextBump = true
		}

		if err := b.persistSweep(ctx, *s, false); err != nil {
			return true, err
		}
	}

	return true, nil
}

// sweepExists returns true if the batch contains the sweep with the given
// outpoint.
func (b *batch) sweepExists(outpoint wire.OutPoint) bool {
	done, err := b.scheduleNextCall()
	defer done()
	if err != nil {
		return false
	}

	_, ok := b.sweeps[outpoint]

	return ok
}

// Wait waits for the batch to gracefully stop.
func (b *batch) Wait() {
	b.Infof("Stopping")
	<-b.finished
}

// stillWaitingMsg is the format of the message printed if the batch is about
// to publish, but initial delay has not ended yet.
const stillWaitingMsg = "Skipping publishing, initial delay will end at " +
	"%v, now is %v."

// Run is the batch's main event loop.
func (b *batch) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		close(b.stopping)

		// Make sure not to call b.wg.Wait from any other place to avoid
		// race condition between b.wg.Add(1) and b.wg.Wait().
		b.wg.Wait()
		close(b.finished)
	}()

	if b.muSig2SignSweep == nil && b.cfg.customMuSig2Signer == nil {
		return fmt.Errorf("no musig2 signer available")
	}
	if b.muSig2SignSweep != nil && b.cfg.customMuSig2Signer != nil {
		return fmt.Errorf("both musig2 signers provided")
	}

	// Cache clock variable.
	clock := b.cfg.clock
	startTime := clock.Now()

	blockChan, blockErrChan, err :=
		b.chainNotifier.RegisterBlockEpochNtfn(runCtx)
	if err != nil {
		return fmt.Errorf("block registration error: %w", err)
	}

	// Set currentHeight here, because it may be needed in monitorSpend.
	select {
	case b.currentHeight = <-blockChan:
		b.Debugf("initial height for the batch is %v", b.currentHeight)

	case <-runCtx.Done():
		return fmt.Errorf("context expired while waiting for current "+
			"height: %w", runCtx.Err())
	}

	// If a primary sweep exists we immediately start monitoring for its
	// spend.
	if b.primarySweepID != zeroSweepID {
		sweep := b.sweeps[b.primarySweepID]
		err := b.monitorSpend(runCtx, sweep)
		if err != nil {
			return fmt.Errorf("monitorSpend error: %w", err)
		}
	}

	// skipBefore is the time before which we skip batch publishing.
	// This is needed to facilitate better grouping of sweeps.
	// The value is set only if the batch has at least one sweep.
	// For batches loaded from DB initialDelay should be 0.
	var skipBefore *time.Time

	// initialDelayChan is a timer which fires upon initial delay end.
	// If initialDelay is set to 0, it will not trigger to avoid setting up
	// timerChan twice, which could lead to double publishing if
	// batchPublishDelay is also 0.
	var initialDelayChan <-chan time.Time

	// We use a timer in order to not publish new transactions at the same
	// time as the block epoch notification. This is done to prevent
	// unnecessary transaction publishments when a spend is detected on that
	// block. This timer starts after new block arrives (including the
	// current tip which we read from blockChan above) or when initialDelay
	// completes.
	timerChan := clock.TickAfter(b.cfg.batchPublishDelay)

	b.Infof("started, primary %s, total sweeps %d",
		b.primarySweepID, len(b.sweeps))

	for {
		// If the batch is not empty, find earliest initialDelay.
		var totalSweptAmt btcutil.Amount
		for _, sweep := range b.sweeps {
			totalSweptAmt += sweep.value
		}

		skipBeforeUpdated := false
		if totalSweptAmt != 0 {
			initialDelay, err := b.cfg.initialDelayProvider(
				ctx, len(b.sweeps), totalSweptAmt,
			)
			if err != nil {
				b.Warnf("InitialDelayProvider failed: %v. We "+
					"publish this batch without a delay.",
					err)
				initialDelay = 0
			}
			if initialDelay < 0 {
				b.Warnf("Negative delay: %v. We publish this "+
					"batch without a delay.", initialDelay)
				initialDelay = 0
			}
			delayStop := startTime.Add(initialDelay)
			if skipBefore == nil || delayStop.Before(*skipBefore) {
				skipBefore = &delayStop
				skipBeforeUpdated = true
			}
		}

		// Create new timer only if the value of skipBefore was updated.
		// Don't create the timer if the delay is <= 0 to avoid double
		// publishing if batchPublishDelay is also 0.
		if skipBeforeUpdated {
			delay := skipBefore.Sub(clock.Now())
			if delay > 0 {
				initialDelayChan = clock.TickAfter(delay)
			}
		}

		select {
		case <-b.callEnter:
			<-b.callLeave

		// blockChan provides immediately the current tip.
		case height := <-blockChan:
			b.Debugf("received block %v", height)

			// Set the timer to publish the batch transaction after
			// the configured delay.
			timerChan = clock.TickAfter(b.cfg.batchPublishDelay)
			b.currentHeight = height

		case <-initialDelayChan:
			b.Debugf("initial delay of duration %v has ended",
				clock.Now().Sub(startTime))

			// Set the timer to publish the batch transaction after
			// the configured delay.
			timerChan = clock.TickAfter(b.cfg.batchPublishDelay)

		case <-timerChan:
			// Check that batch is still open.
			if b.state != Open {
				b.Debugf("Skipping publishing, because "+
					"the batch is not open (%v).", b.state)
				continue
			}

			if skipBefore == nil {
				b.Debugf("Skipping publishing, because " +
					"the batch is empty.")
				continue
			}

			// If the batch became urgent, skipBefore is set to now.
			if b.isUrgent(*skipBefore) {
				*skipBefore = clock.Now()
			}

			// Check that the initial delay has ended. We have also
			// batchPublishDelay on top of initialDelay, so if
			// initialDelayChan has just fired, this check passes.
			now := clock.Now()
			if skipBefore.After(now) {
				b.Debugf(stillWaitingMsg, *skipBefore, now)
				continue
			}

			err := b.publish(ctx)
			if err != nil {
				return fmt.Errorf("publish error: %w", err)
			}

		case spend := <-b.spendChan:
			err := b.handleSpend(runCtx, spend.SpendingTx)
			if err != nil {
				return fmt.Errorf("handleSpend error: %w", err)
			}

		case <-b.confChan:
			if err := b.handleConf(runCtx); err != nil {
				return fmt.Errorf("handleConf error: %w", err)
			}

			return nil

		case <-b.reorgChan:
			b.state = Open
			b.Warnf("reorg detected, batch is able to " +
				"accept new sweeps")

			err := b.monitorSpend(ctx, b.sweeps[b.primarySweepID])
			if err != nil {
				return fmt.Errorf("monitorSpend error: %w", err)
			}

		case testReq := <-b.testReqs:
			testReq.handler()
			close(testReq.quit)

		case err := <-blockErrChan:
			return fmt.Errorf("blocks monitoring error: %w", err)

		case err := <-b.errChan:
			return fmt.Errorf("error with the batch: %w", err)

		case <-runCtx.Done():
			return fmt.Errorf("batch context expired: %w",
				runCtx.Err())
		}
	}
}

// testRunInEventLoop runs a function in the event loop blocking until
// the function returns. For unit tests only!
func (b *batch) testRunInEventLoop(ctx context.Context, handler func()) {
	// If the event loop is finished, run the function.
	select {
	case <-b.stopping:
		handler()

		return
	default:
	}

	quit := make(chan struct{})
	req := &testRequest{
		handler: handler,
		quit:    quit,
	}

	select {
	case b.testReqs <- req:
	case <-ctx.Done():
		return
	}

	select {
	case <-quit:
	case <-ctx.Done():
	}
}

// timeout returns minimum timeout as block height among sweeps of the batch.
// If the batch is empty, return -1.
func (b *batch) timeout() int32 {
	// Find minimum among sweeps' timeouts.
	minTimeout := int32(-1)
	for _, sweep := range b.sweeps {
		if minTimeout == -1 || minTimeout > sweep.timeout {
			minTimeout = sweep.timeout
		}
	}

	return minTimeout
}

// isUrgent checks if the batch became urgent. This is determined by comparing
// the remaining number of blocks until timeout to the initial delay remained,
// given one block is 10 minutes.
func (b *batch) isUrgent(skipBefore time.Time) bool {
	timeout := b.timeout()
	if timeout <= 0 {
		// This may happen if the batch is empty or if SweepInfo.Timeout
		// is not set, may be possible in tests or if there is a bug.
		b.Warnf("Method timeout() returned %v. Number of "+
			"sweeps: %d. It may be an empty batch.",
			timeout, len(b.sweeps))
		return false
	}

	if b.currentHeight == 0 {
		// currentHeight is not initiated yet.
		return false
	}

	blocksToTimeout := timeout - b.currentHeight
	const blockTime = 10 * time.Minute
	timeBank := time.Duration(blocksToTimeout) * blockTime

	// We want to have at least 2x as much time to be safe.
	const safetyFactor = 2
	remainingWaiting := skipBefore.Sub(b.cfg.clock.Now())

	if timeBank >= safetyFactor*remainingWaiting {
		// There is enough time, keep waiting.
		return false
	}

	b.Debugf("cancelling waiting for urgent sweep (timeBank is %v, "+
		"remainingWaiting is %v)", timeBank, remainingWaiting)

	// Signal to the caller to cancel initialDelay.
	return true
}

// publish creates and publishes the latest batch transaction to the network.
func (b *batch) publish(ctx context.Context) error {
	var (
		err         error
		fee         btcutil.Amount
		signSuccess bool
	)

	if len(b.sweeps) == 0 {
		b.Debugf("skipping publish: no sweeps in the batch")

		return nil
	}

	// Run the RBF rate update.
	err = b.updateRbfRate(ctx)
	if err != nil {
		return err
	}

	// logPublishError is a function which logs publish errors.
	logPublishError := func(errMsg string, err error) {
		b.publishErrorHandler(err, errMsg, b.log())
	}

	fee, err, signSuccess = b.publishMixedBatch(ctx)
	if err != nil {
		if signSuccess {
			logPublishError("publish error", err)

			// Publishing error is expected: "insufficient fee" and
			// "output already spent". Don't return the error here
			// not to break the main loop of the sweep batch.
			return nil
		} else {
			logPublishError("signing error", err)

			// Signing error is not expected, because we have
			// non-cooperative method of signing which should
			// always succeed.
			return err
		}
	}

	b.Infof("published, total sweeps: %v, fees: %v", len(b.sweeps), fee)
	for _, sweep := range b.sweeps {
		b.Infof("published sweep %x, value: %v",
			sweep.swapHash[:6], sweep.value)
	}

	return b.persist(ctx)
}

// createPsbt creates serialized PSBT and prevOuts map from unsignedTx and
// the list of sweeps.
func (b *batch) createPsbt(unsignedTx *wire.MsgTx, sweeps []sweep) ([]byte,
	map[wire.OutPoint]*wire.TxOut, error) {

	// Create PSBT packet object.
	packet, err := psbt.NewFromUnsignedTx(unsignedTx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create PSBT: %w", err)
	}

	// Sanity check: the number of inputs in PSBT must be equal to the
	// number of sweeps.
	if len(packet.Inputs) != len(sweeps) {
		return nil, nil, fmt.Errorf("invalid number of packet inputs")
	}

	// Create prevOuts map.
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(sweeps))

	// Fill input info in PSBT and prevOuts.
	for i, sweep := range sweeps {
		txOut := &wire.TxOut{
			Value:    int64(sweep.value),
			PkScript: sweep.htlc.PkScript,
		}

		prevOuts[sweep.outpoint] = txOut
		packet.Inputs[i].WitnessUtxo = txOut
	}

	// Serialize PSBT.
	var psbtBuf bytes.Buffer
	err = packet.Serialize(&psbtBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize PSBT: %w", err)
	}

	return psbtBuf.Bytes(), prevOuts, nil
}

// constructUnsignedTx creates unsigned tx from the sweeps, paying to the addr.
// It also returns absolute fee (from weight and clamped).
func constructUnsignedTx(sweeps []sweep, address btcutil.Address,
	currentHeight int32, feeRate chainfee.SatPerKWeight) (*wire.MsgTx,
	lntypes.WeightUnit, btcutil.Amount, btcutil.Amount, error) {

	// Sanity check, there should be at least 1 sweep in this batch.
	if len(sweeps) == 0 {
		return nil, 0, 0, 0, fmt.Errorf("no sweeps in batch")
	}

	// Create the batch transaction.
	batchTx := &wire.MsgTx{
		Version:  2,
		LockTime: uint32(currentHeight),
	}

	// Add transaction inputs and estimate its weight.
	var weightEstimate input.TxWeightEstimator
	for _, sweep := range sweeps {
		if sweep.nonCoopHint || sweep.coopFailed {
			// Non-cooperative sweep.
			batchTx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: sweep.outpoint,
				Sequence:         sweep.htlc.SuccessSequence(),
			})

			err := sweep.htlcSuccessEstimator(&weightEstimate)
			if err != nil {
				return nil, 0, 0, 0, fmt.Errorf("sweep."+
					"htlcSuccessEstimator failed: %w", err)
			}
		} else {
			// Cooperative sweep.
			batchTx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: sweep.outpoint,
			})

			weightEstimate.AddTaprootKeySpendInput(
				txscript.SigHashDefault,
			)
		}
	}

	// Convert the destination address to pkScript.
	batchPkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("txscript.PayToAddrScript "+
			"failed: %w", err)
	}

	// Add the output to weight estimates.
	err = sweeppkg.AddOutputEstimate(&weightEstimate, address)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("sweep.AddOutputEstimate "+
			"failed: %w", err)
	}

	// Keep track of the total amount this batch is sweeping back.
	batchAmt := btcutil.Amount(0)
	for _, sweep := range sweeps {
		batchAmt += sweep.value
	}

	// Find weight and fee.
	weight := weightEstimate.Weight()
	feeForWeight := feeRate.FeeForWeight(weight)

	// Fee can be rounded towards zero, leading to actual feeRate being
	// slightly lower than the requested value. Increase the fee if this is
	// the case.
	if chainfee.NewSatPerKWeight(feeForWeight, weight) < feeRate {
		feeForWeight++
	}

	// Clamp the calculated fee to the max allowed fee amount for the batch.
	fee := clampBatchFee(feeForWeight, batchAmt)

	// Add the batch transaction output, which excludes the fees paid to
	// miners.
	batchTx.AddTxOut(&wire.TxOut{
		PkScript: batchPkScript,
		Value:    int64(batchAmt - fee),
	})

	return batchTx, weight, feeForWeight, fee, nil
}

// publishMixedBatch constructs and publishes a batch transaction that can
// include sweeps spent both cooperatively and non-cooperatively. If a sweep is
// marked with nonCoopHint or coopFailed flags, it is spent non-cooperatively.
// If a cooperative sweep fails to sign cooperatively, the whole transaction
// is re-signed again, with this sweep signing non-cooperatively. This process
// is optimized, trying to detect all non-cooperative sweeps in one round. The
// function returns the absolute fee. The last result of the function indicates
// if signing succeeded.
func (b *batch) publishMixedBatch(ctx context.Context) (btcutil.Amount, error,
	bool) {

	// Sanity check, there should be at least 1 sweep in this batch.
	if len(b.sweeps) == 0 {
		return 0, fmt.Errorf("no sweeps in batch"), false
	}

	// Append this sweep to an array of sweeps. This is needed to keep the
	// order of sweeps stored, as iterating the sweeps map does not
	// guarantee same order.
	sweeps := make([]sweep, 0, len(b.sweeps))
	for _, sweep := range b.sweeps {
		sweeps = append(sweeps, sweep)
	}

	// Determine if an external address is used.
	addrOverride := false
	for _, sweep := range sweeps {
		if sweep.isExternalAddr {
			addrOverride = true
		}
	}

	// Find destination address.
	var address btcutil.Address
	if addrOverride {
		// Sanity check, there should be exactly 1 sweep in this batch.
		if len(sweeps) != 1 {
			return 0, fmt.Errorf("external address sweep batched " +
				"with other sweeps"), false
		}

		address = sweeps[0].destAddr
	} else {
		var err error
		address, err = b.getBatchDestAddr(ctx)
		if err != nil {
			return 0, err, false
		}
	}

	// Each iteration of this loop is one attempt to sign the transaction
	// cooperatively. We try cooperative signing only for the sweeps not
	// known in advance to be non-cooperative (nonCoopHint) and not failed
	// to sign cooperatively in previous rounds (coopFailed). If any of them
	// fails, the sweep is excluded from all following rounds and another
	// round is attempted. Otherwise the cycle completes and we sign the
	// remaining sweeps non-cooperatively.
	var (
		tx           *wire.MsgTx
		weight       lntypes.WeightUnit
		feeForWeight btcutil.Amount
		fee          btcutil.Amount
		coopInputs   int
	)
	for attempt := 1; ; attempt++ {
		b.Infof("Attempt %d of collecting cooperative signatures.",
			attempt)

		// Construct unsigned batch transaction.
		var err error
		tx, weight, feeForWeight, fee, err = constructUnsignedTx(
			sweeps, address, b.currentHeight, b.rbfCache.FeeRate,
		)
		if err != nil {
			return 0, fmt.Errorf("failed to construct tx: %w", err),
				false
		}

		// Create PSBT and prevOutsMap.
		psbtBytes, prevOutsMap, err := b.createPsbt(tx, sweeps)
		if err != nil {
			return 0, fmt.Errorf("createPsbt failed: %w", err),
				false
		}

		// Keep track if any new sweep failed to sign cooperatively.
		newCoopFailures := false

		// Try to sign all cooperative sweeps first.
		coopInputs = 0
		for i, sweep := range sweeps {
			// Skip non-cooperative sweeps.
			if sweep.nonCoopHint || sweep.coopFailed {
				continue
			}

			// Try to sign the sweep cooperatively.
			finalSig, err := b.musig2sign(
				ctx, i, sweep, tx, prevOutsMap, psbtBytes,
			)
			if err != nil {
				b.Infof("cooperative signing failed for "+
					"sweep %x: %v", sweep.swapHash[:6], err)

				// Set coopFailed flag for this sweep in all the
				// places we store the sweep.
				sweep.coopFailed = true
				sweeps[i] = sweep
				b.sweeps[sweep.outpoint] = sweep

				// Update newCoopFailures to know if we need
				// another attempt of cooperative signing.
				newCoopFailures = true
			} else {
				// Put the signature to witness of the input.
				tx.TxIn[i].Witness = wire.TxWitness{finalSig}
				coopInputs++
			}
		}

		// If there was any failure of cooperative signing, we need to
		// update weight estimates (since non-cooperative signing has
		// larger witness) and hence update the whole transaction and
		// all the signatures. Otherwise we complete cooperative part.
		if !newCoopFailures {
			break
		}
	}

	// Calculate the expected number of non-cooperative sweeps.
	nonCoopInputs := len(sweeps) - coopInputs

	// Now sign the remaining sweeps' inputs non-cooperatively.
	// For that, first collect sign descriptors for the signatures.
	// Also collect prevOuts for all inputs.
	signDescs := make([]*lndclient.SignDescriptor, 0, nonCoopInputs)
	prevOutsList := make([]*wire.TxOut, 0, len(sweeps))
	for i, sweep := range sweeps {
		// Create and store the previous outpoint for this sweep.
		prevOut := &wire.TxOut{
			Value:    int64(sweep.value),
			PkScript: sweep.htlc.PkScript,
		}
		prevOutsList = append(prevOutsList, prevOut)

		// Skip cooperative sweeps.
		if !sweep.nonCoopHint && !sweep.coopFailed {
			continue
		}

		key, err := btcec.ParsePubKey(
			sweep.htlcKeys.ReceiverScriptKey[:],
		)
		if err != nil {
			return 0, fmt.Errorf("btcec.ParsePubKey failed: %w",
				err), false
		}

		// Create and store the sign descriptor for this sweep.
		signDesc := lndclient.SignDescriptor{
			WitnessScript: sweep.htlc.SuccessScript(),
			Output:        prevOut,
			HashType:      sweep.htlc.SigHash(),
			InputIndex:    i,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: key,
			},
		}

		if sweep.htlc.Version == swap.HtlcV3 {
			signDesc.SignMethod = input.TaprootScriptSpendSignMethod
		}

		signDescs = append(signDescs, &signDesc)
	}

	// Sanity checks.
	if len(signDescs) != nonCoopInputs {
		// This must not happen by construction.
		return 0, fmt.Errorf("unexpected size of signDescs: %d != %d",
			len(signDescs), nonCoopInputs), false
	}
	if len(prevOutsList) != len(sweeps) {
		// This must not happen by construction.
		return 0, fmt.Errorf("unexpected size of prevOutsList: "+
			"%d != %d", len(prevOutsList), len(sweeps)), false
	}

	var rawSigs [][]byte
	if nonCoopInputs > 0 {
		// Produce the signatures for our inputs using sign descriptors.
		var err error
		rawSigs, err = b.signerClient.SignOutputRaw(
			ctx, tx, signDescs, prevOutsList,
		)
		if err != nil {
			return 0, fmt.Errorf("signerClient.SignOutputRaw "+
				"failed: %w", err), false
		}
	}

	// Sanity checks.
	if len(rawSigs) != nonCoopInputs {
		// This must not happen by construction.
		return 0, fmt.Errorf("unexpected size of rawSigs: %d != %d",
			len(rawSigs), nonCoopInputs), false
	}

	// Generate success witnesses for non-cooperative sweeps.
	sigIndex := 0
	for i, sweep := range sweeps {
		// Skip cooperative sweeps.
		if !sweep.nonCoopHint && !sweep.coopFailed {
			continue
		}

		witness, err := sweep.htlc.GenSuccessWitness(
			rawSigs[sigIndex], sweep.preimage,
		)
		if err != nil {
			return 0, fmt.Errorf("sweep.htlc.GenSuccessWitness "+
				"failed: %w", err), false
		}
		sigIndex++

		// Add the success witness to our batch transaction's inputs.
		tx.TxIn[i].Witness = witness
	}

	// Log transaction's details.
	var coopHexs, nonCoopHexs []string
	for _, sweep := range sweeps {
		swapHex := fmt.Sprintf("%x", sweep.swapHash[:6])
		if sweep.nonCoopHint || sweep.coopFailed {
			nonCoopHexs = append(nonCoopHexs, swapHex)
		} else {
			coopHexs = append(coopHexs, swapHex)
		}
	}
	txHash := tx.TxHash()
	b.Infof("attempting to publish batch tx=%v with feerate=%v, "+
		"weight=%v, feeForWeight=%v, fee=%v, sweeps=%d, "+
		"%d cooperative: (%s) and %d non-cooperative (%s), destAddr=%s",
		txHash, b.rbfCache.FeeRate, weight, feeForWeight, fee,
		len(tx.TxIn), coopInputs, strings.Join(coopHexs, ", "),
		nonCoopInputs, strings.Join(nonCoopHexs, ", "), address)

	b.debugLogTx("serialized batch", tx)

	// Make sure tx weight matches the expected value.
	realWeight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
	)
	if realWeight != weight {
		b.Warnf("actual weight of tx %v is %v, estimated as %d",
			txHash, realWeight, weight)
	}

	// Publish the transaction.
	err := b.wallet.PublishTransaction(
		ctx, tx, b.cfg.txLabeler(b.id),
	)
	if err != nil {
		return 0, fmt.Errorf("publishing tx failed: %w", err), true
	}

	// Store the batch transaction's txid and pkScript, for monitoring
	// purposes.
	b.batchTxid = &txHash
	b.batchPkScript = tx.TxOut[0].PkScript

	return fee, nil, true
}

func (b *batch) debugLogTx(msg string, tx *wire.MsgTx) {
	// Serialize the transaction and convert to hex string.
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSize()))
	if err := tx.Serialize(buf); err != nil {
		b.Errorf("failed to serialize tx for debug log: %v", err)
		return
	}

	b.Debugf("%s: %s", msg, hex.EncodeToString(buf.Bytes()))
}

// musig2sign signs one sweep using musig2.
func (b *batch) musig2sign(ctx context.Context, inputIndex int, sweep sweep,
	unsignedTx *wire.MsgTx, prevOuts map[wire.OutPoint]*wire.TxOut,
	psbt []byte) ([]byte, error) {

	prevOutputFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	sigHashes := txscript.NewTxSigHashes(unsignedTx, prevOutputFetcher)

	sigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, unsignedTx, inputIndex,
		prevOutputFetcher,
	)
	if err != nil {
		return nil, err
	}

	var (
		signers       [][]byte
		muSig2Version input.MuSig2Version
	)

	// Depending on the MuSig2 version we either pass 32 byte
	// Schnorr public keys or normal 33 byte public keys.
	if sweep.protocolVersion >= loopdb.ProtocolVersionMuSig2 {
		muSig2Version = input.MuSig2Version100RC2
		signers = [][]byte{
			sweep.htlcKeys.SenderInternalPubKey[:],
			sweep.htlcKeys.ReceiverInternalPubKey[:],
		}
	} else {
		muSig2Version = input.MuSig2Version040
		signers = [][]byte{
			sweep.htlcKeys.SenderInternalPubKey[1:],
			sweep.htlcKeys.ReceiverInternalPubKey[1:],
		}
	}

	htlcScript, ok := sweep.htlc.HtlcScript.(*swap.HtlcScriptV3)
	if !ok {
		return nil, fmt.Errorf("invalid htlc script version")
	}

	var digest [32]byte
	copy(digest[:], sigHash)

	// If a custom signer is installed, use it instead of b.signerClient
	// and b.muSig2SignSweep.
	if b.cfg.customMuSig2Signer != nil {
		// Produce a signature.
		finalSig, err := b.cfg.customMuSig2Signer(
			ctx, muSig2Version, sweep.swapHash,
			htlcScript.RootHash, digest,
		)
		if err != nil {
			return nil, fmt.Errorf("customMuSig2Signer failed: %w",
				err)
		}

		// To be sure that we're good, parse and validate that the
		// combined signature is indeed valid for the sig hash and the
		// internal pubkey.
		err = b.verifySchnorrSig(
			htlcScript.TaprootKey, sigHash, finalSig,
		)
		if err != nil {
			return nil, fmt.Errorf("verifySchnorrSig failed: %w",
				err)
		}

		return finalSig, nil
	}

	// Now we're creating a local MuSig2 session using the receiver key's
	// key locator and the htlc's root hash.
	keyLocator := &sweep.htlcKeys.ClientScriptKeyLocator
	musig2SessionInfo, err := b.signerClient.MuSig2CreateSession(
		ctx, muSig2Version, keyLocator, signers,
		lndclient.MuSig2TaprootTweakOpt(htlcScript.RootHash[:], false),
	)
	if err != nil {
		return nil, fmt.Errorf("signerClient.MuSig2CreateSession "+
			"failed: %w", err)
	}

	// With the session active, we can now send the server our
	// public nonce and the sig hash, so that it can create it's own
	// MuSig2 session and return the server side nonce and partial
	// signature.
	serverNonce, serverSig, err := b.muSig2SignSweep(
		ctx, sweep.protocolVersion, sweep.swapHash,
		sweep.swapInvoicePaymentAddr,
		musig2SessionInfo.PublicNonce[:], psbt, prevOuts,
	)
	if err != nil {
		return nil, err
	}

	var serverPublicNonce [musig2.PubNonceSize]byte
	copy(serverPublicNonce[:], serverNonce)

	// Register the server's nonce before attempting to create our
	// partial signature.
	haveAllNonces, err := b.signerClient.MuSig2RegisterNonces(
		ctx, musig2SessionInfo.SessionID,
		[][musig2.PubNonceSize]byte{serverPublicNonce},
	)
	if err != nil {
		return nil, err
	}

	// Sanity check that we have all the nonces.
	if !haveAllNonces {
		return nil, fmt.Errorf("invalid MuSig2 session: " +
			"nonces missing")
	}

	// Since our MuSig2 session has all nonces, we can now create
	// the local partial signature by signing the sig hash.
	_, err = b.signerClient.MuSig2Sign(
		ctx, musig2SessionInfo.SessionID, digest, false,
	)
	if err != nil {
		return nil, err
	}

	// Now combine the partial signatures to use the final combined
	// signature in the sweep transaction's witness.
	haveAllSigs, finalSig, err := b.signerClient.MuSig2CombineSig(
		ctx, musig2SessionInfo.SessionID, [][]byte{serverSig},
	)
	if err != nil {
		return nil, err
	}

	if !haveAllSigs {
		return nil, fmt.Errorf("failed to combine signatures")
	}

	// To be sure that we're good, parse and validate that the
	// combined signature is indeed valid for the sig hash and the
	// internal pubkey.
	err = b.verifySchnorrSig(htlcScript.TaprootKey, sigHash, finalSig)
	if err != nil {
		return nil, err
	}

	return finalSig, nil
}

// updateRbfRate updates the fee rate we should use for the new batch
// transaction. This fee rate does not guarantee RBF success, but the continuous
// increase leads to an eventual successful RBF replacement.
func (b *batch) updateRbfRate(ctx context.Context) error {
	// If the feeRate is unset then we never published before, so we
	// retrieve the fee estimate from our wallet.
	if b.rbfCache.FeeRate == 0 {
		// We set minFeeRate in each sweep, so fee rate is expected to
		// be initiated here.
		b.Warnf("rbfCache.FeeRate is 0, which must not happen.")

		if b.cfg.batchConfTarget == 0 {
			b.Warnf("updateRbfRate called with zero " +
				"batchConfTarget")
		}

		b.Infof("initializing rbf fee rate for conf target=%v",
			b.cfg.batchConfTarget)

		rate, err := b.wallet.EstimateFeeRate(
			ctx, b.cfg.batchConfTarget,
		)
		if err != nil {
			return err
		}

		// Set the initial value for our fee rate.
		b.rbfCache.FeeRate = rate
	} else if !b.cfg.noBumping {
		if b.rbfCache.SkipNextBump {
			// Skip fee bumping, unset the flag, to bump next time.
			b.rbfCache.SkipNextBump = false
		} else {
			// Bump the fee rate by the configured step.
			b.rbfCache.FeeRate += defaultFeeRateStep
		}
	}

	b.rbfCache.LastHeight = b.currentHeight

	return b.persist(ctx)
}

// monitorSpend monitors the primary sweep's outpoint for spends. The reason we
// monitor the primary sweep's outpoint is because the primary sweep was the
// first sweep that entered this batch, therefore it is present in all the
// versions of the batch transaction. This means that even if an older version
// of the batch transaction gets confirmed, due to the uncertainty of RBF
// replacements and network propagation, we can always detect the transaction.
func (b *batch) monitorSpend(ctx context.Context, primarySweep sweep) error {
	spendCtx, cancel := context.WithCancel(ctx)

	spendChan, spendErr, err := b.chainNotifier.RegisterSpendNtfn(
		spendCtx, &primarySweep.outpoint, primarySweep.htlc.PkScript,
		primarySweep.initiationHeight,
	)
	if err != nil {
		cancel()

		return err
	}

	b.wg.Add(1)
	go func() {
		defer cancel()
		defer b.wg.Done()

		b.Infof("monitoring spend for outpoint %s",
			primarySweep.outpoint.String())

		for {
			select {
			case spend := <-spendChan:
				select {
				case b.spendChan <- spend:

				case <-ctx.Done():
				}

				return

			case err := <-spendErr:
				b.writeToErrChan(
					fmt.Errorf("spend error: %w", err),
				)

				return

			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

// monitorConfirmations monitors the batch transaction for confirmations.
func (b *batch) monitorConfirmations(ctx context.Context) error {
	// Find initiationHeight.
	primarySweep, ok := b.sweeps[b.primarySweepID]
	if !ok {
		return fmt.Errorf("can't find primarySweep")
	}

	reorgChan := make(chan struct{})

	confCtx, cancel := context.WithCancel(ctx)

	confChan, errChan, err := b.chainNotifier.RegisterConfirmationsNtfn(
		confCtx, b.batchTxid, b.batchPkScript, batchConfHeight,
		primarySweep.initiationHeight,
		lndclient.WithReOrgChan(reorgChan),
	)
	if err != nil {
		cancel()
		return err
	}

	b.wg.Add(1)
	go func() {
		defer cancel()
		defer b.wg.Done()

		for {
			select {
			case conf := <-confChan:
				select {
				case b.confChan <- conf:

				case <-ctx.Done():
				}

				return

			case err := <-errChan:
				b.writeToErrChan(fmt.Errorf("confirmations "+
					"monitoring error: %w", err))

				return

			case <-reorgChan:
				// A re-org has been detected. We set the batch
				// state back to open since our batch
				// transaction is no longer present in any
				// block. We can accept more sweeps and try to
				// publish new transactions, at this point we
				// need to monitor again for a new spend.
				select {
				case b.reorgChan <- struct{}{}:
				case <-ctx.Done():
				}
				return

			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

// getFeePortionForSweep calculates the fee portion that each sweep should pay
// for the batch transaction. The fee is split evenly among the sweeps, If the
// fee cannot be split evenly, the remainder is paid by the first sweep.
func getFeePortionForSweep(spendTx *wire.MsgTx, numSweeps int,
	totalSweptAmt btcutil.Amount) (btcutil.Amount, btcutil.Amount) {

	totalFee := int64(totalSweptAmt)
	if len(spendTx.TxOut) > 0 {
		totalFee -= spendTx.TxOut[0].Value
	}
	feePortionPerSweep := totalFee / int64(numSweeps)
	roundingDiff := totalFee - (int64(numSweeps) * feePortionPerSweep)

	return btcutil.Amount(feePortionPerSweep), btcutil.Amount(roundingDiff)
}

// getFeePortionPaidBySweep returns the fee portion that the sweep should pay
// for the batch transaction. If the sweep is the first sweep in the batch, it
// pays the rounding difference.
func getFeePortionPaidBySweep(spendTx *wire.MsgTx, feePortionPerSweep,
	roundingDiff btcutil.Amount, sweep *sweep) btcutil.Amount {

	if bytes.Equal(spendTx.TxIn[0].SignatureScript, sweep.htlc.SigScript) {
		return feePortionPerSweep + roundingDiff
	}

	return feePortionPerSweep
}

// handleSpend handles a spend notification.
func (b *batch) handleSpend(ctx context.Context, spendTx *wire.MsgTx) error {
	var (
		txHash     = spendTx.TxHash()
		purgeList  = make([]SweepRequest, 0, len(b.sweeps))
		notifyList = make([]sweep, 0, len(b.sweeps))
	)
	b.batchTxid = &txHash
	if len(spendTx.TxOut) > 0 {
		b.batchPkScript = spendTx.TxOut[0].PkScript
	} else {
		b.Warnf("transaction %v has no outputs", txHash)
	}

	// As a previous version of the batch transaction may get confirmed,
	// which does not contain the latest sweeps, we need to detect the
	// sweeps that did not make it to the confirmed transaction and feed
	// them back to the batcher. This will ensure that the sweeps will enter
	// a new batch instead of remaining dangling.
	var totalSweptAmt btcutil.Amount
	for _, sweep := range b.sweeps {
		found := false

		for _, txIn := range spendTx.TxIn {
			if txIn.PreviousOutPoint == sweep.outpoint {
				found = true
				totalSweptAmt += sweep.value
				notifyList = append(notifyList, sweep)
			}
		}

		// If the sweep's outpoint was not found in the transaction's
		// inputs this means it was left out. So we delete it from this
		// batch and feed it back to the batcher.
		if !found {
			newSweep := sweep
			delete(b.sweeps, sweep.outpoint)
			purgeList = append(purgeList, SweepRequest{
				SwapHash: newSweep.swapHash,
				Inputs: []Input{
					{
						Outpoint: newSweep.outpoint,
						Value:    newSweep.value,
					},
				},
				Notifier: newSweep.notifier,
			})
		}
	}

	// Calculate the fee portion that each sweep should pay for the batch.
	feePortionPaidPerSweep, roundingDifference := getFeePortionForSweep(
		spendTx, len(notifyList), totalSweptAmt,
	)

	for _, sweep := range notifyList {
		// Save the sweep as completed.
		err := b.persistSweep(ctx, sweep, true)
		if err != nil {
			return err
		}

		// If the sweep's notifier is empty then this means that a swap
		// is not waiting to read an update from it, so we can skip
		// the notification part.
		if sweep.notifier == nil ||
			*sweep.notifier == (SpendNotifier{}) {

			continue
		}

		spendDetail := SpendDetail{
			Tx: spendTx,
			OnChainFeePortion: getFeePortionPaidBySweep(
				spendTx, feePortionPaidPerSweep,
				roundingDifference, &sweep,
			),
		}

		// Dispatch the sweep notifier, we don't care about the outcome
		// of this action so we don't wait for it.
		go sweep.notifySweepSpend(ctx, &spendDetail)
	}

	// Proceed with purging the sweeps. This will feed the sweeps that
	// didn't make it to the confirmed batch transaction back to the batcher
	// for re-entry. This batch doesn't care for the outcome of this
	// operation so we don't wait for it.
	go func() {
		// Make sure this context doesn't expire so we successfully
		// add the sweeps to the batcher.
		ctx := context.WithoutCancel(ctx)

		// Iterate over the purge list and feed the sweeps back to the
		// batcher.
		for _, sweep := range purgeList {
			err := b.purger(ctx, &sweep)
			if err != nil {
				b.Errorf("unable to purge sweep %x: %v",
					sweep.SwapHash[:6], err)
			}
		}
	}()

	b.Infof("spent, total sweeps: %v, purged sweeps: %v",
		len(notifyList), len(purgeList))

	err := b.monitorConfirmations(ctx)
	if err != nil {
		return err
	}

	// We are no longer able to accept new sweeps, so we mark the batch as
	// closed and persist on storage.
	b.state = Closed

	return b.persist(ctx)
}

// handleConf handles a confirmation notification. This is the final step of the
// batch. Here we signal to the batcher that this batch was completed.
func (b *batch) handleConf(ctx context.Context) error {
	b.Infof("confirmed in txid %s", b.batchTxid)
	b.state = Confirmed

	return b.store.ConfirmBatch(ctx, b.id)
}

// isComplete returns true if the batch is completed. This method is used by the
// batcher for lazy deletion of batches.
func (b *batch) isComplete() bool {
	done, err := b.scheduleNextCall()
	defer done()

	// We override the ErrBatchShuttingDown error as that is the expected
	// error to be returned by the scheduler once the batch's main run loop
	// has exited.
	if err != nil && err != ErrBatchShuttingDown {
		return false
	}
	return b.state == Confirmed
}

// persist updates the batch in the database.
func (b *batch) persist(ctx context.Context) error {
	bch := &dbBatch{}

	bch.ID = b.id
	bch.State = stateEnumToString(b.state)

	if b.batchTxid != nil {
		bch.BatchTxid = *b.batchTxid
	}

	bch.BatchPkScript = b.batchPkScript
	bch.LastRbfHeight = b.rbfCache.LastHeight
	bch.LastRbfSatPerKw = int32(b.rbfCache.FeeRate)
	bch.MaxTimeoutDistance = b.cfg.maxTimeoutDistance

	return b.store.UpdateSweepBatch(ctx, bch)
}

// getBatchDestAddr returns the batch's destination address. If the batch
// has already generated an address then the same one will be returned.
func (b *batch) getBatchDestAddr(ctx context.Context) (btcutil.Address, error) {
	var address btcutil.Address

	// If a batch address is set, use that. Otherwise, generate a
	// new address.
	if b.batchAddress != nil {
		address = b.batchAddress
	} else {
		var err error

		// Generate a wallet address for the batch transaction's output.
		address, err = b.wallet.NextAddr(
			ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
		if err != nil {
			return address, err
		}

		// Save that new address in order to re-use in future
		// versions of the batch tx.
		b.batchAddress = address
	}

	return address, nil
}

func (b *batch) insertAndAcquireID(ctx context.Context) (int32, error) {
	bch := &dbBatch{}
	bch.State = stateEnumToString(b.state)
	bch.MaxTimeoutDistance = b.cfg.maxTimeoutDistance

	id, err := b.store.InsertSweepBatch(ctx, bch)
	if err != nil {
		return 0, err
	}

	b.id = id
	b.setLog(batchPrefixLogger(fmt.Sprintf("%d", b.id)))

	return id, nil
}

// notifySweepSpend writes the spendTx to the sweep's notifier channel.
func (s *sweep) notifySweepSpend(ctx context.Context,
	spendDetail *SpendDetail) {

	select {
	// Try to write the update to the notification channel.
	case s.notifier.SpendChan <- spendDetail:

	// If a quit signal was provided by the swap, continue.
	case <-s.notifier.QuitChan:

	// If the context was canceled, return.
	case <-ctx.Done():
	}
}

func (b *batch) writeToErrChan(err error) {
	select {
	case b.errChan <- err:
	default:
	}
}

func (b *batch) persistSweep(ctx context.Context, sweep sweep,
	completed bool) error {

	return b.store.UpsertSweep(ctx, &dbSweep{
		BatchID:   b.id,
		SwapHash:  sweep.swapHash,
		Outpoint:  sweep.outpoint,
		Amount:    sweep.value,
		Completed: completed,
	})
}

// clampBatchFee takes the fee amount and total amount of the sweeps in the
// batch and makes sure the fee is not too high. If the fee is too high, it is
// clamped to the maximum allowed fee.
func clampBatchFee(fee btcutil.Amount,
	totalAmount btcutil.Amount) btcutil.Amount {

	maxFeeAmount := btcutil.Amount(float64(totalAmount) *
		maxFeeToSwapAmtRatio)

	if fee > maxFeeAmount {
		return maxFeeAmount
	}

	return fee
}

func stateEnumToString(state batchState) string {
	switch state {
	case Open:
		return batchOpen

	case Closed:
		return batchClosed

	case Confirmed:
		return batchConfirmed
	}

	return ""
}
