package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultMaxTimeoutDistance is the default maximum timeout distance
	// of sweeps that can appear in the same batch.
	defaultMaxTimeoutDistance = 288

	// defaultMainnetPublishDelay is the default publish delay that is used
	// for mainnet.
	defaultMainnetPublishDelay = 5 * time.Second

	// defaultTestnetPublishDelay is the default publish delay that is used
	// for testnet.
	defaultTestnetPublishDelay = 500 * time.Millisecond
)

type BatcherStore interface {
	// FetchUnconfirmedSweepBatches fetches all the batches from the
	// database that are not in a confirmed state.
	FetchUnconfirmedSweepBatches(ctx context.Context) ([]*dbBatch, error)

	// InsertSweepBatch inserts a batch into the database, returning the id
	// of the inserted batch.
	InsertSweepBatch(ctx context.Context, batch *dbBatch) (int32, error)

	// CancelBatch marks a batch as cancelled in the database. Note that we
	// only use this call for batches that have no sweeps or all the sweeps
	// are in skipped transaction and so we'd not be able to resume.
	CancelBatch(ctx context.Context, id int32) error

	// UpdateSweepBatch updates a batch in the database.
	UpdateSweepBatch(ctx context.Context, batch *dbBatch) error

	// FetchBatchSweeps fetches all the sweeps that belong to a batch.
	FetchBatchSweeps(ctx context.Context, id int32) ([]*dbSweep, error)

	// UpsertSweep inserts a sweep into the database, or updates an existing
	// sweep if it already exists.
	UpsertSweep(ctx context.Context, sweep *dbSweep) error

	// GetSweepStatus returns the completed status of the sweep.
	GetSweepStatus(ctx context.Context,
		outpoint wire.OutPoint) (bool, error)

	// GetParentBatch returns the parent batch of a (completed) sweep.
	GetParentBatch(ctx context.Context,
		outpoint wire.OutPoint) (*dbBatch, error)

	// TotalSweptAmount returns the total amount swept by a (confirmed)
	// batch.
	TotalSweptAmount(ctx context.Context, id int32) (btcutil.Amount, error)
}

// SweepInfo stores any data related to sweeping a specific outpoint.
type SweepInfo struct {
	// ConfTarget is the confirmation target of the sweep.
	ConfTarget int32

	// Timeout is the timeout of the swap that the sweep belongs to.
	Timeout int32

	// InitiationHeight is the height at which the swap was initiated.
	InitiationHeight int32

	// HTLC is the HTLC that is being swept.
	HTLC swap.Htlc

	// Preimage is the preimage of the HTLC that is being swept.
	Preimage lntypes.Preimage

	// SwapInvoicePaymentAddr is the payment address of the swap invoice.
	SwapInvoicePaymentAddr [32]byte

	// HTLCKeys is the set of keys used to sign the HTLC.
	HTLCKeys loopdb.HtlcKeys

	// HTLCSuccessEstimator is a function that estimates the weight of the
	// HTLC success script.
	HTLCSuccessEstimator func(*input.TxWeightEstimator) error

	// ProtocolVersion is the protocol version of the swap that the sweep
	// belongs to.
	ProtocolVersion loopdb.ProtocolVersion

	// IsExternalAddr is true if the sweep spends to a non-wallet address.
	IsExternalAddr bool

	// DestAddr is the destination address of the sweep.
	DestAddr btcutil.Address

	// NonCoopHint is set, if the sweep can not be spent cooperatively and
	// has to be spent using preimage. This is only used in fee estimations
	// when selecting a batch for the sweep to minimize fees.
	NonCoopHint bool

	// IsPresigned stores if presigned mode is enabled for the sweep. This
	// value should be stable for a sweep. Currently presigned and
	// non-presigned sweeps never appear in the same batch.
	IsPresigned bool

	// Change is an optional change output of the sweep.
	Change *wire.TxOut
}

// SweepFetcher is used to get details of a sweep.
type SweepFetcher interface {
	// FetchSweep returns details of the sweep with the given hash or
	// outpoint. The outpoint is used if hash is not unique.
	FetchSweep(ctx context.Context, hash lntypes.Hash,
		outpoint wire.OutPoint) (*SweepInfo, error)
}

// MuSig2SignSweep is a function that can be used to sign a sweep transaction
// cooperatively with the swap server.
type MuSig2SignSweep func(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, nonce []byte, sweepTxPsbt []byte,
	prevoutMap map[wire.OutPoint]*wire.TxOut) (
	[]byte, []byte, error)

// SignMuSig2 is a function that can be used to sign a sweep transaction in a
// custom way.
type SignMuSig2 func(ctx context.Context, muSig2Version input.MuSig2Version,
	swapHash lntypes.Hash, rootHash chainhash.Hash, sigHash [32]byte,
) ([]byte, error)

// PresignedHelper provides methods used when batches are presigned in advance.
// In this mode sweepbatcher uses transactions provided by PresignedHelper,
// which are pre-signed. The helper also memorizes transactions it previously
// produced. It also affects batch selection: presigned inputs and regular
// (non-presigned) inputs never appear in the same batch. Also if presigning
// fails (e.g. because one of the inputs is offline), an input can't be added to
// a batch.
type PresignedHelper interface {
	// DestPkScript returns destination pkScript used by the sweep batch
	// with the primary outpoint specified. Returns an error, if such tx
	// doesn't exist. If there are many such transactions, returns any of
	// pkScript's; all of them should have the same destination pkScript.
	// TODO: embed this data into SweepInfo.
	DestPkScript(ctx context.Context,
		primarySweepID wire.OutPoint) ([]byte, error)

	// SignTx signs an unsigned transaction or returns a pre-signed tx.
	// It must satisfy the following invariants:
	//   - the set of inputs is the same, though the order may change;
	//   - the main output is the same, but its amount may be different;
	//   - the main output is the first output in the transaction;
	//   - an optional set of change outputs may be added, the values and
	//     pkscripts must be preserved.
	//   - feerate is higher or equal to minRelayFee;
	//   - LockTime may be decreased;
	//   - transaction version must be the same;
	//   - witness must not be empty;
	//   - Sequence numbers in the inputs must be preserved.
	// When choosing a presigned transaction, a transaction with fee rate
	// closer to the fee rate passed is selected. If loadOnly is set, it
	// doesn't try to sign the transaction and only loads a presigned tx.
	// These rules are enforced by CheckSignedTx function.
	SignTx(ctx context.Context, primarySweepID wire.OutPoint,
		tx *wire.MsgTx, inputAmt btcutil.Amount,
		minRelayFee, feeRate chainfee.SatPerKWeight,
		loadOnly bool) (*wire.MsgTx, error)

	// CleanupTransactions removes all transactions related to any of the
	// outpoints. Should be called after sweep batch tx is reorg-safely
	// confirmed.
	CleanupTransactions(ctx context.Context, inputs []wire.OutPoint) error
}

