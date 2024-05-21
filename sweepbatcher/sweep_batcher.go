package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultMaxTimeoutDistance is the default maximum timeout distance
	// of sweeps that can appear in the same batch.
	defaultMaxTimeoutDistance = 288

	// batchOpen is the string representation of the state of a batch that
	// is open.
	batchOpen = "open"

	// batchClosed is the string representation of the state of a batch
	// that is closed.
	batchClosed = "closed"

	// batchConfirmed is the string representation of the state of a batch
	// that is confirmed.
	batchConfirmed = "confirmed"

	// defaultMainnetPublishDelay is the default publish delay that is used
	// for mainnet.
	defaultMainnetPublishDelay = 5 * time.Second

	// defaultTestnetPublishDelay is the default publish delay that is used
	// for testnet.
	defaultPublishDelay = 500 * time.Millisecond
)

type BatcherStore interface {
	// FetchUnconfirmedSweepBatches fetches all the batches from the
	// database that are not in a confirmed state.
	FetchUnconfirmedSweepBatches(ctx context.Context) ([]*dbBatch, error)

	// InsertSweepBatch inserts a batch into the database, returning the id
	// of the inserted batch.
	InsertSweepBatch(ctx context.Context, batch *dbBatch) (int32, error)

	// DropBatch drops a batch from the database. This should only be used
	// when a batch is empty.
	DropBatch(ctx context.Context, id int32) error

	// UpdateSweepBatch updates a batch in the database.
	UpdateSweepBatch(ctx context.Context, batch *dbBatch) error

	// ConfirmBatch confirms a batch by setting its state to confirmed.
	ConfirmBatch(ctx context.Context, id int32) error

	// FetchBatchSweeps fetches all the sweeps that belong to a batch.
	FetchBatchSweeps(ctx context.Context, id int32) ([]*dbSweep, error)

	// UpsertSweep inserts a sweep into the database, or updates an existing
	// sweep if it already exists.
	UpsertSweep(ctx context.Context, sweep *dbSweep) error

	// GetSweepStatus returns the completed status of the sweep.
	GetSweepStatus(ctx context.Context, swapHash lntypes.Hash) (bool, error)

	// GetParentBatch returns the parent batch of a (completed) sweep.
	GetParentBatch(ctx context.Context, swapHash lntypes.Hash) (*dbBatch,
		error)

	// TotalSweptAmount returns the total amount swept by a (confirmed)
	// batch.
	TotalSweptAmount(ctx context.Context, id int32) (btcutil.Amount, error)
}

// LoopOutFetcher is used to load LoopOut swaps from the database.
// It is implemented by loopdb.SwapStore.
type LoopOutFetcher interface {
	// FetchLoopOutSwap returns the loop out swap with the given hash.
	FetchLoopOutSwap(ctx context.Context,
		hash lntypes.Hash) (*loopdb.LoopOut, error)
}

// MuSig2SignSweep is a function that can be used to sign a sweep transaction
// cooperatively with the swap server.
type MuSig2SignSweep func(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
	prevoutMap map[wire.OutPoint]*wire.TxOut) (
	[]byte, []byte, error)

// VerifySchnorrSig is a function that can be used to verify a schnorr
// signature.
type VerifySchnorrSig func(pubKey *btcec.PublicKey, hash, sig []byte) error

// SweepRequest is a request to sweep a specific outpoint.
type SweepRequest struct {
	// SwapHash is the hash of the swap that is being swept.
	SwapHash lntypes.Hash

	// Outpoint is the outpoint that is being swept.
	Outpoint wire.OutPoint

	// Value is the value of the outpoint that is being swept.
	Value btcutil.Amount

	// Notifier is a notifier that is used to notify the requester of this
	// sweep that the sweep was successful.
	Notifier *SpendNotifier
}

type SpendDetail struct {
	// Tx is the transaction that spent the outpoint.
	Tx *wire.MsgTx

	// OnChainFeePortion is the fee portion that was paid to get this sweep
	// confirmed on chain. This is the difference between the value of the
	// outpoint and the value of all sweeps that were included in the batch
	// divided by the number of sweeps.
	OnChainFeePortion btcutil.Amount
}

