package withdraw

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"golang.org/x/sync/errgroup"
)

var (
	// ErrWithdrawingMixedDeposits is returned when a withdrawal is
	// requested for deposits in different states.
	ErrWithdrawingMixedDeposits = errors.New("need to withdraw deposits " +
		"having the same state, either all deposited or all " +
		"withdrawing")

	// ErrDiffPreviousWithdrawalTx signals that the user selected new
	// deposits that have different previous withdrawal transactions.
	ErrDiffPreviousWithdrawalTx = errors.New("can't bump fee for " +
		"deposits with different previous withdrawal tx hash")

	// ErrMissingPreviousWithdrawn is returned when the user tries to bump
	// the fee for a subset of previously selected deposits to withdraw.
	ErrMissingPreviousWithdrawn = errors.New("can't bump fee for subset " +
		"of clustered deposits")

	// ErrMissingFinalizedTx is returned if previously withdrawn deposits
	// don't have a finalized withdrawal tx attached.
	ErrMissingFinalizedTx = errors.New("deposit does not have a " +
		"finalized withdrawal tx, can't bump fee")

	// MinConfs is the minimum number of confirmations we require for a
	// deposit to be considered withdrawn.
	MinConfs int32 = 3

	// Is the default confirmation target for the fee estimation of the
	// withdrawal transaction.
	defaultConfTarget int32 = 3
)

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	// StaticAddressServerClient is the client that calls the swap server
	// rpcs to negotiate static address withdrawals.
	StaticAddressServerClient staticaddressrpc.StaticAddressServerClient

	// AddressManager gives the withdrawal manager access to static address
	// parameters.
	AddressManager AddressManager

	// DepositManager gives the withdrawal manager access to the deposits
	// enabling it to create and manage withdrawals.
	DepositManager DepositManager

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params

	// ChainNotifier is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is the signer client that is used to sign transactions.
	Signer lndclient.SignerClient

	// Store is the store that is used to persist the finalized withdrawal
	// transactions.
	Store *SqlStore
}

// newWithdrawalRequest is used to send withdrawal request to the manager main
// loop.
type newWithdrawalRequest struct {
	outpoints   []wire.OutPoint
	respChan    chan *newWithdrawalResponse
	destAddr    string
	satPerVbyte int64
	amount      int64
}

// newWithdrawalResponse is used to return withdrawal info and error to the
// server.
type newWithdrawalResponse struct {
	txHash            string
	withdrawalAddress string
	err               error
}

// Manager manages the withdrawal state machines.
type Manager struct {
	cfg *ManagerConfig

	// mu protects access to finalizedWithdrawalTxns.
	mu sync.Mutex

	// newWithdrawalRequestChan receives a list of outpoints that should be
	// withdrawn. The request is forwarded to the managers main loop.
	newWithdrawalRequestChan chan newWithdrawalRequest

	// exitChan signals subroutines that the withdrawal manager is exiting.
	exitChan chan struct{}

	// errChan forwards errors from the withdrawal manager to the server.
	errChan chan error

	// initiationHeight stores the currently best known block height.
	initiationHeight atomic.Uint32

	// finalizedWithdrawalTx are the finalized withdrawal transactions that
	// are published to the network and re-published on block arrivals.
	finalizedWithdrawalTxns map[chainhash.Hash]*wire.MsgTx
}

// NewManager creates a new deposit withdrawal manager.
func NewManager(cfg *ManagerConfig, currentHeight uint32) *Manager {
	m := &Manager{
		cfg:                      cfg,
		finalizedWithdrawalTxns:  make(map[chainhash.Hash]*wire.MsgTx),
		exitChan:                 make(chan struct{}),
		newWithdrawalRequestChan: make(chan newWithdrawalRequest),
		errChan:                  make(chan error),
	}
	m.initiationHeight.Store(currentHeight)

	return m
}