// VerifySchnorrSig is a function that can be used to verify a schnorr
// signature.
type VerifySchnorrSig func(pubKey *btcec.PublicKey, hash, sig []byte) error

// FeeRateProvider is a function that returns min fee rate of a batch sweeping
// the UTXO of the swap.
type FeeRateProvider func(ctx context.Context, swapHash lntypes.Hash,
	utxo wire.OutPoint) (chainfee.SatPerKWeight, error)

// InitialDelayProvider returns the duration after which a newly created batch
// is first published. It allows to customize the duration based on total value
// of the batch. There is a trade-off between better grouping and getting funds
// faster. If the function returns an error, no delay is used and the error is
// logged as a warning.
type InitialDelayProvider func(ctx context.Context, numSweeps int,
	value btcutil.Amount, fast bool) (time.Duration, error)

// zeroInitialDelay returns no delay for any sweeps.
func zeroInitialDelay(_ context.Context, _ int,
	_ btcutil.Amount, _ bool) (time.Duration, error) {

	return 0, nil
}

// PublishErrorHandler is a function that handles transaction publishing error.
type PublishErrorHandler func(err error, errMsg string, log btclog.Logger)

// defaultPublishErrorLogger is an instance of PublishErrorHandler which logs
// all errors as warnings, but "insufficient fee" as info (since they are
// expected, if RBF fails).
func defaultPublishErrorLogger(err error, errMsg string, log btclog.Logger) {
	// Check if the error is "insufficient fee" error.
	if strings.Contains(err.Error(), chain.ErrInsufficientFee.Error()) {
		// Log "insufficient fee" with level Info.
		log.Infof("%s: %v", errMsg, err)

		return
	}

	// Log any other error as a warning.
	log.Warnf("%s: %v", errMsg, err)
}

// Input specifies an UTXO with amount that is added to the batcher.
type Input struct {
	// Outpoint is the outpoint that is being swept.
	Outpoint wire.OutPoint

	// Value is the value of the outpoint that is being swept.
	Value btcutil.Amount
}

// SweepRequest is a request to sweep an outpoint or a group of outpoints.
type SweepRequest struct {
	// SwapHash is the hash of the swap that is being swept.
	SwapHash lntypes.Hash

	// Inputs specifies the inputs in the same request. All the inputs
	// belong to the same swap and are added to the same batch.
	Inputs []Input

	// Notifier is a notifier that is used to notify the requester of this
	// sweep that the sweep was successful.
	Notifier *SpendNotifier

	// Fast is set by the client if the sweep should be published
	// immediately.
	Fast bool
}

// addSweepsRequest is a request to sweep an outpoint or a group of outpoints
// that is used internally by the batcher (between AddSweep and handleSweeps).
type addSweepsRequest struct {
	// sweeps is the list of sweeps already loaded from DB and fee rate
	// source.
	sweeps []*sweep

	// Notifier is a notifier that is used to notify the requester of this
	// sweep that the sweep was successful.
	notifier *SpendNotifier

	// fast indicates sweeps that are part of a fast swap.
	fast bool
}

// SpendDetail is a notification that is send to the user of sweepbatcher when
// a batch gets the first confirmation.
type SpendDetail struct {
	// Tx is the transaction that spent the outpoint.
	Tx *wire.MsgTx

	// OnChainFeePortion is the fee portion that was paid to get this sweep
	// confirmed on chain. This is the difference between the value of the
	// outpoint and the value of all sweeps that were included in the batch
	// divided by the number of sweeps.
	OnChainFeePortion btcutil.Amount
}

// ConfDetail is a notification that is send to the user of sweepbatcher when
// a batch is reorg-safely confirmed, i.e. gets batchConfHeight confirmations.
type ConfDetail struct {
	// TxConfirmation has data about the confirmation of the transaction.
	*chainntnfs.TxConfirmation

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
	SpendChan chan<- *SpendDetail

	// SpendErrChan is a channel where spend errors are received.
	SpendErrChan chan<- error

	// ConfChan is a channel where the confirmation details are received.
	// This channel is optional.
	ConfChan chan<- *ConfDetail

	// ConfErrChan is a channel where confirmation errors are received.
	// This channel is optional.
	ConfErrChan chan<- error

	// QuitChan is a channel that can be closed to stop the notifier.
	QuitChan <-chan bool
}

var (
	ErrBatcherShuttingDown = errors.New("batcher shutting down")
)

// testRequest is a function passed to an event loop and a channel used to
// wait until the function is executed. This is used in unit tests only!
type testRequest struct {
	// handler is the function to an event loop.
	handler func()

	// quit is closed when the handler completes.
	quit chan struct{}
}

// Batcher is a system that is responsible for accepting sweep requests and
// placing them in appropriate batches. It will spin up new batches as needed.
type Batcher struct {
	// batches is a map of batch IDs to the currently active batches.
	batches map[int32]*batch

	// addSweepsChan is a channel where sweep requests are received.
	addSweepsChan chan *addSweepsRequest

	// testReqs is a channel where test requests are received.
	// This is used only in unit tests! The reason to have this is to
	// avoid data races in require.Eventually calls running in parallel
	// to the event loop. See method testRunInEventLoop().
	testReqs chan *testRequest

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

	// musig2ServerSign includes all the required functionality to collect
	// and verify signatures by the swap server in order to cooperatively
	// sweep funds.
	musig2ServerSign MuSig2SignSweep

	// VerifySchnorrSig is a function that can be used to verify a schnorr
	// signature.
	VerifySchnorrSig VerifySchnorrSig

	// chainParams are the chain parameters of the chain that is used by
	// batches.
	chainParams *chaincfg.Params

	// store includes all the database interactions that are needed by the
	// batcher and the batches.
	store BatcherStore

	// sweepStore is used to load sweeps from the database.
	sweepStore SweepFetcher

	// wg is a waitgroup that is used to wait for all the goroutines to
	// exit.
	wg sync.WaitGroup

	// clock provides methods to work with time and timers.
	clock clock.Clock

	// initialDelayProvider provides the delay of first batch publishing
	// after creation. It only affects newly created batches, not batches
	// loaded from DB, so publishing does happen in case of a daemon restart
	// (especially important in case of a crashloop). If a sweep is about to
	// expire (time until timeout is less that 2x initialDelay), then
	// waiting is skipped.
	initialDelayProvider InitialDelayProvider

	// publishDelay is the delay of batch publishing that is applied in the
	// beginning, after the appearance of a new block in the network or
	// after the end of initial delay. For batches recovered from DB this
	// value is always 0s, regardless of this setting.
	publishDelay time.Duration

	// customFeeRate provides custom min fee rate per swap. The batch uses
	// max of the fee rates of its swaps. In this mode confTarget is
	// ignored and fee bumping by sweepbatcher is disabled.
	customFeeRate FeeRateProvider

	// txLabeler is a function generating a transaction label. It is called
	// before publishing a batch transaction. Batch ID is passed to it.
	txLabeler func(batchID int32) string

	// customMuSig2Signer is a custom signer. If it is set, it is used to
	// create musig2 signatures instead of musig2SignSweep and signerClient.
	// Note that musig2SignSweep must be nil in this case, however signer
	// client must still be provided, as it is used for non-coop spendings.
	customMuSig2Signer SignMuSig2

	// publishErrorHandler is a function that handles transaction publishing
	// error. By default, it logs all errors as warnings, but "insufficient
	// fee" as Info.
	publishErrorHandler PublishErrorHandler

	// presignedHelper provides methods used when presigned batches are
	// enabled.
	presignedHelper PresignedHelper

	// skippedTxns is the list of previous transactions to ignore when
	// loading the sweeps from DB. This is needed to fix a historical bug.
	skippedTxns map[chainhash.Hash]struct{}
}

