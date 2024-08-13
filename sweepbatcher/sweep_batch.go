package sweepbatcher

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
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
)

var (
	ErrBatchShuttingDown = errors.New("batch shutting down")
)

// sweep stores any data related to sweeping a specific outpoint.
type sweep struct {
	// swapHash is the hash of the swap that the sweep belongs to.
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

	// initialDelay is the delay of first batch publishing after creation.
	// It only affects newly created batches, not batches loaded from DB,
	// so publishing does happen in case of a daemon restart (especially
	// important in case of a crashloop).
	initialDelay time.Duration

	// batchPublishDelay is the delay between receiving a new block or
	// initial delay completion and publishing the batch transaction.
	batchPublishDelay time.Duration

	// noBumping instructs sweepbatcher not to fee bump itself and rely on
	// external source of fee rates (FeeRateProvider).
	noBumping bool

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

// batch is a collection of sweeps that are published together.
type batch struct {
	// id is the primary identifier of this batch.
	id int32

	// state is the current state of the batch.
	state batchState

	// primarySweepID is the swap hash of the primary sweep in the batch.
	primarySweepID lntypes.Hash

	// sweeps store the sweeps that this batch currently contains.
	sweeps map[lntypes.Hash]sweep

	// currentHeight is the current block height.
	currentHeight int32

	// blockEpochChan is the channel over which block epoch notifications
	// are received.
	blockEpochChan chan int32

	// spendChan is the channel over which spend notifications are received.
	spendChan chan *chainntnfs.SpendDetail

	// confChan is the channel over which confirmation notifications are
	// received.
	confChan chan *chainntnfs.TxConfirmation

	// reorgChan is the channel over which reorg notifications are received.
	reorgChan chan struct{}

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

	// purger is a function that can take a sweep which is being purged and
	// hand it over to the batcher for further processing.
	purger Purger

	// store includes all the database interactions that are needed by the
	// batch.
	store BatcherStore

	// cfg is the configuration for this batch.
	cfg *batchConfig

	// log is the logger for this batch.
	log btclog.Logger

	wg sync.WaitGroup
}

// Purger is a function that takes a sweep request and feeds it back to the
// batcher main entry point. The name is inspired by its purpose, which is to
// purge the batch from sweeps that didn't make it to the confirmed tx.
type Purger func(sweepReq *SweepRequest) error

// batchKit is a kit of dependencies that are used to initialize a batch. This
// struct is only used as a wrapper for the arguments that are required to
// create a new batch.
type batchKit struct {
	id               int32
	batchTxid        *chainhash.Hash
	batchPkScript    []byte
	state            batchState
	primaryID        lntypes.Hash
	sweeps           map[lntypes.Hash]sweep
	rbfCache         rbfCache
	returnChan       chan SweepRequest
	wallet           lndclient.WalletKitClient
	chainNotifier    lndclient.ChainNotifierClient
	signerClient     lndclient.SignerClient
	musig2SignSweep  MuSig2SignSweep
	verifySchnorrSig VerifySchnorrSig
	purger           Purger
	store            BatcherStore
	log              btclog.Logger
	quit             chan struct{}
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
		id:               -1,
		state:            Open,
		sweeps:           make(map[lntypes.Hash]sweep),
		blockEpochChan:   make(chan int32),
		spendChan:        make(chan *chainntnfs.SpendDetail),
		confChan:         make(chan *chainntnfs.TxConfirmation, 1),
		reorgChan:        make(chan struct{}, 1),
		errChan:          make(chan error, 1),
		callEnter:        make(chan struct{}),
		callLeave:        make(chan struct{}),
		stopping:         make(chan struct{}),
		finished:         make(chan struct{}),
		quit:             bk.quit,
		batchTxid:        bk.batchTxid,
		wallet:           bk.wallet,
		chainNotifier:    bk.chainNotifier,
		signerClient:     bk.signerClient,
		muSig2SignSweep:  bk.musig2SignSweep,
		verifySchnorrSig: bk.verifySchnorrSig,
		purger:           bk.purger,
		store:            bk.store,
		cfg:              &cfg,
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
		if sweep.swapHash == bk.primaryID {
			cfg.batchConfTarget = sweep.confTarget
			break
		}
	}

	return &batch{
		id:               bk.id,
		state:            bk.state,
		primarySweepID:   bk.primaryID,
		sweeps:           bk.sweeps,
		blockEpochChan:   make(chan int32),
		spendChan:        make(chan *chainntnfs.SpendDetail),
		confChan:         make(chan *chainntnfs.TxConfirmation, 1),
		reorgChan:        make(chan struct{}, 1),
		errChan:          make(chan error, 1),
		callEnter:        make(chan struct{}),
		callLeave:        make(chan struct{}),
		stopping:         make(chan struct{}),
		finished:         make(chan struct{}),
		quit:             bk.quit,
		batchTxid:        bk.batchTxid,
		batchPkScript:    bk.batchPkScript,
		rbfCache:         bk.rbfCache,
		wallet:           bk.wallet,
		chainNotifier:    bk.chainNotifier,
		signerClient:     bk.signerClient,
		muSig2SignSweep:  bk.musig2SignSweep,
		verifySchnorrSig: bk.verifySchnorrSig,
		purger:           bk.purger,
		store:            bk.store,
		log:              bk.log,
		cfg:              &cfg,
	}, nil
}