// Run runs the deposit withdrawal manager.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	newBlockChan, newBlockErrChan, err :=
		m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx)

	if err != nil {
		log.Errorf("unable to register for block epoch "+
			"notifications: %v", err)

		return err
	}

	err = m.recoverWithdrawals(ctx)
	if err != nil {
		log.Errorf("unable to recover withdrawals: %v", err)

		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(initChan)

	for {
		select {
		case <-newBlockChan:
			err = m.republishWithdrawals(ctx)
			if err != nil {
				log.Errorf("Error republishing withdrawals: %v",
					err)
			}

		case req := <-m.newWithdrawalRequestChan:
			txHash, withdrawalAddress, err := m.WithdrawDeposits(
				ctx, req.outpoints, req.destAddr,
				req.satPerVbyte, req.amount,
			)
			if err != nil {
				log.Errorf("Error withdrawing deposits: %v",
					err)
			}

			// We forward the initialized loop-in and error to
			// DeliverLoopInRequest.
			resp := &newWithdrawalResponse{
				txHash:            txHash,
				withdrawalAddress: withdrawalAddress,
				err:               err,
			}
			select {
			case req.respChan <- resp:

			case <-ctx.Done():
				// Notify subroutines that the main loop has
				// been canceled.
				close(m.exitChan)

				return ctx.Err()
			}

		case err = <-newBlockErrChan:
			return err

		case <-ctx.Done():
			// Signal subroutines that the manager is exiting.
			close(m.exitChan)

			return ctx.Err()
		}
	}
}

func (m *Manager) recoverWithdrawals(ctx context.Context) error {
	// To recover withdrawals we cluster those with equal withdrawal
	// addresses and publish their withdrawal tx. Each cluster represents a
	// separate withdrawal intent by the user.
	withdrawingDeposits, err := m.cfg.DepositManager.GetActiveDepositsInState(
		deposit.Withdrawing,
	)
	if err != nil {
		return err
	}

	// Group the deposits by their finalized withdrawal transaction.
	depositsByWithdrawalTx := make(map[chainhash.Hash][]*deposit.Deposit)
	hash2tx := make(map[chainhash.Hash]*wire.MsgTx)
	for _, d := range withdrawingDeposits {
		withdrawalTx := d.FinalizedWithdrawalTx
		if withdrawalTx == nil {
			continue
		}
		txid := withdrawalTx.TxHash()
		hash2tx[txid] = withdrawalTx

		depositsByWithdrawalTx[txid] = append(
			depositsByWithdrawalTx[txid], d,
		)
	}

	// Publishing a transaction can take a while in neutrino mode, so
	// do it in parallel.
	eg := &errgroup.Group{}

	// We can now reinstate each cluster of deposits for a withdrawal.
	for txid, deposits := range depositsByWithdrawalTx {
		eg.Go(func() error {
			err := m.cfg.DepositManager.TransitionDeposits(
				ctx, deposits, deposit.OnWithdrawInitiated,
				deposit.Withdrawing,
			)
			if err != nil {
				return err
			}

			tx, ok := hash2tx[txid]
			if !ok {
				return fmt.Errorf("can't find tx %v", txid)
			}

			_, err = m.publishFinalizedWithdrawalTx(ctx, tx)
			if err != nil {
				return err
			}

			err = m.handleWithdrawal(
				ctx, deposits, tx.TxHash(),
				tx.TxOut[0].PkScript,
			)
			if err != nil {
				return err
			}

			m.mu.Lock()
			m.finalizedWithdrawalTxns[tx.TxHash()] = tx
			m.mu.Unlock()

			return nil
		})
	}

	// Wait for all goroutines to report back.
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("error recovering withdrawals: %w", err)
	}

	return nil
}