// BatcherConfig holds batcher configuration.
type BatcherConfig struct {
	// clock provides methods to work with time and timers.
	clock clock.Clock

	// initialDelayProvider provides the delay of first batch publishing
	// after creation. It only affects newly created batches, not batches
	// loaded from DB, so publishing does happen in case of a daemon restart
	// (especially important in case of a crashloop). If a sweep is about to
	// expire (time until timeout is less that 2x initialDelay), then
	// waiting is skipped.
	initialDelayProvider InitialDelayProvider

	// publishDelay is the delay of batch publishing that is applied in the
	// beginning, after the appearance of a new block in the network or
	// after the end of initial delay. For batches recovered from DB this
	// value is always 0s, regardless of this setting.
	publishDelay time.Duration

	// customFeeRate provides custom min fee rate per swap. The batch uses
	// max of the fee rates of its swaps. In this mode confTarget is
	// ignored and fee bumping by sweepbatcher is disabled.
	customFeeRate FeeRateProvider

	// txLabeler is a function generating a transaction label. It is called
	// before publishing a batch transaction. Batch ID is passed to it.
	txLabeler func(batchID int32) string

	// customMuSig2Signer is a custom signer. If it is set, it is used to
	// create musig2 signatures instead of musig2SignSweep and signerClient.
	// Note that musig2SignSweep must be nil in this case, however signer
	// client must still be provided, as it is used for non-coop spendings.
	customMuSig2Signer SignMuSig2

	// publishErrorHandler is a function that handles transaction publishing
	// error. By default, it logs all errors as warnings, but "insufficient
	// fee" as Info.
	publishErrorHandler PublishErrorHandler

	// presignedHelper provides methods used when presigned batches are
	// enabled.
	presignedHelper PresignedHelper

	// skippedTxns is the list of previous transactions to ignore when
	// loading the sweeps from DB. This is needed to fix a historical bug.
	skippedTxns map[chainhash.Hash]struct{}
}

// BatcherOption configures batcher behaviour.
type BatcherOption func(*BatcherConfig)

// WithClock sets the clock used by sweepbatcher and its batches. It is needed
// to manipulate time in tests.
func WithClock(clock clock.Clock) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.clock = clock
	}
}

// WithInitialDelay instructs sweepbatcher to wait for the duration provided
// after new batch creation before it is first published. This facilitates
// better grouping. Defaults to 0s (no initial delay). If a sweep is about
// to expire (time until timeout is less that 2x initialDelay), then waiting
// is skipped.
func WithInitialDelay(provider InitialDelayProvider) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.initialDelayProvider = provider
	}
}

// WithPublishDelay sets the delay of batch publishing that is applied in the
// beginning, after the appearance of a new block in the network or after the
// end of initial delay (see WithInitialDelay). It is needed to prevent
// unnecessary transaction publishments when a spend is detected on that block.
// Default value depends on the network: 5 seconds in mainnet, 0.5s in testnet.
// For batches recovered from DB this value is always 0s.
func WithPublishDelay(publishDelay time.Duration) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.publishDelay = publishDelay
	}
}

// WithCustomFeeRate instructs sweepbatcher not to fee bump itself and rely on
// external source of fee rates (FeeRateProvider). To apply a fee rate change,
// the caller should re-add the sweep by calling AddSweep.
func WithCustomFeeRate(customFeeRate FeeRateProvider) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.customFeeRate = customFeeRate
	}
}

// WithTxLabeler sets a function generating a transaction label. It is called
// before publishing a batch transaction. Batch ID is passed to the function.
// By default, loop/labels.LoopOutBatchSweepSuccess is used.
func WithTxLabeler(txLabeler func(batchID int32) string) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.txLabeler = txLabeler
	}
}

// WithCustomSignMuSig2 instructs sweepbatcher to use a custom function to
// produce MuSig2 signatures. If it is set, it is used to create
// musig2 signatures instead of musig2SignSweep and signerClient. Note
// that musig2SignSweep must be nil in this case, however signerClient
// must still be provided, as it is used for non-coop spendings.
func WithCustomSignMuSig2(customMuSig2Signer SignMuSig2) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.customMuSig2Signer = customMuSig2Signer
	}
}

// WithPublishErrorHandler sets the callback used to handle publish errors.
// It can be used to filter out noisy messages.
func WithPublishErrorHandler(handler PublishErrorHandler) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.publishErrorHandler = handler
	}
}

// WithPresignedHelper enables presigned batches in the batcher. When a sweep
// intended for presigning is added, it must be first passed to the
// PresignSweepsGroup method, before first call of the AddSweep method.
func WithPresignedHelper(presignedHelper PresignedHelper) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.presignedHelper = presignedHelper
	}
}

// WithSkippedTxns is the list of previous transactions to ignore when
// loading the sweeps from DB. This is needed to fix a historical bug.
func WithSkippedTxns(skippedTxns map[chainhash.Hash]struct{}) BatcherOption {
	return func(cfg *BatcherConfig) {
		cfg.skippedTxns = skippedTxns
	}
}