// addSweep tries to add a sweep to the batch. If this is the first sweep being
// added to the batch then it also sets the primary sweep ID.
func (b *batch) addSweep(ctx context.Context, sweep *sweep) (bool, error) {
	done, err := b.scheduleNextCall()
	defer done()

	if err != nil {
		return false, err
	}

	// If the provided sweep is nil, we can't proceed with any checks, so
	// we just return early.
	if sweep == nil {
		return false, nil
	}

	// Before we run through the acceptance checks, let's just see if this
	// sweep is already in our batch. In that case, just update the sweep.
	_, ok := b.sweeps[sweep.swapHash]
	if ok {
		// If the sweep was resumed from storage, and the swap requested
		// to sweep again, a new sweep notifier will be created by the
		// swap. By re-assigning to the batch's sweep we make sure that
		// everything, including the notifier, is up to date.
		b.sweeps[sweep.swapHash] = *sweep

		// If this is the primary sweep, we also need to update the
		// batch's confirmation target and fee rate.
		if b.primarySweepID == sweep.swapHash {
			b.cfg.batchConfTarget = sweep.confTarget
			b.rbfCache.FeeRate = sweep.minFeeRate
			b.rbfCache.SkipNextBump = true
		}

		return true, nil
	}

	// Since all the actions of the batch happen sequentially, we could
	// arrive here after the batch got closed because of a spend. In this
	// case we cannot add the sweep to this batch.
	if b.state != Open {
		return false, nil
	}

	// If this batch contains a single sweep that spends to a non-wallet
	// address, or the incoming sweep is spending to non-wallet address,
	// we cannot add this sweep to the batch.
	for _, s := range b.sweeps {
		if s.isExternalAddr || sweep.isExternalAddr {
			return false, nil
		}
	}

	// Check the timeout of the incoming sweep against the timeout of all
	// already contained sweeps. If that difference exceeds the configured
	// maximum we cannot add this sweep.
	for _, s := range b.sweeps {
		timeoutDistance :=
			int32(math.Abs(float64(sweep.timeout - s.timeout)))

		if timeoutDistance > b.cfg.maxTimeoutDistance {
			return false, nil
		}
	}

	// Past this point we know that a new incoming sweep passes the
	// acceptance criteria and is now ready to be added to this batch.

	// If this is the first sweep being added to the batch, make it the
	// primary sweep.
	if b.primarySweepID == lntypes.ZeroHash {
		b.primarySweepID = sweep.swapHash
		b.cfg.batchConfTarget = sweep.confTarget
		b.rbfCache.FeeRate = sweep.minFeeRate
		b.rbfCache.SkipNextBump = true

		// We also need to start the spend monitor for this new primary
		// sweep.
		err := b.monitorSpend(ctx, *sweep)
		if err != nil {
			return false, err
		}
	}

	// Add the sweep to the batch's sweeps.
	b.log.Infof("adding sweep %x", sweep.swapHash[:6])
	b.sweeps[sweep.swapHash] = *sweep

	// Update FeeRate. Max(sweep.minFeeRate) for all the sweeps of
	// the batch is the basis for fee bumps.
	if b.rbfCache.FeeRate < sweep.minFeeRate {
		b.rbfCache.FeeRate = sweep.minFeeRate
		b.rbfCache.SkipNextBump = true
	}

	return true, b.persistSweep(ctx, *sweep, false)
}