// WithdrawDeposits starts a deposits withdrawal flow. If the amount is set to 0
// the full amount of the selected deposits will be withdrawn.
func (m *Manager) WithdrawDeposits(ctx context.Context,
	outpoints []wire.OutPoint, destAddr string, satPerVbyte int64,
	amount int64) (string, string, error) {

	if len(outpoints) == 0 {
		return "", "", fmt.Errorf("no outpoints selected to " +
			"withdraw, unconfirmed deposits can't be withdrawn")
	}

	var (
		deposits       []*deposit.Deposit
		allDeposited   bool
		allWithdrawing bool
	)

	// Ensure that the deposits are in a state in which they can be
	// withdrawn.
	deposits, allDeposited = m.cfg.DepositManager.AllOutpointsActiveDeposits(
		outpoints, deposit.Deposited,
	)

	// If not all passed outpoints are in state Deposited, we'll check if
	// they are all in state Withdrawing. If they are, then the user is
	// requesting a fee bump, if not we'll return an error as we only allow
	// fee bumping deposits in state Withdrawing.
	if !allDeposited {
		deposits, allWithdrawing = m.cfg.DepositManager.AllOutpointsActiveDeposits(
			outpoints, deposit.Withdrawing,
		)

		if !allWithdrawing {
			return "", "", ErrWithdrawingMixedDeposits
		}

		// Ensure that all previously withdrawn deposits reference their
		// finalized withdrawal tx.
		for _, d := range deposits {
			if d.FinalizedWithdrawalTx == nil {
				return "", "", ErrMissingFinalizedTx
			}
		}

		// If republishing of an existing withdrawal is requested we
		// ensure that all deposits remain clustered in the context of
		// the same withdrawal tx. We do this by checking that they have
		// the same previous withdrawal tx hash. This ensures that the
		// shape of the transaction stays the same.
		prevWithdrawalTx := deposits[0].FinalizedWithdrawalTx
		hash := prevWithdrawalTx.TxHash()
		for i := 1; i < len(deposits); i++ {
			if deposits[i].FinalizedWithdrawalTx.TxHash() != hash {
				return "", "", ErrDiffPreviousWithdrawalTx
			}
		}

		// We also avoid that the user selects a subset of previously
		// clustered deposits for a fee bump. This would result in a
		// different transaction shape.
		outpointMap := make(map[wire.OutPoint]struct{})
		for _, d := range deposits {
			outpointMap[d.OutPoint] = struct{}{}
		}

		// Check that all previously withdrawn deposits are included in
		// the new withdrawal.
		for _, in := range prevWithdrawalTx.TxIn {
			if _, ok := outpointMap[in.PreviousOutPoint]; !ok {
				return "", "", ErrMissingPreviousWithdrawn
			}
		}

		if len(deposits) != len(prevWithdrawalTx.TxIn) {
			return "", "", ErrMissingPreviousWithdrawn
		}
	}

	var (
		withdrawalAddress btcutil.Address
		err               error
	)

	// Check if the user provided an address to withdraw to. If not, we'll
	// generate a new address for them.
	if destAddr != "" {
		withdrawalAddress, err = btcutil.DecodeAddress(
			destAddr, m.cfg.ChainParams,
		)
		if err != nil {
			return "", "", err
		}
	} else {
		withdrawalAddress, err = m.cfg.WalletKit.NextAddr(
			ctx, lnwallet.DefaultAccountName,
			walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
		if err != nil {
			return "", "", err
		}
	}

	finalizedTx, err := m.createFinalizedWithdrawalTx(
		ctx, deposits, withdrawalAddress, satPerVbyte, amount,
	)
	if err != nil {
		return "", "", err
	}

	published, err := m.publishFinalizedWithdrawalTx(ctx, finalizedTx)
	if err != nil {
		return "", "", err
	}

	if !published {
		return "", "", nil
	}

	withdrawalPkScript, err := txscript.PayToAddrScript(withdrawalAddress)
	if err != nil {
		return "", "", fmt.Errorf("could not get withdrawal "+
			"pkscript: %w", err)
	}

	// If this is the first time this cluster of deposits is withdrawn, we
	// start a goroutine that listens for the spent of the first input of
	// the withdrawal transaction.
	// Since we ensure above that the same ensemble of deposits is
	// republished in case of a fee bump, it suffices if only one spent
	// notifier is run.
	if allDeposited {
		// Persist info about the finalized withdrawal.
		err = m.cfg.Store.CreateWithdrawal(ctx, deposits)
		if err != nil {
			log.Errorf("Error persisting "+
				"withdrawal: %v", err)
		}

		err = m.handleWithdrawal(
			ctx, deposits, finalizedTx.TxHash(), withdrawalPkScript,
		)
		if err != nil {
			return "", "", err
		}
	}

	// If a previous withdrawal existed across the selected deposits, and
	// it isn't the same as the new withdrawal, we remove it from the
	// finalized withdrawals to stop republishing it on block arrivals.
	deposits[0].Lock()
	prevTx := deposits[0].FinalizedWithdrawalTx
	deposits[0].Unlock()

	if prevTx != nil && prevTx.TxHash() != finalizedTx.TxHash() {
		m.mu.Lock()
		delete(m.finalizedWithdrawalTxns, prevTx.TxHash())
		m.mu.Unlock()
	}

	// Attach the finalized withdrawal tx to the deposits. After a client
	// restart we can use this address as an indicator to republish the
	// withdrawal tx and continue the withdrawal.
	// Deposits with the same withdrawal tx are part of the same withdrawal.
	for _, d := range deposits {
		d.Lock()
		d.FinalizedWithdrawalTx = finalizedTx
		d.Unlock()
	}

	// Add the new withdrawal tx to the finalized withdrawals to republish
	// it on block arrivals.
	m.mu.Lock()
	m.finalizedWithdrawalTxns[finalizedTx.TxHash()] = finalizedTx
	m.mu.Unlock()

	// Transition the deposits to the withdrawing state. If the user fee
	// bumped a withdrawal this results in a NOOP transition.
	err = m.cfg.DepositManager.TransitionDeposits(
		ctx, deposits, deposit.OnWithdrawInitiated, deposit.Withdrawing,
	)
	if err != nil {
		return "", "", fmt.Errorf("failed to transition deposits %w",
			err)
	}

	// Update the deposits in the database.
	for _, d := range deposits {
		err = m.cfg.DepositManager.UpdateDeposit(ctx, d)
		if err != nil {
			return "", "", fmt.Errorf("failed to update "+
				"deposit %w", err)
		}
	}

	return finalizedTx.TxID(), withdrawalAddress.String(), nil
}

func (m *Manager) createFinalizedWithdrawalTx(ctx context.Context,
	deposits []*deposit.Deposit, withdrawalAddress btcutil.Address,
	satPerVbyte int64, selectedWithdrawalAmount int64) (*wire.MsgTx,
	error) {

	// Create a musig2 session for each deposit.
	withdrawalSessions, clientNonces, err := m.createMusig2Sessions(
		ctx, deposits,
	)
	if err != nil {
		return nil, err
	}

	var withdrawalSweepFeeRate chainfee.SatPerKWeight
	if satPerVbyte == 0 {
		// Get the fee rate for the withdrawal sweep.
		withdrawalSweepFeeRate, err = m.cfg.WalletKit.EstimateFeeRate(
			ctx, defaultConfTarget,
		)
		if err != nil {
			return nil, err
		}
	} else {
		withdrawalSweepFeeRate = chainfee.SatPerKVByte(
			satPerVbyte * 1000,
		).FeePerKWeight()
	}

	params, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %w", err)
	}

	outpoints := toOutpoints(deposits)
	prevOuts := m.toPrevOuts(deposits, params.PkScript)
	withdrawalTx, withdrawAmount, changeAmount, err := m.createWithdrawalTx(
		ctx, outpoints, prevOuts,
		btcutil.Amount(selectedWithdrawalAmount), withdrawalAddress,
		withdrawalSweepFeeRate,
	)
	if err != nil {
		return nil, err
	}

	// Request the server to sign the withdrawal transaction.
	//
	// The withdrawal and change amount are sent to the server with the
	// expectation that the server just signs the transaction, without
	// performing fee calculations and dust considerations. The client is
	// responsible for that.
	resp, err := m.cfg.StaticAddressServerClient.ServerWithdrawDeposits(
		ctx, &staticaddressrpc.ServerWithdrawRequest{
			Outpoints:       toPrevoutInfo(outpoints),
			ClientNonces:    clientNonces,
			ClientSweepAddr: withdrawalAddress.String(),
			TxFeeRate:       uint64(withdrawalSweepFeeRate),
			WithdrawAmount:  int64(withdrawAmount),
			ChangeAmount:    int64(changeAmount),
		},
	)
	if err != nil {
		return nil, err
	}

	coopServerNonces, err := toNonces(resp.ServerNonces)
	if err != nil {
		return nil, err
	}

	// Next we'll get our sweep tx signatures.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	_, err = m.signMusig2Tx(
		ctx, prevOutFetcher, outpoints, m.cfg.Signer, withdrawalTx,
		withdrawalSessions, coopServerNonces,
	)
	if err != nil {
		return nil, err
	}

	// Now we'll finalize the sweepless sweep transaction.
	finalizedTx, err := m.finalizeMusig2Transaction(
		ctx, outpoints, m.cfg.Signer, withdrawalSessions,
		withdrawalTx, resp.Musig2SweepSigs,
	)
	if err != nil {
		return nil, err
	}

	return finalizedTx, nil
}