// NewBatcher creates a new Batcher instance.
func NewBatcher(wallet lndclient.WalletKitClient,
	chainNotifier lndclient.ChainNotifierClient,
	signerClient lndclient.SignerClient, musig2ServerSigner MuSig2SignSweep,
	verifySchnorrSig VerifySchnorrSig, chainparams *chaincfg.Params,
	store BatcherStore, sweepStore SweepFetcher,
	opts ...BatcherOption) *Batcher {

	badTx1, err := chainhash.NewHashFromStr(
		"7028bdac753a254785d29506f311abcda323706b531345105f38999" +
			"aecd6f3d1",
	)
	if err != nil {
		panic(err)
	}

	cfg := BatcherConfig{
		// By default, loop/labels.LoopOutBatchSweepSuccess is used
		// to label sweep transactions.
		txLabeler: labels.LoopOutBatchSweepSuccess,

		// publishErrorHandler is a function that handles transaction
		// publishing error. By default, it logs all errors as warnings,
		// but "insufficient fee" as Info.
		publishErrorHandler: defaultPublishErrorLogger,

		skippedTxns: map[chainhash.Hash]struct{}{
			*badTx1: {},
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// If WithClock was not provided, use default clock.
	if cfg.clock == nil {
		cfg.clock = clock.NewDefaultClock()
	}

	if cfg.customMuSig2Signer != nil && musig2ServerSigner != nil {
		panic("customMuSig2Signer must not be used with " +
			"musig2ServerSigner")
	}

	return &Batcher{
		batches:              make(map[int32]*batch),
		addSweepsChan:        make(chan *addSweepsRequest),
		testReqs:             make(chan *testRequest),
		errChan:              make(chan error, 1),
		quit:                 make(chan struct{}),
		initDone:             make(chan struct{}),
		wallet:               wallet,
		chainNotifier:        chainNotifier,
		signerClient:         signerClient,
		musig2ServerSign:     musig2ServerSigner,
		VerifySchnorrSig:     verifySchnorrSig,
		chainParams:          chainparams,
		store:                store,
		sweepStore:           sweepStore,
		clock:                cfg.clock,
		initialDelayProvider: cfg.initialDelayProvider,
		publishDelay:         cfg.publishDelay,
		customFeeRate:        cfg.customFeeRate,
		txLabeler:            cfg.txLabeler,
		customMuSig2Signer:   cfg.customMuSig2Signer,
		publishErrorHandler:  cfg.publishErrorHandler,
		presignedHelper:      cfg.presignedHelper,
		skippedTxns:          cfg.skippedTxns,
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
		case req := <-b.addSweepsChan:
			err = b.handleSweeps(
				runCtx, req.sweeps, req.notifier, req.fast,
			)
			if err != nil {
				warnf("handleSweeps failed: %v.", err)

				return err
			}

		case testReq := <-b.testReqs:
			testReq.handler()
			close(testReq.quit)

		case err := <-b.errChan:
			warnf("Batcher received an error: %v.", err)

			return err

		case <-runCtx.Done():
			infof("Stopping Batcher: run context cancelled.")

			return runCtx.Err()
		}
	}
}

// PresignSweepsGroup creates and stores presigned transactions for the sweeps
// group. This method must be called prior to AddSweep if presigned mode is
// enabled, otherwise AddSweep will fail. All the sweeps must belong to the same
// swap. The order of sweeps is important. The first sweep serves as
// primarySweepID if the group starts a new batch. The change output may be nil
// to indicate that the sweep group does not create a change output.
func (b *Batcher) PresignSweepsGroup(ctx context.Context, inputs []Input,
	sweepTimeout int32, destAddress btcutil.Address,
	changeOutput *wire.TxOut) error {

	if len(inputs) == 0 {
		return fmt.Errorf("no inputs passed to PresignSweepsGroup")
	}
	if b.presignedHelper == nil {
		return fmt.Errorf("presignedHelper is not installed")
	}

	// Find the feerate needed to get into next block. Use conf_target=2,
	nextBlockFeeRate, err := b.wallet.EstimateFeeRate(ctx, 2)
	if err != nil {
		return fmt.Errorf("failed to get nextBlockFeeRate: %w", err)
	}
	minRelayFeeRate, err := b.wallet.MinRelayFee(ctx)
	if err != nil {
		return fmt.Errorf("failed to get minRelayFeeRate: %w", err)
	}
	destPkscript, err := txscript.PayToAddrScript(destAddress)
	if err != nil {
		return fmt.Errorf("txscript.PayToAddrScript failed: %w", err)
	}
	infof("PresignSweepsGroup: nextBlockFeeRate is %v, inputs: %v, "+
		"destAddress: %v, destPkscript: %x sweepTimeout: %d",
		nextBlockFeeRate, inputs, destAddress, destPkscript,
		sweepTimeout)

	sweeps := make([]sweep, len(inputs))
	for i, input := range inputs {
		sweeps[i] = sweep{
			outpoint: input.Outpoint,
			value:    input.Value,
			timeout:  sweepTimeout,
		}
	}

	// Set the change output on the primary group sweep.
	sweeps[0].change = changeOutput

	// The sweeps are ordered inside the group, the first one is the primary
	// outpoint in the batch.
	primarySweepID := sweeps[0].outpoint

	return presign(
		ctx, b.presignedHelper, destAddress, primarySweepID, sweeps,
		nextBlockFeeRate, minRelayFeeRate,
	)
}

// AddSweep loads information about sweeps from the store and fee rate source,
// and adds them to the batcher for handling. This will either place the sweep
// in an existing batch or create a new one. The method can be called multiple
// times, but the sweeps (including the order of them) must be the same. If
// notifier is provided, the batcher sends back sweeping results through it.
func (b *Batcher) AddSweep(ctx context.Context, sweepReq *SweepRequest) error {
	// If the batcher is shutting down, quit now.
	select {
	case <-b.quit:
		return ErrBatcherShuttingDown

	default:
	}

	sweeps, err := b.fetchSweeps(ctx, *sweepReq)
	if err != nil {
		return fmt.Errorf("fetchSweeps failed: %w", err)
	}

	if len(sweeps) == 0 {
		return fmt.Errorf("trying to add an empty group of sweeps")
	}

	// Since the whole group is added to the same batch and belongs to
	// the same transaction, we use sweeps[0] below where we need any sweep.
	sweep := sweeps[0]

	completed, err := b.store.GetSweepStatus(ctx, sweep.outpoint)
	if err != nil {
		return fmt.Errorf("failed to get the status of sweep %v: %w",
			sweep.outpoint, err)
	}
	var (
		parentBatch    *dbBatch
		fullyConfirmed bool
	)
	if completed {
		// Verify that the parent batch is confirmed. Note that a batch
		// is only considered confirmed after it has received three
		// on-chain confirmations to prevent issues caused by reorgs.
		parentBatch, err = b.store.GetParentBatch(ctx, sweep.outpoint)
		if err != nil {
			return fmt.Errorf("unable to get parent batch for "+
				"sweep %x: %w", sweep.swapHash[:6], err)
		}

		if parentBatch.Confirmed {
			fullyConfirmed = true
		}
	}

	minRelayFeeRate, err := b.wallet.MinRelayFee(ctx)
	if err != nil {
		return fmt.Errorf("failed to get min relay fee: %w", err)
	}

	// If this is a presigned mode, make sure PresignSweepsGroup was called.
	// We skip the check for reorg-safely confirmed sweeps, because their
	// presigned transactions were already cleaned up from the store.
	if sweep.presigned && !fullyConfirmed {
		err := ensurePresigned(
			ctx, sweeps, b.presignedHelper, minRelayFeeRate,
			b.chainParams,
		)
		if err != nil {
			return fmt.Errorf("inputs with primarySweep %v were "+
				"not presigned (call PresignSweepsGroup "+
				"first): %w", sweep.outpoint, err)
		}
	}

	infof("Batcher adding sweep group of %d sweeps with primarySweep %x, "+
		"presigned=%v, fully_confirmed=%v", len(sweeps),
		sweep.swapHash[:6], sweep.presigned, completed)

	req := &addSweepsRequest{
		sweeps:   sweeps,
		notifier: sweepReq.Notifier,
		fast:     sweepReq.Fast,
	}

	select {
	case b.addSweepsChan <- req:
		return nil

	case <-b.quit:
		return ErrBatcherShuttingDown
	}
}

// testRunInEventLoop runs a function in the event loop blocking until
// the function returns. For unit tests only!
func (b *Batcher) testRunInEventLoop(ctx context.Context, handler func()) {
	// If the event loop is finished, run the function.
	select {
	case <-b.quit:
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

// handleSweeps handles a sweep request by either placing the group of sweeps in
// an existing batch, or by spinning up a new batch for it.
func (b *Batcher) handleSweeps(ctx context.Context, sweeps []*sweep,
	notifier *SpendNotifier, fast bool) error {

	// Since the whole group is added to the same batch and belongs to
	// the same transaction, we use sweeps[0] below where we need any sweep.
	sweep := sweeps[0]

	sweep.notifier = notifier

	// Check if the sweep is already in a batch. If that is the case, we
	// provide the sweep to that batch and return.
	readded := false
	for _, batch := range b.batches {
		if batch.sweepExists(sweep.outpoint) {
			accepted, err := batch.addSweeps(ctx, sweeps)
			if err != nil && !errors.Is(err, ErrBatchShuttingDown) {
				return err
			}

			if !accepted {
				return fmt.Errorf("existing sweep %x was not "+
					"accepted by batch %d",
					sweep.swapHash[:6], batch.id)
			}

			// The sweep was updated in the batch, our job is done.
			readded = true
			break
		}
	}

	// Check whether any batch is already completed and lazily delete it. Do
	// this after attempting the re-add so we do not remove the batch that
	// would have accepted the sweep again; otherwise we could miss the
	// re-addition and spin up a new batch for an already finished sweep.
	for _, batch := range b.batches {
		if batch.isComplete() {
			delete(b.batches, batch.id)
		}
	}

	// If the batch was re-added to an existing batch, our job is done.
	if readded {
		return nil
	}

	// If the sweep has already been completed in a confirmed batch then we
	// can't attach its notifier to the batch as that is no longer running.
	// Instead we directly detect and return the spend here. We cannot reuse
	// the values gathered in AddSweep because the sweep status may change in
	// the meantime. If the status flips while handleSweeps is running, the
	// re-add path above will handle it. A batch is removed from b.batches
	// only after the code below finds the sweep fully confirmed and switches
	// to the monitorSpendAndNotify path.
	completed, err := b.store.GetSweepStatus(ctx, sweep.outpoint)
	if err != nil {
		return fmt.Errorf("failed to get the status of sweep %v: %w",
			sweep.outpoint, err)
	}
	debugf("Status of the sweep group of %d sweeps with primarySweep %x: "+
		"presigned=%v, fully_confirmed=%v", len(sweeps),
		sweep.swapHash[:6], sweep.presigned, completed)
	if completed {
		// Verify that the parent batch is confirmed. Note that a batch
		// is only considered confirmed after it has received three
		// on-chain confirmations to prevent issues caused by reorgs.
		parentBatch, err := b.store.GetParentBatch(ctx, sweep.outpoint)
		if err != nil {
			return fmt.Errorf("unable to get parent batch for "+
				"sweep %x: %w", sweep.swapHash[:6], err)
		}

		debugf("Status of the parent batch of the sweep group of %d "+
			"sweeps with primarySweep %x: confirmed=%v",
			len(sweeps), sweep.swapHash[:6], parentBatch.Confirmed)

		// Note that sweeps are marked completed after the batch is
		// marked confirmed because here we check the sweep status
		// first and then check the batch status.
		if parentBatch.Confirmed {
			debugf("Sweep group of %d sweeps with primarySweep %x "+
				"is fully confirmed, switching directly to "+
				"monitoring", len(sweeps), sweep.swapHash[:6])

			return b.monitorSpendAndNotify(
				ctx, sweeps, parentBatch.ID, notifier,
			)
		}
	}

	// If fast is set, we spin up a new batch which is published
	// immediately.
	if fast {
		return b.spinUpNewBatch(ctx, sweeps, true)
	}

	// Try to run the greedy algorithm of batch selection to minimize costs.
	err = b.greedyAddSweeps(ctx, sweeps)
	if err == nil {
		// The greedy algorithm succeeded.
		return nil
	}

	warnf("Greedy batch selection algorithm failed for sweep %x: %v."+
		" Falling back to old approach.", sweep.swapHash[:6], err)

	// If one of the batches accepts the sweep, we provide it to that batch.
	for _, batch := range b.batches {
		accepted, err := batch.addSweeps(ctx, sweeps)
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
	return b.spinUpNewBatch(ctx, sweeps, false)
}

// spinUpNewBatch creates new batch, starts it and adds the sweeps to it. If
// presigned mode is enabled, the result also depends on outcome of
// presignedHelper.SignTx.
func (b *Batcher) spinUpNewBatch(ctx context.Context, sweeps []*sweep,
	fast bool) error {

	// Spin up a fresh batch.
	newBatch, err := b.spinUpBatch(ctx, fast)
	if err != nil {
		return err
	}

	// Add the sweeps to the fresh batch.
	accepted, err := newBatch.addSweeps(ctx, sweeps)
	if err != nil {
		return err
	}

	// If the sweeps weren't accepted by the fresh batch something is wrong,
	// we should return the error.
	if !accepted {
		return fmt.Errorf("sweep %x was not accepted by new batch %d",
			sweeps[0].swapHash[:6], newBatch.id)
	}

	return nil
}

// spinUpBatch spins up a new batch and returns it.
func (b *Batcher) spinUpBatch(ctx context.Context, fast bool) (*batch, error) {
	cfg := b.newBatchConfig(defaultMaxTimeoutDistance)

	switch b.chainParams {
	case &chaincfg.MainNetParams:
		cfg.batchPublishDelay = defaultMainnetPublishDelay

	default:
		cfg.batchPublishDelay = defaultTestnetPublishDelay
	}

	if b.publishDelay != 0 {
		if b.publishDelay < 0 {
			return nil, fmt.Errorf("negative publishDelay: %v",
				b.publishDelay)
		}
		cfg.batchPublishDelay = b.publishDelay
	}

	cfg.initialDelayProvider = b.initialDelayProvider
	if cfg.initialDelayProvider == nil || fast {
		cfg.initialDelayProvider = zeroInitialDelay
	}

	batchKit := b.newBatchKit()

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
			b.writeToErrChan(
				ctx, fmt.Errorf("new batch failed: %w", err),
			)
		}
	}()

	return batch, nil
}

// filterDbSweeps copies dbSweeps, skipping the sweeps from skipped txs.
func filterDbSweeps(skippedTxns map[chainhash.Hash]struct{},
	dbSweeps []*dbSweep) []*dbSweep {

	result := make([]*dbSweep, 0, len(dbSweeps))
	for _, dbSweep := range dbSweeps {
		if _, has := skippedTxns[dbSweep.Outpoint.Hash]; has {
			continue
		}

		result = append(result, dbSweep)
	}

	return result
}

// spinUpBatchFromDB spins up a batch that already existed in storage, then
// returns it.
func (b *Batcher) spinUpBatchFromDB(ctx context.Context, batch *batch) error {
	dbSweeps, err := b.store.FetchBatchSweeps(ctx, batch.id)
	if err != nil {
		return err
	}
	dbSweeps = filterDbSweeps(b.skippedTxns, dbSweeps)

	if len(dbSweeps) == 0 {
		infof("skipping restored batch %d as it has no sweeps",
			batch.id)

		// It is safe to cancel this empty batch as it has no sweeps
		// that are not skipped.
		err := b.store.CancelBatch(ctx, batch.id)
		if err != nil {
			warnf("unable to drop empty batch %d: %v",
				batch.id, err)
		}

		return nil
	}

	primarySweep := dbSweeps[0]

	sweeps := make(map[wire.OutPoint]sweep)

	// Collect feeRate from sweeps and stored batch.
	feeRate := batch.rbfCache.FeeRate

	for _, dbSweep := range dbSweeps {
		sweep, err := b.convertSweep(ctx, dbSweep)
		if err != nil {
			return err
		}

		sweeps[sweep.outpoint] = *sweep

		// Set minFeeRate to max(sweep.minFeeRate) for all sweeps.
		if feeRate < sweep.minFeeRate {
			feeRate = sweep.minFeeRate
		}
	}

	rbfCache := rbfCache{
		LastHeight: batch.rbfCache.LastHeight,
		FeeRate:    feeRate,
	}

	logger := batchPrefixLogger(fmt.Sprintf("%d", batch.id))

	batchKit := b.newBatchKit()
	batchKit.id = batch.id
	batchKit.batchTxid = batch.batchTxid
	batchKit.batchPkScript = batch.batchPkScript
	batchKit.state = batch.state
	batchKit.primaryID = primarySweep.Outpoint
	batchKit.sweeps = sweeps
	batchKit.rbfCache = rbfCache
	batchKit.log = logger

	cfg := b.newBatchConfig(batch.cfg.maxTimeoutDistance)

	// Note that initialDelay and batchPublishDelay are 0 for batches
	// recovered from DB so publishing happen in case of a daemon restart
	// (especially important in case of a crashloop).
	cfg.initialDelayProvider = zeroInitialDelay

	newBatch, err := NewBatchFromDB(cfg, batchKit)
	if err != nil {
		return fmt.Errorf("failed in NewBatchFromDB: %w", err)
	}

	// We add the batch to our map of batches and start it.
	b.batches[batch.id] = newBatch

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()

		err := newBatch.Run(ctx)
		if err != nil {
			b.writeToErrChan(
				ctx, fmt.Errorf("db batch failed: %w", err),
			)
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
		batch := batch{}
		batch.id = bch.ID

		if bch.Confirmed {
			batch.state = Confirmed
		} else {
			// We don't store Closed state separately in DB.
			// If the batch is closed (included into a block, but
			// not reorg-safely confirmed), it is now considered
			// Open again. It will receive a spending notification
			// as soon as it starts, so it is not an issue. If a
			// sweep manages to be added during this time, it will
			// be detected as missing when analyzing the spend
			// notification and will be added to new batch.
			batch.state = Open
		}

		batch.batchTxid = &bch.BatchTxid
		batch.batchPkScript = bch.BatchPkScript

		rbfCache := rbfCache{
			LastHeight: bch.LastRbfHeight,
			FeeRate:    chainfee.SatPerKWeight(bch.LastRbfSatPerKw),
		}
		batch.rbfCache = rbfCache

		bchCfg := b.newBatchConfig(bch.MaxTimeoutDistance)
		batch.cfg = &bchCfg

		batches = append(batches, &batch)
	}

	return batches, nil
}

// monitorSpendAndNotify monitors the spend of a specific outpoint and writes
// the response back to the response channel. It is called if the batch is fully
// confirmed and we just need to deliver the data back to the caller though
// SpendNotifier.
func (b *Batcher) monitorSpendAndNotify(ctx context.Context, sweeps []*sweep,
	parentBatchID int32, notifier *SpendNotifier) error {

	// If the caller has not provided a notifier, stop.
	if notifier == nil || *notifier == (SpendNotifier{}) {
		return nil
	}

	spendCtx, cancel := context.WithCancel(ctx)

	// Then we get the total amount that was swept by the batch.
	totalSwept, err := b.store.TotalSweptAmount(ctx, parentBatchID)
	if err != nil {
		cancel()

		return err
	}

	// Find the primarySweepID.
	dbSweeps, err := b.store.FetchBatchSweeps(ctx, parentBatchID)
	if err != nil {
		cancel()

		return err
	}
	primarySweepID := dbSweeps[0].Outpoint

	sweep := sweeps[0]

	spendChan, spendErr, err := b.chainNotifier.RegisterSpendNtfn(
		spendCtx, &sweep.outpoint, sweep.htlc.PkScript,
		sweep.initiationHeight,
	)
	if err != nil {
		cancel()

		return err
	}

	b.wg.Add(1)
	go func() {
		defer cancel()
		defer b.wg.Done()
		infof("Batcher monitoring spend for swap %x",
			sweep.swapHash[:6])

		select {
		case spend := <-spendChan:
			spendTx := spend.SpendingTx

			// Calculate the fee portion that each sweep should pay
			// for the batch.
			feePortionPerSweep, roundingDifference :=
				getFeePortionForSweep(
					spendTx, len(spendTx.TxIn),
					totalSwept,
				)

			// Sum onchain fee across all the sweeps of the swap.
			var fee btcutil.Amount
			for _, s := range sweeps {
				isFirst := s.outpoint == primarySweepID

				fee += getFeePortionPaidBySweep(
					feePortionPerSweep, roundingDifference,
					isFirst,
				)
			}

			// Notify the requester of the spend with the spend
			// details, including the fee portion for this
			// particular sweep.
			spendDetail := &SpendDetail{
				Tx:                spendTx,
				OnChainFeePortion: fee,
			}

			select {
			// Try to write the update to the notification channel.
			case notifier.SpendChan <- spendDetail:
				err := b.monitorConfAndNotify(
					ctx, sweep, notifier, spendTx,
					fee,
				)
				if err != nil {
					b.writeToErrChan(
						ctx, fmt.Errorf("monitor conf "+
							"failed: %w", err),
					)
				}

			// If a quit signal was provided by the swap, continue.
			case <-notifier.QuitChan:

			// If the context was canceled, stop.
			case <-ctx.Done():
			}

			return

		case err := <-spendErr:
			select {
			// Try to write the error to the notification
			// channel.
			case notifier.SpendErrChan <- err:

			// If a quit signal was provided by the swap,
			// continue.
			case <-notifier.QuitChan:

			// If the context was canceled, stop.
			case <-ctx.Done():
			}

			b.writeToErrChan(
				ctx, fmt.Errorf("spend error: %w", err),
			)

			return

		// If a quit signal was provided by the swap, continue.
		case <-notifier.QuitChan:
			return

		// If the context was canceled, stop.
		case <-ctx.Done():
			return
		}
	}()

	return nil
}

// monitorConfAndNotify monitors the confirmation of a specific transaction and
// writes the response back to the response channel. It is called if the batch
// is reorg-safely confirmed, and we just need to deliver the data back to the
// caller though SpendNotifier.
func (b *Batcher) monitorConfAndNotify(ctx context.Context, sweep *sweep,
	notifier *SpendNotifier, spendTx *wire.MsgTx,
	onChainFeePortion btcutil.Amount) error {

	// If confirmation notifications were not requested, stop.
	if notifier.ConfChan == nil && notifier.ConfErrChan == nil {
		return nil
	}

	batchTxid := spendTx.TxHash()

	if len(spendTx.TxOut) < 1 {
		return fmt.Errorf("expected at least one output in batch")
	}
	batchPkScript := spendTx.TxOut[0].PkScript

	reorgChan := make(chan struct{})

	confCtx, cancel := context.WithCancel(ctx)

	confChan, errChan, err := b.chainNotifier.RegisterConfirmationsNtfn(
		confCtx, &batchTxid, batchPkScript, batchConfHeight,
		sweep.initiationHeight, lndclient.WithReOrgChan(reorgChan),
	)
	if err != nil {
		cancel()
		return err
	}

	b.wg.Add(1)
	go func() {
		defer cancel()
		defer b.wg.Done()

		select {
		case conf := <-confChan:
			if notifier.ConfChan != nil {
				confDetail := &ConfDetail{
					TxConfirmation:    conf,
					OnChainFeePortion: onChainFeePortion,
				}

				select {
				case notifier.ConfChan <- confDetail:
				case <-notifier.QuitChan:
				case <-ctx.Done():
				}
			}

		case err := <-errChan:
			if notifier.ConfErrChan != nil {
				select {
				case notifier.ConfErrChan <- err:
				case <-notifier.QuitChan:
				case <-ctx.Done():
				}
			}

			b.writeToErrChan(ctx, fmt.Errorf("confirmations "+
				"monitoring error: %w", err))

		case <-reorgChan:
			// A re-org has been detected, but the batch is fully
			// confirmed and this is unexpected. Crash the batcher.
			b.writeToErrChan(ctx, fmt.Errorf("unexpected reorg"))

		case <-ctx.Done():
		}
	}()

	return nil
}

func (b *Batcher) writeToErrChan(ctx context.Context, err error) {
	select {
	case b.errChan <- err:
	case <-ctx.Done():
	}
}

// convertSweep converts a fetched sweep from the database to a sweep that is
// ready to be processed by the batcher. It loads swap from loopdb by calling
// the method FetchLoopOutSwap.
func (b *Batcher) convertSweep(ctx context.Context, dbSweep *dbSweep) (
	*sweep, error) {

	return b.loadSweep(ctx, dbSweep.SwapHash, dbSweep.Outpoint,
		dbSweep.Amount)
}

// LoopOutFetcher is used to load LoopOut swaps from the database.
// It is implemented by loopdb.SwapStore.
type LoopOutFetcher interface {
	// FetchLoopOutSwap returns the loop out swap with the given hash.
	FetchLoopOutSwap(ctx context.Context,
		hash lntypes.Hash) (*loopdb.LoopOut, error)
}

// SwapStoreWrapper is LoopOutFetcher wrapper providing SweepFetcher interface.
type SwapStoreWrapper struct {
	// swapStore is used to load LoopOut swaps from the database.
	swapStore LoopOutFetcher

	// chainParams are the chain parameters of the chain that is used by
	// batches.
	chainParams *chaincfg.Params
}

// FetchSweep returns details of the sweep with the given hash.
// In LoopOut case, swap hashes are unique.
// Implements SweepFetcher interface.
func (f *SwapStoreWrapper) FetchSweep(ctx context.Context,
	swapHash lntypes.Hash, _ wire.OutPoint) (*SweepInfo, error) {

	swap, err := f.swapStore.FetchLoopOutSwap(ctx, swapHash)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch loop out for %x: %w",
			swapHash[:6], err)
	}

	htlc, err := utils.GetHtlc(
		swapHash, &swap.Contract.SwapContract, f.chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get htlc: %w", err)
	}

	swapPaymentAddr, err := utils.ObtainSwapPaymentAddr(
		swap.Contract.SwapInvoice, f.chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get payment addr: %w", err)
	}

	return &SweepInfo{
		ConfTarget:             swap.Contract.SweepConfTarget,
		Timeout:                swap.Contract.CltvExpiry,
		InitiationHeight:       swap.Contract.InitiationHeight,
		HTLC:                   *htlc,
		Preimage:               swap.Contract.Preimage,
		SwapInvoicePaymentAddr: *swapPaymentAddr,
		HTLCKeys:               swap.Contract.HtlcKeys,
		HTLCSuccessEstimator:   htlc.AddSuccessToEstimator,
		ProtocolVersion:        swap.Contract.ProtocolVersion,
		IsExternalAddr:         swap.Contract.IsExternalAddr,
		DestAddr:               swap.Contract.DestAddr,
	}, nil
}