// SpendNotifier is a notifier that is used to notify the requester of a sweep
// that the sweep was successful.
type SpendNotifier struct {
	// SpendChan is a channel where the spend details are received.
	SpendChan chan *SpendDetail

	// SpendErrChan is a channel where spend errors are received.
	SpendErrChan chan error

	// QuitChan is a channel that can be closed to stop the notifier.
	QuitChan chan bool
}

var (
	ErrBatcherShuttingDown = errors.New("batcher shutting down")
)

// Batcher is a system that is responsible for accepting sweep requests and
// placing them in appropriate batches. It will spin up new batches as needed.
type Batcher struct {
	// batches is a map of batch IDs to the currently active batches.
	batches map[int32]*batch

	// sweepReqs is a channel where sweep requests are received.
	sweepReqs chan SweepRequest

	// errChan is a channel where errors are received.
	errChan chan error

	// quit signals that the batch must stop.
	quit chan struct{}

	// initDone is a channel that is closed when the batcher has been
	// initialized.
	initDone chan struct{}

	// wallet is the wallet kit client that is used by batches.
	wallet lndclient.WalletKitClient

	// chainNotifier is the chain notifier client that is used by batches.
	chainNotifier lndclient.ChainNotifierClient

	// signerClient is the signer client that is used by batches.
	signerClient lndclient.SignerClient

	// musig2ServerKit includes all the required functionality to collect
	// and verify signatures by the swap server in order to cooperatively
	// sweep funds.
	musig2ServerSign MuSig2SignSweep

	// verifySchnorrSig is a function that can be used to verify a schnorr
	// signature.
	VerifySchnorrSig VerifySchnorrSig

	// chainParams are the chain parameters of the chain that is used by
	// batches.
	chainParams *chaincfg.Params

	// store includes all the database interactions that are needed by the
	// batcher and the batches.
	store BatcherStore

	// swapStore is used to load LoopOut swaps from the database.
	swapStore LoopOutFetcher

	// wg is a waitgroup that is used to wait for all the goroutines to
	// exit.
	wg sync.WaitGroup
}

// NewBatcher creates a new Batcher instance.
func NewBatcher(wallet lndclient.WalletKitClient,
	chainNotifier lndclient.ChainNotifierClient,
	signerClient lndclient.SignerClient, musig2ServerSigner MuSig2SignSweep,
	verifySchnorrSig VerifySchnorrSig, chainparams *chaincfg.Params,
	store BatcherStore, swapStore LoopOutFetcher) *Batcher {

	return &Batcher{
		batches:          make(map[int32]*batch),
		sweepReqs:        make(chan SweepRequest),
		errChan:          make(chan error, 1),
		quit:             make(chan struct{}),
		initDone:         make(chan struct{}),
		wallet:           wallet,
		chainNotifier:    chainNotifier,
		signerClient:     signerClient,
		musig2ServerSign: musig2ServerSigner,
		VerifySchnorrSig: verifySchnorrSig,
		chainParams:      chainparams,
		store:            store,
		swapStore:        swapStore,
	}
}

// Run starts the batcher and processes incoming sweep requests.
func (b *Batcher) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		close(b.quit)

		for _, batch := range b.batches {
			batch.Wait()
		}

		b.wg.Wait()
	}()

	// First we fetch all the batches that are not in a confirmed state from
	// the database. We will then resume the execution of these batches.
	batches, err := b.FetchUnconfirmedBatches(runCtx)
	if err != nil {
		return err
	}

	for _, batch := range batches {
		err := b.spinUpBatchFromDB(runCtx, batch)
		if err != nil {
			return err
		}
	}

	// Signal that the batcher has been initialized.
	close(b.initDone)

	for {
		select {
		case sweepReq := <-b.sweepReqs:
			sweep, err := b.fetchSweep(runCtx, sweepReq)
			if err != nil {
				return err
			}

			err = b.handleSweep(runCtx, sweep, sweepReq.Notifier)
			if err != nil {
				return err
			}

		case err := <-b.errChan:
			return err

		case <-runCtx.Done():
			return runCtx.Err()
		}
	}
}

// AddSweep adds a sweep request to the batcher for handling. This will either
// place the sweep in an existing batch or create a new one.
func (b *Batcher) AddSweep(sweepReq *SweepRequest) error {
	select {
	case b.sweepReqs <- *sweepReq:
		return nil

	case <-b.quit:
		return ErrBatcherShuttingDown
	}
}