func (m *Manager) publishFinalizedWithdrawalTx(ctx context.Context,
	tx *wire.MsgTx) (bool, error) {

	if tx == nil {
		return false, errors.New("can't publish, finalized " +
			"withdrawal tx is nil")
	}

	log.Debugf("Publishing deposit withdrawal with txid: %v ...",
		tx.TxHash())

	txLabel := fmt.Sprintf("deposit-withdrawal-%v", tx.TxHash())

	// Publish the withdrawal sweep transaction.
	err := m.cfg.WalletKit.PublishTransaction(ctx, tx, txLabel)
	if err != nil {
		if !strings.Contains(err.Error(), chain.ErrSameNonWitnessData.Error()) &&
			!strings.Contains(err.Error(), "output already spent") &&
			!strings.Contains(err.Error(), chain.ErrInsufficientFee.Error()) {

			return false, err
		} else {
			if strings.Contains(err.Error(), "output already spent") {
				log.Warnf("output already spent, tx %v, %v",
					tx.TxHash(), err)
			}

			return false, nil
		}
	} else {
		log.Debugf("Published deposit withdrawal with txid: %v",
			tx.TxHash())
	}

	return true, nil
}

// handleWithdrawal starts a goroutine that listens for the spent of the first
// input of the withdrawal transaction.
func (m *Manager) handleWithdrawal(ctx context.Context,
	deposits []*deposit.Deposit, txHash chainhash.Hash,
	withdrawalPkscript []byte) error {

	addrParams, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		log.Errorf("error retrieving address params %w", err)

		return fmt.Errorf("withdrawal failed")
	}

	d := deposits[0]
	spentChan, errChan, err := m.cfg.ChainNotifier.RegisterSpendNtfn(
		ctx, &d.OutPoint, addrParams.PkScript,
		int32(d.ConfirmationHeight),
	)

	go func() {
		select {
		case spentTx := <-spentChan:
			spendingHeight := uint32(spentTx.SpendingHeight)
			// If the transaction received one confirmation, we
			// ensure re-org safety by waiting for some more
			// confirmations.
			var confChan chan *chainntnfs.TxConfirmation
			confChan, errChan, err =
				m.cfg.ChainNotifier.RegisterConfirmationsNtfn(
					ctx, spentTx.SpenderTxHash,
					withdrawalPkscript, MinConfs,
					int32(m.initiationHeight.Load()),
				)
			select {
			case tx := <-confChan:
				err = m.cfg.DepositManager.TransitionDeposits(
					ctx, deposits, deposit.OnWithdrawn,
					deposit.Withdrawn,
				)
				if err != nil {
					log.Errorf("Error transitioning "+
						"deposits: %v", err)
				}

				// Remove the withdrawal tx from the active
				// withdrawals to stop republishing it on block
				// arrivals.
				m.mu.Lock()
				delete(m.finalizedWithdrawalTxns, txHash)
				m.mu.Unlock()

				// Persist info about the finalized withdrawal.
				err = m.cfg.Store.UpdateWithdrawal(
					ctx, deposits, tx.Tx, spendingHeight,
					addrParams.PkScript,
				)
				if err != nil {
					log.Errorf("Error persisting "+
						"withdrawal: %v", err)
				}

			case err := <-errChan:
				log.Errorf("Error waiting for confirmation: %v",
					err)

			case <-ctx.Done():
				log.Errorf("Withdrawal tx confirmation wait " +
					"canceled")
			}

		case err := <-errChan:
			log.Errorf("Error waiting for confirmation: %v", err)

		case <-ctx.Done():
			log.Errorf("Withdrawal tx confirmation wait canceled")
		}
	}()

	return nil
}