// NewSweepFetcherFromSwapStore accepts swapStore (e.g. loopdb) and returns
// a wrapper implementing SweepFetcher interface (suitable for NewBatcher).
func NewSweepFetcherFromSwapStore(swapStore LoopOutFetcher,
	chainParams *chaincfg.Params) (*SwapStoreWrapper, error) {

	return &SwapStoreWrapper{
		swapStore:   swapStore,
		chainParams: chainParams,
	}, nil
}

// fetchSweeps fetches the sweep related information from the database.
func (b *Batcher) fetchSweeps(ctx context.Context,
	sweepReq SweepRequest) ([]*sweep, error) {

	sweeps := make([]*sweep, len(sweepReq.Inputs))
	for i, utxo := range sweepReq.Inputs {
		s, err := b.loadSweep(
			ctx, sweepReq.SwapHash, utxo.Outpoint,
			utxo.Value,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to load "+
				"sweep %v: %w", utxo.Outpoint, err)
		}
		sweeps[i] = s
	}

	return sweeps, nil
}

// loadSweep loads inputs of sweep from the database and from FeeRateProvider
// if needed and returns an assembled sweep object.
func (b *Batcher) loadSweep(ctx context.Context, swapHash lntypes.Hash,
	outpoint wire.OutPoint, value btcutil.Amount) (*sweep, error) {

	s, err := b.sweepStore.FetchSweep(ctx, swapHash, outpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch sweep data for %x: %w",
			swapHash[:6], err)
	}

	// Make sure that PkScript of the coin is filled. Otherwise
	// RegisterSpendNtfn fails.
	if len(s.HTLC.PkScript) == 0 {
		return nil, fmt.Errorf("sweep data for %x doesn't have "+
			"HTLC.PkScript set", swapHash[:6])
	}

	// Find minimum fee rate for the sweep. Use customFeeRate if it is
	// provided, otherwise use wallet's EstimateFeeRate.
	minFeeRate, err := minimumSweepFeeRate(
		ctx, b.customFeeRate, b.wallet,
		swapHash, outpoint, s.ConfTarget,
	)
	if err != nil {
		return nil, err
	}

	return &sweep{
		swapHash:               swapHash,
		outpoint:               outpoint,
		value:                  value,
		confTarget:             s.ConfTarget,
		timeout:                s.Timeout,
		initiationHeight:       s.InitiationHeight,
		htlc:                   s.HTLC,
		preimage:               s.Preimage,
		swapInvoicePaymentAddr: s.SwapInvoicePaymentAddr,
		htlcKeys:               s.HTLCKeys,
		htlcSuccessEstimator:   s.HTLCSuccessEstimator,
		protocolVersion:        s.ProtocolVersion,
		isExternalAddr:         s.IsExternalAddr,
		destAddr:               s.DestAddr,
		minFeeRate:             minFeeRate,
		nonCoopHint:            s.NonCoopHint,
		presigned:              s.IsPresigned,
		change:                 s.Change,
	}, nil
}