// sweepExists returns true if the batch contains the sweep with the given hash.
func (b *batch) sweepExists(hash lntypes.Hash) bool {
	done, err := b.scheduleNextCall()
	defer done()
	if err != nil {
		return false
	}

	_, ok := b.sweeps[hash]

	return ok
}

// Wait waits for the batch to gracefully stop.
func (b *batch) Wait() {
	b.log.Infof("Stopping")
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

	blockChan, blockErrChan, err :=
		b.chainNotifier.RegisterBlockEpochNtfn(runCtx)
	if err != nil {
		return err
	}

	// If a primary sweep exists we immediately start monitoring for its
	// spend.
	if b.primarySweepID != lntypes.ZeroHash {
		sweep := b.sweeps[b.primarySweepID]
		err := b.monitorSpend(runCtx, sweep)
		if err != nil {
			return err
		}
	}

	// skipBefore is the time before which we skip batch publishing.
	// This is needed to facilitate better grouping of sweeps.
	// For batches loaded from DB initialDelay should be 0.
	skipBefore := clock.Now().Add(b.cfg.initialDelay)

	// initialDelayChan is a timer which fires upon initial delay end.
	// If initialDelay is 0, it does not fire to prevent race with
	// blockChan which also fires immediately with current tip. Such a race
	// may result in double publishing if batchPublishDelay is also 0.
	var initialDelayChan <-chan time.Time
	if b.cfg.initialDelay > 0 {
		initialDelayChan = clock.TickAfter(b.cfg.initialDelay)
	}

	// We use a timer in order to not publish new transactions at the same
	// time as the block epoch notification. This is done to prevent
	// unnecessary transaction publishments when a spend is detected on that
	// block. This timer starts after new block arrives or initialDelay
	// completes.
	var timerChan <-chan time.Time

	b.log.Infof("started, primary %x, total sweeps %v",
		b.primarySweepID[0:6], len(b.sweeps))

	for {
		select {
		case <-b.callEnter:
			<-b.callLeave

		// blockChan provides immediately the current tip.
		case height := <-blockChan:
			b.log.Debugf("received block %v", height)

			// Set the timer to publish the batch transaction after
			// the configured delay.
			timerChan = clock.TickAfter(b.cfg.batchPublishDelay)
			b.currentHeight = height

		case <-initialDelayChan:
			b.log.Debugf("initial delay of duration %v has ended",
				b.cfg.initialDelay)

			// Set the timer to publish the batch transaction after
			// the configured delay.
			timerChan = clock.TickAfter(b.cfg.batchPublishDelay)

		case <-timerChan:
			// Check that batch is still open.
			if b.state != Open {
				b.log.Debugf("Skipping publishing, because the"+
					"batch is not open (%v).", b.state)
				continue
			}

			// If the batch became urgent, skipBefore is set to now.
			if b.isUrgent(skipBefore) {
				skipBefore = clock.Now()
			}

			// Check that the initial delay has ended. We have also
			// batchPublishDelay on top of initialDelay, so if
			// initialDelayChan has just fired, this check passes.
			now := clock.Now()
			if skipBefore.After(now) {
				b.log.Debugf(stillWaitingMsg, skipBefore, now)
				continue
			}

			err := b.publish(ctx)
			if err != nil {
				return err
			}

		case spend := <-b.spendChan:
			err := b.handleSpend(runCtx, spend.SpendingTx)
			if err != nil {
				return err
			}

		case <-b.confChan:
			return b.handleConf(runCtx)

		case <-b.reorgChan:
			b.state = Open
			b.log.Warnf("reorg detected, batch is able to accept " +
				"new sweeps")

			err := b.monitorSpend(ctx, b.sweeps[b.primarySweepID])
			if err != nil {
				return err
			}

		case err := <-blockErrChan:
			return err

		case err := <-b.errChan:
			return err

		case <-runCtx.Done():
			return runCtx.Err()
		}
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
		b.log.Warnf("Method timeout() returned %v. Number of"+
			" sweeps: %d. It may be an empty batch.",
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

	b.log.Debugf("cancelling waiting for urgent sweep (timeBank is %v, "+
		"remainingWaiting is %v)", timeBank, remainingWaiting)

	// Signal to the caller to cancel initialDelay.
	return true
}

// publish creates and publishes the latest batch transaction to the network.
func (b *batch) publish(ctx context.Context) error {
	var (
		err         error
		fee         btcutil.Amount
		coopSuccess bool
	)

	if len(b.sweeps) == 0 {
		b.log.Debugf("skipping publish: no sweeps in the batch")

		return nil
	}

	// Run the RBF rate update.
	err = b.updateRbfRate(ctx)
	if err != nil {
		return err
	}

	fee, err, coopSuccess = b.publishBatchCoop(ctx)
	if err != nil {
		b.log.Warnf("co-op publish error: %v", err)
	}

	if !coopSuccess {
		fee, err = b.publishBatch(ctx)
	}
	if err != nil {
		b.log.Warnf("publish error: %v", err)
		return nil
	}

	b.log.Infof("published, total sweeps: %v, fees: %v", len(b.sweeps), fee)
	for _, sweep := range b.sweeps {
		b.log.Infof("published sweep %x, value: %v",
			sweep.swapHash[:6], sweep.value)
	}

	return b.persist(ctx)
}

// publishBatch creates and publishes the batch transaction. It will consult the
// RBFCache to determine the fee rate to use.
func (b *batch) publishBatch(ctx context.Context) (btcutil.Amount, error) {
	// Create the batch transaction.
	batchTx := wire.NewMsgTx(2)
	batchTx.LockTime = uint32(b.currentHeight)

	var (
		batchAmt  btcutil.Amount
		prevOuts  = make([]*wire.TxOut, 0, len(b.sweeps))
		signDescs = make(
			[]*lndclient.SignDescriptor, 0, len(b.sweeps),
		)
		sweeps       = make([]sweep, 0, len(b.sweeps))
		fee          btcutil.Amount
		inputCounter int
		addrOverride bool
	)

	var weightEstimate input.TxWeightEstimator

	// Add all the sweeps to the batch transaction.
	for _, sweep := range b.sweeps {
		if sweep.isExternalAddr {
			addrOverride = true
		}

		batchAmt += sweep.value
		batchTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: sweep.outpoint,
			Sequence:         sweep.htlc.SuccessSequence(),
		})

		err := sweep.htlcSuccessEstimator(&weightEstimate)
		if err != nil {
			return 0, err
		}

		// Append this sweep to an array of sweeps. This is needed to
		// keep the order of sweeps stored, as iterating the sweeps map
		// does not guarantee same order.
		sweeps = append(sweeps, sweep)

		// Create and store the previous outpoint for this sweep.
		prevOuts = append(prevOuts, &wire.TxOut{
			Value:    int64(sweep.value),
			PkScript: sweep.htlc.PkScript,
		})

		key, err := btcec.ParsePubKey(
			sweep.htlcKeys.ReceiverScriptKey[:],
		)
		if err != nil {
			return fee, err
		}

		// Create and store the sign descriptor for this sweep.
		signDesc := lndclient.SignDescriptor{
			WitnessScript: sweep.htlc.SuccessScript(),
			Output:        prevOuts[len(prevOuts)-1],
			HashType:      sweep.htlc.SigHash(),
			InputIndex:    inputCounter,
			KeyDesc: keychain.KeyDescriptor{
				PubKey: key,
			},
		}

		inputCounter++

		if sweep.htlc.Version == swap.HtlcV3 {
			signDesc.SignMethod = input.TaprootScriptSpendSignMethod
		}

		signDescs = append(signDescs, &signDesc)
	}

	var address btcutil.Address

	if addrOverride {
		// Sanity check, there should be exactly 1 sweep in this batch.
		if len(sweeps) != 1 {
			return 0, fmt.Errorf("external address sweep batched " +
				"with other sweeps")
		}

		address = sweeps[0].destAddr
	} else {
		var err error
		address, err = b.getBatchDestAddr(ctx)
		if err != nil {
			return fee, err
		}
	}

	batchPkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return fee, err
	}

	err = sweeppkg.AddOutputEstimate(&weightEstimate, address)
	if err != nil {
		return fee, err
	}

	weight := weightEstimate.Weight()
	feeForWeight := b.rbfCache.FeeRate.FeeForWeight(weight)

	// Clamp the calculated fee to the max allowed fee amount for the batch.
	fee = clampBatchFee(feeForWeight, batchAmt)

	// Add the batch transaction output, which excludes the fees paid to
	// miners.
	batchTx.AddTxOut(&wire.TxOut{
		PkScript: batchPkScript,
		Value:    int64(batchAmt - fee),
	})

	// Collect the signatures for our inputs.
	rawSigs, err := b.signerClient.SignOutputRaw(
		ctx, batchTx, signDescs, prevOuts,
	)
	if err != nil {
		return fee, err
	}

	for i, sweep := range sweeps {
		// Generate the success witness for the sweep.
		witness, err := sweep.htlc.GenSuccessWitness(
			rawSigs[i], sweep.preimage,
		)
		if err != nil {
			return fee, err
		}

		// Add the success witness to our batch transaction's inputs.
		batchTx.TxIn[i].Witness = witness
	}

	b.log.Infof("attempting to publish non-coop tx=%v with feerate=%v, "+
		"weight=%v, feeForWeight=%v, fee=%v, sweeps=%d, destAddr=%s",
		batchTx.TxHash(), b.rbfCache.FeeRate, weight, feeForWeight, fee,
		len(batchTx.TxIn), address)

	b.debugLogTx("serialized non-coop sweep", batchTx)

	err = b.wallet.PublishTransaction(
		ctx, batchTx, labels.LoopOutBatchSweepSuccess(b.id),
	)
	if err != nil {
		return fee, err
	}

	// Store the batch transaction's txid and pkScript, for monitoring
	// purposes.
	txHash := batchTx.TxHash()
	b.batchTxid = &txHash
	b.batchPkScript = batchPkScript

	return fee, nil
}