func toOutpoints(deposits []*deposit.Deposit) []wire.OutPoint {
	outpoints := make([]wire.OutPoint, len(deposits))
	for i, d := range deposits {
		outpoints[i] = wire.OutPoint{
			Hash:  d.Hash,
			Index: d.Index,
		}
	}

	return outpoints
}

// signMusig2Tx adds the server nonces to the musig2 sessions and signs the
// transaction.
func (m *Manager) signMusig2Tx(ctx context.Context,
	prevOutFetcher *txscript.MultiPrevOutFetcher, outpoints []wire.OutPoint,
	signer lndclient.SignerClient, tx *wire.MsgTx,
	musig2sessions []*input.MuSig2SessionInfo,
	counterPartyNonces [][musig2.PubNonceSize]byte) ([][]byte, error) {

	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	sigs := make([][]byte, len(outpoints))

	for idx, outpoint := range outpoints {
		if !reflect.DeepEqual(tx.TxIn[idx].PreviousOutPoint,
			outpoint) {

			return nil, fmt.Errorf("tx input does not match " +
				"deposits")
		}

		taprootSigHash, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, tx, idx,
			prevOutFetcher,
		)
		if err != nil {
			return nil, err
		}

		var digest [32]byte
		copy(digest[:], taprootSigHash)

		// Register the server's nonce before attempting to create our
		// partial signature.
		haveAllNonces, err := signer.MuSig2RegisterNonces(
			ctx, musig2sessions[idx].SessionID,
			[][musig2.PubNonceSize]byte{counterPartyNonces[idx]},
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
		sig, err := signer.MuSig2Sign(
			ctx, musig2sessions[idx].SessionID, digest, false,
		)
		if err != nil {
			return nil, err
		}

		sigs[idx] = sig
	}

	return sigs, nil
}