// handleSweep handles a sweep request by either placing it in an existing
// batch, or by spinning up a new batch for it.
func (b *Batcher) handleSweep(ctx context.Context, sweep *sweep,
	notifier *SpendNotifier) error {

	completed, err := b.store.GetSweepStatus(ctx, sweep.swapHash)
	if err != nil {
		return err
	}

	log.Infof("Batcher handling sweep %x, completed=%v", sweep.swapHash[:6],
		completed)

	// If the sweep has already been completed in a confirmed batch then we
	// can't attach its notifier to the batch as that is no longer running.
	// Instead we directly detect and return the spend here.
	if completed && *notifier != (SpendNotifier{}) {
		return b.monitorSpendAndNotify(ctx, sweep, notifier)
	}

	sweep.notifier = notifier

	// Check if the sweep is already in a batch. If that is the case, we
	// provide the sweep to that batch and return.
	for _, batch := range b.batches {
		// This is a check to see if a batch is completed. In that case
		// we just lazily delete it and continue our scan.
		if batch.isComplete() {
			delete(b.batches, batch.id)
			continue
		}

		if batch.sweepExists(sweep.swapHash) {
			accepted, err := batch.addSweep(ctx, sweep)
			if err != nil && !errors.Is(err, ErrBatchShuttingDown) {
				return err
			}

			if !accepted {
				return fmt.Errorf("existing sweep %x was not "+
					"accepted by batch %d", sweep.swapHash[:6],
					batch.id)
			}

			// The sweep was updated in the batch, our job is done.
			return nil
		}
	}

	// If one of the batches accepts the sweep, we provide it to that batch.
	for _, batch := range b.batches {
		accepted, err := batch.addSweep(ctx, sweep)
		if err != nil && !errors.Is(err, ErrBatchShuttingDown) {
			return err
		}

		// If the sweep was accepted by this batch, we return, our job
		// is done.
		if accepted {
			return nil
		}
	}

	// If no batch is capable of accepting the sweep, we spin up a fresh
	// batch and hand the sweep over to it.
	batch, err := b.spinUpBatch(ctx)
	if err != nil {
		return err
	}

	// Add the sweep to the fresh batch.
	accepted, err := batch.addSweep(ctx, sweep)
	if err != nil {
		return err
	}

	// If the sweep wasn't accepted by the fresh batch something is wrong,
	// we should return the error.
	if !accepted {
		return fmt.Errorf("sweep %x was not accepted by new batch %d",
			sweep.swapHash[:6], batch.id)
	}

	return nil
}

// spinUpBatch spins up a new batch and returns it.
func (b *Batcher) spinUpBatch(ctx context.Context) (*batch, error) {
	cfg := batchConfig{
		maxTimeoutDistance: defaultMaxTimeoutDistance,
		batchConfTarget:    defaultBatchConfTarget,
	}

	switch b.chainParams {
	case &chaincfg.MainNetParams:
		cfg.batchPublishDelay = defaultMainnetPublishDelay

	default:
		cfg.batchPublishDelay = defaultPublishDelay
	}

	batchKit := batchKit{
		returnChan:       b.sweepReqs,
		wallet:           b.wallet,
		chainNotifier:    b.chainNotifier,
		signerClient:     b.signerClient,
		musig2SignSweep:  b.musig2ServerSign,
		verifySchnorrSig: b.VerifySchnorrSig,
		purger:           b.AddSweep,
		store:            b.store,
		quit:             b.quit,
	}

	batch := NewBatch(cfg, batchKit)

	id, err := batch.insertAndAcquireID(ctx)
	if err != nil {
		return nil, err
	}

	// We add the batch to our map of batches and start it.
	b.batches[id] = batch

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()

		err := batch.Run(ctx)
		if err != nil {
			_ = b.writeToErrChan(ctx, err)
		}
	}()

	return batch, nil
}