// publishBatchCoop attempts to construct and publish a batch transaction that
// collects all the required signatures interactively from the server. This
// helps with collecting the funds immediately without revealing any information
// related to the HTLC script.
func (b *batch) publishBatchCoop(ctx context.Context) (btcutil.Amount,
	error, bool) {

	var (
		batchAmt       = btcutil.Amount(0)
		sweeps         = make([]sweep, 0, len(b.sweeps))
		fee            = btcutil.Amount(0)
		weightEstimate input.TxWeightEstimator
		addrOverride   bool
	)

	// Sanity check, there should be at least 1 sweep in this batch.
	if len(b.sweeps) == 0 {
		return 0, fmt.Errorf("no sweeps in batch"), false
	}

	// Create the batch transaction.
	batchTx := &wire.MsgTx{
		Version:  2,
		LockTime: uint32(b.currentHeight),
	}

	for _, sweep := range b.sweeps {
		// Append this sweep to an array of sweeps. This is needed to
		// keep the order of sweeps stored, as iterating the sweeps map
		// does not guarantee same order.
		sweeps = append(sweeps, sweep)
	}

	// Add all the sweeps to the batch transaction.
	for _, sweep := range sweeps {
		if sweep.isExternalAddr {
			addrOverride = true
		}

		// Keep track of the total amount this batch is sweeping back.
		batchAmt += sweep.value

		// Add this sweep's input to the transaction.
		batchTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: sweep.outpoint,
		})

		weightEstimate.AddTaprootKeySpendInput(txscript.SigHashDefault)
	}

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
			return fee, err, false
		}
	}

	batchPkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return fee, err, false
	}

	err = sweeppkg.AddOutputEstimate(&weightEstimate, address)
	if err != nil {
		return fee, err, false
	}

	weight := weightEstimate.Weight()
	feeForWeight := b.rbfCache.FeeRate.FeeForWeight(weight)

	// Clamp the calculated fee to the max allowed fee amount for the batch.
	fee = clampBatchFee(feeForWeight, batchAmt)

	// Add the batch transaction output, which excludes the fees paid to
	// miners.
	batchTx.AddTxOut(&wire.TxOut{
		PkScript: batchPkScript,
		Value:    int64(batchAmt - fee),
	})

	packet, err := psbt.NewFromUnsignedTx(batchTx)
	if err != nil {
		return fee, err, false
	}

	if len(packet.Inputs) != len(sweeps) {
		return fee, fmt.Errorf("invalid number of packet inputs"), false
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut)

	for i, sweep := range sweeps {
		txOut := &wire.TxOut{
			Value:    int64(sweep.value),
			PkScript: sweep.htlc.PkScript,
		}

		prevOuts[sweep.outpoint] = txOut
		packet.Inputs[i].WitnessUtxo = txOut
	}

	var psbtBuf bytes.Buffer
	err = packet.Serialize(&psbtBuf)
	if err != nil {
		return fee, err, false
	}

	// Attempt to cooperatively sign the batch tx with the server.
	err = b.coopSignBatchTx(
		ctx, packet, sweeps, prevOuts, psbtBuf.Bytes(),
	)
	if err != nil {
		return fee, err, false
	}

	b.log.Infof("attempting to publish coop tx=%v with feerate=%v, "+
		"weight=%v, feeForWeight=%v, fee=%v, sweeps=%d, destAddr=%s",
		batchTx.TxHash(), b.rbfCache.FeeRate, weight, feeForWeight, fee,
		len(batchTx.TxIn), address)

	b.debugLogTx("serialized coop sweep", batchTx)

	err = b.wallet.PublishTransaction(
		ctx, batchTx, labels.LoopOutBatchSweepSuccess(b.id),
	)
	if err != nil {
		return fee, err, true
	}

	// Store the batch transaction's txid and pkScript, for monitoring
	// purposes.
	txHash := batchTx.TxHash()
	b.batchTxid = &txHash
	b.batchPkScript = batchPkScript

	return fee, nil, true
}