func withdrawalValue(prevOuts map[wire.OutPoint]*wire.TxOut) btcutil.Amount {
	var totalValue btcutil.Amount
	for _, prevOut := range prevOuts {
		totalValue += btcutil.Amount(prevOut.Value)
	}
	return totalValue
}

// toNonces converts a byte slice to a 66 byte slice.
func toNonces(nonces [][]byte) ([][musig2.PubNonceSize]byte, error) {
	res := make([][musig2.PubNonceSize]byte, 0, len(nonces))
	for _, n := range nonces {
		nonce, err := byteSliceTo66ByteSlice(n)
		if err != nil {
			return nil, err
		}

		res = append(res, nonce)
	}

	return res, nil
}

// byteSliceTo66ByteSlice converts a byte slice to a 66 byte slice.
func byteSliceTo66ByteSlice(b []byte) ([musig2.PubNonceSize]byte, error) {
	if len(b) != musig2.PubNonceSize {
		return [musig2.PubNonceSize]byte{},
			fmt.Errorf("invalid byte slice length")
	}

	var res [musig2.PubNonceSize]byte
	copy(res[:], b)

	return res, nil
}

func (m *Manager) createWithdrawalTx(ctx context.Context,
	outpoints []wire.OutPoint, prevOuts map[wire.OutPoint]*wire.TxOut,
	selectedWithdrawalAmount btcutil.Amount, withdrawAddr btcutil.Address,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, btcutil.Amount,
	btcutil.Amount, error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	var (
		hasChange        bool
		dustLimit        = lnwallet.DustLimitForSize(input.P2TRSize)
		withdrawalAmount btcutil.Amount
		changeAmount     btcutil.Amount
	)

	// Estimate the transaction weight without change.
	weight, err := withdrawalTxWeight(len(outpoints), withdrawAddr, false)
	if err != nil {
		return nil, 0, 0, err
	}
	feeWithoutChange := feeRate.FeeForWeightRoundUp(weight)

	// If the user selected a fraction of the sum of the selected deposits
	// to withdraw, check if a change output is needed.
	totalWithdrawalAmount := withdrawalValue(prevOuts)
	if selectedWithdrawalAmount > 0 {
		// Estimate the transaction weight with change.
		weight, err = withdrawalTxWeight(
			len(outpoints), withdrawAddr, true,
		)
		if err != nil {
			return nil, 0, 0, err
		}
		feeWithChange := feeRate.FeeForWeightRoundUp(weight)

		// The available change that can cover fees is the total
		// selected deposit amount minus the selected withdrawal amount.
		change := totalWithdrawalAmount - selectedWithdrawalAmount

		switch {
		case change-feeWithChange >= dustLimit:
			// If the change can cover the fees without turning into
			// dust, add a non-dust change output.
			hasChange = true
			changeAmount = change - feeWithChange
			withdrawalAmount = selectedWithdrawalAmount

		case change-feeWithoutChange >= 0:
			// If the change is dust, we give it to the miners.
			hasChange = false
			withdrawalAmount = selectedWithdrawalAmount

		default:
			// If the fees eat into our withdrawal amount, we fail
			// the withdrawal.
			return nil, 0, 0, fmt.Errorf("the change doesn't " +
				"cover for fees. Consider lowering the fee " +
				"rate or decrease the withdrawal amount")
		}
	} else {
		// If the user wants to withdraw the full amount, we don't need
		// a change output.
		hasChange = false
		withdrawalAmount = totalWithdrawalAmount - feeWithoutChange
	}

	if withdrawalAmount < dustLimit {
		return nil, 0, 0, fmt.Errorf("withdrawal amount is below " +
			"dust limit")
	}

	if changeAmount < 0 {
		return nil, 0, 0, fmt.Errorf("change amount is negative")
	}

	// For the users convenience we check that the change amount is lower
	// than each input's value. If the change amount is higher than an
	// input's value, we wouldn't have to include that input into the
	// transaction, saving fees.
	for outpoint, txOut := range prevOuts {
		if changeAmount >= btcutil.Amount(txOut.Value) {
			return nil, 0, 0, fmt.Errorf("change amount %v is "+
				"higher than an input value %v of input %v",
				changeAmount, btcutil.Amount(txOut.Value),
				outpoint)
		}
	}

	withdrawScript, err := txscript.PayToAddrScript(withdrawAddr)
	if err != nil {
		return nil, 0, 0, err
	}

	// Create the withdrawal output.
	msgTx.AddTxOut(&wire.TxOut{
		Value:    int64(withdrawalAmount),
		PkScript: withdrawScript,
	})

	if hasChange {
		// Send change back to the same static address.
		staticAddress, err := m.cfg.AddressManager.GetStaticAddress(ctx)
		if err != nil {
			log.Errorf("error retrieving taproot address %v", err)

			return nil, 0, 0, fmt.Errorf("withdrawal failed")
		}

		changeAddress, err := btcutil.NewAddressTaproot(
			schnorr.SerializePubKey(staticAddress.TaprootKey),
			m.cfg.ChainParams,
		)
		if err != nil {
			return nil, 0, 0, err
		}

		changeScript, err := txscript.PayToAddrScript(changeAddress)
		if err != nil {
			return nil, 0, 0, err
		}

		msgTx.AddTxOut(&wire.TxOut{
			Value:    int64(changeAmount),
			PkScript: changeScript,
		})
	}

	return msgTx, withdrawalAmount, changeAmount, nil
}