// spinUpBatchDB spins up a batch that already existed in storage, then
// returns it.
func (b *Batcher) spinUpBatchFromDB(ctx context.Context, batch *batch) error {
	dbSweeps, err := b.store.FetchBatchSweeps(ctx, batch.id)
	if err != nil {
		return err
	}

	if len(dbSweeps) == 0 {
		log.Infof("skipping restored batch %d as it has no sweeps",
			batch.id)

		// It is safe to drop this empty batch as it has no sweeps.
		err := b.store.DropBatch(ctx, batch.id)
		if err != nil {
			log.Warnf("unable to drop empty batch %d: %v",
				batch.id, err)
		}

		return nil
	}

	primarySweep := dbSweeps[0]

	sweeps := make(map[lntypes.Hash]sweep)

	for _, dbSweep := range dbSweeps {
		sweep, err := b.convertSweep(dbSweep)
		if err != nil {
			return err
		}

		sweeps[sweep.swapHash] = *sweep
	}

	rbfCache := rbfCache{
		LastHeight: batch.rbfCache.LastHeight,
		FeeRate:    batch.rbfCache.FeeRate,
	}

	batchKit := batchKit{
		id:               batch.id,
		batchTxid:        batch.batchTxid,
		batchPkScript:    batch.batchPkScript,
		state:            batch.state,
		primaryID:        primarySweep.SwapHash,
		sweeps:           sweeps,
		rbfCache:         rbfCache,
		returnChan:       b.sweepReqs,
		wallet:           b.wallet,
		chainNotifier:    b.chainNotifier,
		signerClient:     b.signerClient,
		musig2SignSweep:  b.musig2ServerSign,
		verifySchnorrSig: b.VerifySchnorrSig,
		purger:           b.AddSweep,
		store:            b.store,
		log:              batchPrefixLogger(fmt.Sprintf("%d", batch.id)),
		quit:             b.quit,
	}

	cfg := batchConfig{
		maxTimeoutDistance: batch.cfg.maxTimeoutDistance,
		batchConfTarget:    defaultBatchConfTarget,
	}

	newBatch := NewBatchFromDB(cfg, batchKit)

	// We add the batch to our map of batches and start it.
	b.batches[batch.id] = newBatch

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()

		err := newBatch.Run(ctx)
		if err != nil {
			_ = b.writeToErrChan(ctx, err)
		}
	}()

	return nil
}

// FetchUnconfirmedBatches fetches all the batches from the database that are
// not in a confirmed state.
func (b *Batcher) FetchUnconfirmedBatches(ctx context.Context) ([]*batch,
	error) {

	dbBatches, err := b.store.FetchUnconfirmedSweepBatches(ctx)
	if err != nil {
		return nil, err
	}

	batches := make([]*batch, 0, len(dbBatches))
	for _, bch := range dbBatches {
		bch := bch

		batch := batch{}
		batch.id = bch.ID

		switch bch.State {
		case batchOpen:
			batch.state = Open

		case batchClosed:
			batch.state = Closed

		case batchConfirmed:
			batch.state = Confirmed
		}

		batch.batchTxid = &bch.BatchTxid
		batch.batchPkScript = bch.BatchPkScript

		rbfCache := rbfCache{
			LastHeight: bch.LastRbfHeight,
			FeeRate:    chainfee.SatPerKWeight(bch.LastRbfSatPerKw),
		}
		batch.rbfCache = rbfCache

		bchCfg := batchConfig{
			maxTimeoutDistance: bch.MaxTimeoutDistance,
		}
		batch.cfg = &bchCfg

		batches = append(batches, &batch)
	}

	return batches, nil
}