func (b *batch) debugLogTx(msg string, tx *wire.MsgTx) {
	// Serialize the transaction and convert to hex string.
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSize()))
	if err := tx.Serialize(buf); err != nil {
		b.log.Errorf("failed to serialize tx for debug log: %v", err)
		return
	}

	b.log.Debugf("%s: %s", msg, hex.EncodeToString(buf.Bytes()))
}

// coopSignBatchTx collects the necessary signatures from the server in order
// to cooperatively sweep the funds.
func (b *batch) coopSignBatchTx(ctx context.Context, packet *psbt.Packet,
	sweeps []sweep, prevOuts map[wire.OutPoint]*wire.TxOut,
	psbt []byte) error {

	for i, sweep := range sweeps {
		sweep := sweep

		finalSig, err := b.musig2sign(
			ctx, i, sweep, packet.UnsignedTx, prevOuts, psbt,
		)
		if err != nil {
			return err
		}

		packet.UnsignedTx.TxIn[i].Witness = wire.TxWitness{
			finalSig,
		}
	}

	return nil
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
		b.log.Warnf("rbfCache.FeeRate is 0, which must not happen.")

		if b.cfg.batchConfTarget == 0 {
			b.log.Warnf("updateRbfRate called with zero " +
				"batchConfTarget")
		}

		b.log.Infof("initializing rbf fee rate for conf target=%v",
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

		b.log.Infof("monitoring spend for outpoint %s",
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
				b.writeToErrChan(err)
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
	reorgChan := make(chan struct{})

	confCtx, cancel := context.WithCancel(ctx)

	confChan, errChan, err := b.chainNotifier.RegisterConfirmationsNtfn(
		confCtx, b.batchTxid, b.batchPkScript, batchConfHeight,
		b.currentHeight, lndclient.WithReOrgChan(reorgChan),
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
				b.writeToErrChan(err)
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

	totalFee := int64(totalSweptAmt) - spendTx.TxOut[0].Value
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
	b.batchPkScript = spendTx.TxOut[0].PkScript

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
			delete(b.sweeps, sweep.swapHash)
			purgeList = append(purgeList, SweepRequest{
				SwapHash: newSweep.swapHash,
				Outpoint: newSweep.outpoint,
				Value:    newSweep.value,
				Notifier: newSweep.notifier,
			})
		}
	}

	// Calculate the fee portion that each sweep should pay for the batch.
	feePortionPaidPerSweep, roundingDifference := getFeePortionForSweep(
		spendTx, len(notifyList), totalSweptAmt,
	)

	for _, sweep := range notifyList {
		sweep := sweep
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
		// Iterate over the purge list and feed the sweeps back to the
		// batcher.
		for _, sweep := range purgeList {
			sweep := sweep

			err := b.purger(&sweep)
			if err != nil {
				b.log.Errorf("unable to purge sweep %x:  %v",
					sweep.SwapHash[:6], err)
			}
		}
	}()

	b.log.Infof("spent, total sweeps: %v, purged sweeps: %v",
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
	b.log.Infof("confirmed")
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
	b.log = batchPrefixLogger(fmt.Sprintf("%d", b.id))

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