// withdrawalFee returns the weight for the withdrawal transaction.
func withdrawalTxWeight(numInputs int, sweepAddress btcutil.Address,
	hasChange bool) (lntypes.WeightUnit, error) {

	var weightEstimator input.TxWeightEstimator
	for i := 0; i < numInputs; i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	// Get the weight of the sweep output.
	switch sweepAddress.(type) {
	case *btcutil.AddressWitnessPubKeyHash:
		weightEstimator.AddP2WKHOutput()

	case *btcutil.AddressTaproot:
		weightEstimator.AddP2TROutput()

	default:
		return 0, fmt.Errorf("invalid sweep address type %T",
			sweepAddress)
	}

	// If there's a change output add the weight of the static address.
	if hasChange {
		weightEstimator.AddP2TROutput()
	}

	return weightEstimator.Weight(), nil
}

// finalizeMusig2Transaction creates the finalized transactions for either
// the htlc or the cooperative close.
func (m *Manager) finalizeMusig2Transaction(ctx context.Context,
	outpoints []wire.OutPoint, signer lndclient.SignerClient,
	musig2Sessions []*input.MuSig2SessionInfo,
	tx *wire.MsgTx, serverSigs [][]byte) (*wire.MsgTx, error) {

	for idx := range outpoints {
		haveAllSigs, finalSig, err := signer.MuSig2CombineSig(
			ctx, musig2Sessions[idx].SessionID,
			[][]byte{serverSigs[idx]},
		)
		if err != nil {
			return nil, err
		}

		if !haveAllSigs {
			return nil, fmt.Errorf("missing sigs")
		}

		tx.TxIn[idx].Witness = wire.TxWitness{finalSig}
	}

	return tx, nil
}

func toPrevoutInfo(outpoints []wire.OutPoint) []*staticaddressrpc.PrevoutInfo {
	var result []*staticaddressrpc.PrevoutInfo
	for _, o := range outpoints {
		outP := o
		outpoint := &staticaddressrpc.PrevoutInfo{
			TxidBytes:   outP.Hash[:],
			OutputIndex: outP.Index,
		}
		result = append(result, outpoint)
	}

	return result
}