// feeRateEstimator determines feerate by confTarget.
type feeRateEstimator interface {
	// EstimateFeeRate returns feerate corresponding to the confTarget.
	EstimateFeeRate(ctx context.Context,
		confTarget int32) (chainfee.SatPerKWeight, error)
}

// minimumSweepFeeRate determines minimum feerate for a sweep.
func minimumSweepFeeRate(ctx context.Context, customFeeRate FeeRateProvider,
	wallet feeRateEstimator, swapHash lntypes.Hash, outpoint wire.OutPoint,
	sweepConfTarget int32) (chainfee.SatPerKWeight, error) {

	// Find minimum fee rate for the sweep. Use customFeeRate if it is
	// provided, otherwise use wallet's EstimateFeeRate.
	if customFeeRate != nil {
		minFeeRate, err := customFeeRate(ctx, swapHash, outpoint)
		if err != nil {
			return 0, fmt.Errorf("failed to fetch min fee rate "+
				"for %x: %w", swapHash[:6], err)
		}
		if minFeeRate < chainfee.AbsoluteFeePerKwFloor {
			return 0, fmt.Errorf("min fee rate too low (%v) for "+
				"%x", minFeeRate, swapHash[:6])
		}

		return minFeeRate, nil
	}

	// Make sure sweepConfTarget is at least 2. LND's walletkit fails with
	// conftarget of 0 or 1.
	// TODO: when https://github.com/lightningnetwork/lnd/pull/10087 is
	// merged and that LND version becomes a requirement, we can decrease
	// this from 2 to 1.
	if sweepConfTarget < 2 {
		warnf("Fee estimation was requested for confTarget=%d for "+
			"sweep %x; changing confTarget to 2", sweepConfTarget,
			swapHash[:6])
		sweepConfTarget = 2
	}

	minFeeRate, err := wallet.EstimateFeeRate(ctx, sweepConfTarget)
	if err != nil {
		return 0, fmt.Errorf("failed to estimate fee rate "+
			"for %x, confTarget=%d: %w", swapHash[:6],
			sweepConfTarget, err)
	}

	return minFeeRate, nil
}

// newBatchConfig creates new batch config.
func (b *Batcher) newBatchConfig(maxTimeoutDistance int32) batchConfig {
	return batchConfig{
		maxTimeoutDistance: maxTimeoutDistance,
		customFeeRate:      b.customFeeRate,
		txLabeler:          b.txLabeler,
		customMuSig2Signer: b.customMuSig2Signer,
		presignedHelper:    b.presignedHelper,
		skippedTxns:        b.skippedTxns,
		clock:              b.clock,
		chainParams:        b.chainParams,
	}
}

// newBatchKit creates new batch kit.
func (b *Batcher) newBatchKit() batchKit {
	return batchKit{
		wallet:              b.wallet,
		chainNotifier:       b.chainNotifier,
		signerClient:        b.signerClient,
		musig2SignSweep:     b.musig2ServerSign,
		verifySchnorrSig:    b.VerifySchnorrSig,
		publishErrorHandler: b.publishErrorHandler,
		purger:              b.AddSweep,
		store:               b.store,
		quit:                b.quit,
	}
}