// monitorSpendAndNotify monitors the spend of a specific outpoint and writes
// the response back to the response channel.
func (b *Batcher) monitorSpendAndNotify(ctx context.Context, sweep *sweep,
	notifier *SpendNotifier) error {

	spendCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// First get the batch that completed the sweep.
	parentBatch, err := b.store.GetParentBatch(ctx, sweep.swapHash)
	if err != nil {
		return err
	}

	// Then we get the total amount that was swept by the batch.
	totalSwept, err := b.store.TotalSweptAmount(ctx, parentBatch.ID)
	if err != nil {
		return err
	}

	spendChan, spendErr, err := b.chainNotifier.RegisterSpendNtfn(
		spendCtx, &sweep.outpoint, sweep.htlc.PkScript,
		sweep.initiationHeight,
	)
	if err != nil {
		return err
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		log.Infof("Batcher monitoring spend for swap %x",
			sweep.swapHash[:6])

		for {
			select {
			case spend := <-spendChan:
				spendTx := spend.SpendingTx
				// Calculate the fee portion that each sweep
				// should pay for the batch.
				feePortionPerSweep, roundingDifference :=
					getFeePortionForSweep(
						spendTx, len(spendTx.TxIn),
						totalSwept,
					)

				// Notify the requester of the spend
				// with the spend details, including the fee
				// portion for this particular sweep.
				spendDetail := &SpendDetail{
					Tx: spendTx,
					OnChainFeePortion: getFeePortionPaidBySweep( // nolint:lll
						spendTx, feePortionPerSweep,
						roundingDifference, sweep,
					),
				}

				select {
				case notifier.SpendChan <- spendDetail:
				case <-ctx.Done():
				}

				return

			case err := <-spendErr:
				select {
				case notifier.SpendErrChan <- err:
				case <-ctx.Done():
				}

				_ = b.writeToErrChan(ctx, err)
				return

			case <-notifier.QuitChan:
				return

			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

func (b *Batcher) writeToErrChan(ctx context.Context, err error) error {
	select {
	case b.errChan <- err:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// convertSweep converts a fetched sweep from the database to a sweep that is
// ready to be processed by the batcher.
func (b *Batcher) convertSweep(dbSweep *dbSweep) (*sweep, error) {
	swap := dbSweep.LoopOut

	htlc, err := utils.GetHtlc(
		dbSweep.SwapHash, &swap.Contract.SwapContract, b.chainParams,
	)
	if err != nil {
		return nil, err
	}

	swapPaymentAddr, err := utils.ObtainSwapPaymentAddr(
		swap.Contract.SwapInvoice, b.chainParams,
	)
	if err != nil {
		return nil, err
	}

	return &sweep{
		swapHash:               swap.Hash,
		outpoint:               dbSweep.Outpoint,
		value:                  dbSweep.Amount,
		confTarget:             swap.Contract.SweepConfTarget,
		timeout:                swap.Contract.CltvExpiry,
		initiationHeight:       swap.Contract.InitiationHeight,
		htlc:                   *htlc,
		preimage:               swap.Contract.Preimage,
		swapInvoicePaymentAddr: *swapPaymentAddr,
		htlcKeys:               swap.Contract.HtlcKeys,
		htlcSuccessEstimator:   htlc.AddSuccessToEstimator,
		protocolVersion:        swap.Contract.ProtocolVersion,
		isExternalAddr:         swap.Contract.IsExternalAddr,
		destAddr:               swap.Contract.DestAddr,
	}, nil
}

// fetchSweep fetches the sweep related information from the database.
func (b *Batcher) fetchSweep(ctx context.Context,
	sweepReq SweepRequest) (*sweep, error) {

	swapHash, err := lntypes.MakeHash(sweepReq.SwapHash[:])
	if err != nil {
		return nil, fmt.Errorf("failed to parse swapHash: %v", err)
	}

	swap, err := b.swapStore.FetchLoopOutSwap(ctx, swapHash)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch loop out for %x: %v",
			swapHash[:6], err)
	}

	htlc, err := utils.GetHtlc(
		swapHash, &swap.Contract.SwapContract, b.chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get htlc: %v", err)
	}

	swapPaymentAddr, err := utils.ObtainSwapPaymentAddr(
		swap.Contract.SwapInvoice, b.chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get payment addr: %v", err)
	}

	return &sweep{
		swapHash:               swap.Hash,
		outpoint:               sweepReq.Outpoint,
		value:                  sweepReq.Value,
		confTarget:             swap.Contract.SweepConfTarget,
		timeout:                swap.Contract.CltvExpiry,
		initiationHeight:       swap.Contract.InitiationHeight,
		htlc:                   *htlc,
		preimage:               swap.Contract.Preimage,
		swapInvoicePaymentAddr: *swapPaymentAddr,
		htlcKeys:               swap.Contract.HtlcKeys,
		htlcSuccessEstimator:   htlc.AddSuccessToEstimator,
		protocolVersion:        swap.Contract.ProtocolVersion,
		isExternalAddr:         swap.Contract.IsExternalAddr,
		destAddr:               swap.Contract.DestAddr,
	}, nil
}