// createMusig2Sessions creates a musig2 session for a number of deposits.
func (m *Manager) createMusig2Sessions(ctx context.Context,
	deposits []*deposit.Deposit) ([]*input.MuSig2SessionInfo, [][]byte,
	error) {

	musig2Sessions := make([]*input.MuSig2SessionInfo, len(deposits))
	clientNonces := make([][]byte, len(deposits))

	// Create the sessions and nonces from the deposits.
	for i := 0; i < len(deposits); i++ {
		session, err := m.createMusig2Session(ctx)
		if err != nil {
			return nil, nil, err
		}

		musig2Sessions[i] = session
		clientNonces[i] = session.PublicNonce[:]
	}

	return musig2Sessions, clientNonces, nil
}

// Musig2CreateSession creates a musig2 session for the deposit.
func (m *Manager) createMusig2Session(ctx context.Context) (
	*input.MuSig2SessionInfo, error) {

	addressParams, err := m.cfg.AddressManager.GetStaticAddressParameters(
		ctx,
	)
	if err != nil {
		return nil, fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %w", err)
	}

	signers := [][]byte{
		addressParams.ClientPubkey.SerializeCompressed(),
		addressParams.ServerPubkey.SerializeCompressed(),
	}

	address, err := m.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %w", err)
	}

	expiryLeaf := address.TimeoutLeaf

	rootHash := expiryLeaf.TapHash()

	return m.cfg.Signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &addressParams.KeyLocator,
		signers, lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
	)
}

func (m *Manager) toPrevOuts(deposits []*deposit.Deposit,
	pkScript []byte) map[wire.OutPoint]*wire.TxOut {

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(deposits))
	for _, d := range deposits {
		outpoint := wire.OutPoint{
			Hash:  d.Hash,
			Index: d.Index,
		}
		txOut := &wire.TxOut{
			Value:    int64(d.Value),
			PkScript: pkScript,
		}
		prevOuts[outpoint] = txOut
	}

	return prevOuts
}

func (m *Manager) republishWithdrawals(ctx context.Context) error {
	m.mu.Lock()
	txns := make([]*wire.MsgTx, 0, len(m.finalizedWithdrawalTxns))
	for _, tx := range m.finalizedWithdrawalTxns {
		txns = append(txns, tx)
	}
	m.mu.Unlock()

	for _, finalizedTx := range txns {
		if finalizedTx == nil {
			log.Warnf("Finalized withdrawal tx is nil")
			continue
		}

		_, err := m.publishFinalizedWithdrawalTx(ctx, finalizedTx)
		if err != nil {
			log.Errorf("Error republishing withdrawal: %v", err)

			return err
		}
	}

	return nil
}

// DeliverWithdrawalRequest forwards a withdrawal request to the manager main
// loop.
func (m *Manager) DeliverWithdrawalRequest(ctx context.Context,
	outpoints []wire.OutPoint, destAddr string, satPerVbyte int64,
	amount int64) (string, string, error) {

	request := newWithdrawalRequest{
		outpoints:   outpoints,
		destAddr:    destAddr,
		satPerVbyte: satPerVbyte,
		amount:      amount,
		respChan:    make(chan *newWithdrawalResponse),
	}

	// Send the new loop-in request to the manager run loop.
	select {
	case m.newWithdrawalRequestChan <- request:

	case <-m.exitChan:
		return "", "", fmt.Errorf("withdrawal manager has been " +
			"canceled")

	case <-ctx.Done():
		return "", "", fmt.Errorf("context canceled while withdrawing")
	}

	// Wait for the response from the manager run loop.
	select {
	case resp := <-request.respChan:
		return resp.txHash, resp.withdrawalAddress, resp.err

	case <-m.exitChan:
		return "", "", fmt.Errorf("withdrawal manager has been " +
			"canceled")

	case <-ctx.Done():
		return "", "", fmt.Errorf("context canceled while waiting " +
			"for withdrawal response")
	}
}

// GetAllWithdrawals returns all finalized withdrawals from the store.
func (m *Manager) GetAllWithdrawals(ctx context.Context) ([]Withdrawal, error) {
	return m.cfg.Store.GetAllWithdrawals(ctx)
}
