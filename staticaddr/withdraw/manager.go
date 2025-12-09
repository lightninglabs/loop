package withdraw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
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
func NewManager(cfg *ManagerConfig, currentHeight uint32) (*Manager, error) {
	if currentHeight == 0 {
		return nil, fmt.Errorf("invalid current height %d",
			currentHeight)
	}

	m := &Manager{
		cfg:                      cfg,
		finalizedWithdrawalTxns:  make(map[chainhash.Hash]*wire.MsgTx),
		exitChan:                 make(chan struct{}),
		newWithdrawalRequestChan: make(chan newWithdrawalRequest),
		errChan:                  make(chan error),
	}
	m.initiationHeight.Store(currentHeight)

	return m, nil
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
	// requesting a fee bump, if not, we'll return an error as we only allow
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

	finalizedTx, _, err := m.CreateFinalizedWithdrawalTx(
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

// CreateFinalizedWithdrawalTx creates and signs a finalized withdrawal
// transaction that can be broadcast to the network. It returns the
// signed *wire.MsgTx representation and the unsigned psbt.
func (m *Manager) CreateFinalizedWithdrawalTx(ctx context.Context,
	deposits []*deposit.Deposit, withdrawalAddress btcutil.Address,
	satPerVbyte int64, selectedWithdrawalAmount int64) (*wire.MsgTx, []byte,
	error) {

	// Create a musig2 session for each deposit.
	addrParams, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, nil, err
	}

	staticAddress, err := m.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, nil, err
	}

	sessions, clientNonces, idx, err := staticutil.CreateMusig2SessionsPerDeposit(
		ctx, m.cfg.Signer, deposits, addrParams, staticAddress,
	)
	if err != nil {
		return nil, nil, err
	}

	var withdrawalSweepFeeRate chainfee.SatPerKWeight
	if satPerVbyte == 0 {
		// Get the fee rate for the withdrawal sweep.
		withdrawalSweepFeeRate, err = m.cfg.WalletKit.EstimateFeeRate(
			ctx, defaultConfTarget,
		)
		if err != nil {
			return nil, nil, err
		}
	} else {
		withdrawalSweepFeeRate = chainfee.SatPerKVByte(
			satPerVbyte * 1000,
		).FeePerKWeight()
	}

	params, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't get confirmation "+
			"height for deposit, %w", err)
	}

	outpoints := toOutpoints(deposits)
	prevOuts, err := staticutil.ToPrevOuts(deposits, params.PkScript)
	if err != nil {
		return nil, nil, err
	}

	withdrawalTx, unsignedPsbt, err := m.createWithdrawalTx(
		ctx, outpoints, deposits, prevOuts,
		btcutil.Amount(selectedWithdrawalAmount), withdrawalAddress,
		withdrawalSweepFeeRate,
	)
	if err != nil {
		return nil, nil, err
	}

	// Request the server to sign the withdrawal transaction.
	//
	// The withdrawal and change amount are sent to the server with the
	// expectation that the server just signs the transaction, without
	// performing fee calculations and dust considerations. The client is
	// responsible for that.
	// nolint:lll
	sigResp, err := m.cfg.StaticAddressServerClient.ServerPsbtWithdrawDeposits(
		ctx, &staticaddressrpc.ServerPsbtWithdrawRequest{
			WithdrawalPsbt:  unsignedPsbt,
			DepositToNonces: clientNonces,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	// Do some sanity checks.
	txHash := withdrawalTx.TxHash()
	if !bytes.Equal(txHash.CloneBytes(), sigResp.Txid) {
		return nil, nil, errors.New("txid doesn't match")
	}

	if len(sigResp.SigningInfo) != len(deposits) {
		return nil, nil, errors.New("invalid number of " +
			"deposit signatures")
	}

	// Verify 1:1 matching between deposits and SigningInfo entries.
	// Each deposit must have exactly one corresponding entry in
	// SigningInfo.
	for _, d := range deposits {
		depositKey := d.OutPoint.String()
		if _, ok := sigResp.SigningInfo[depositKey]; !ok {
			return nil, nil, fmt.Errorf("missing signature for "+
				"deposit %s", depositKey)
		}
	}

	// Next we'll get our sweep tx signatures.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	finalizedTx, err := m.signMusig2Tx(
		ctx, prevOutFetcher, m.cfg.Signer, withdrawalTx, sessions,
		sigResp.SigningInfo, idx,
	)
	if err != nil {
		return nil, nil, err
	}

	return finalizedTx, unsignedPsbt, nil
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
			log.Errorf("Error waiting for spending: %v", err)

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
	prevOutFetcher *txscript.MultiPrevOutFetcher,
	signer lndclient.SignerClient, tx *wire.MsgTx,
	sessions map[string]*input.MuSig2SessionInfo,
	sigInfo map[string]*staticaddressrpc.ServerPsbtWithdrawSigningInfo,
	depositsToIdx map[string]int) (*wire.MsgTx, error) {

	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Create our digest.
	var sigHash [32]byte

	if len(sigInfo) != len(depositsToIdx) {
		return nil, fmt.Errorf("unexpected number of partial " +
			"signatures from server")
	}

	for txIndex, input := range tx.TxIn {
		outpoint := input.PreviousOutPoint.String()
		if i, ok := depositsToIdx[outpoint]; ok {
			if i != txIndex {
				return nil, fmt.Errorf("deposit index maps " +
					"wrong tx index")
			}
			continue
		}

		return nil, fmt.Errorf("tx outpoint not in deposit index map")
	}

	// We'll now add the nonce to our session and sign the tx.
	for deposit, sigAndNonce := range sigInfo {
		session, ok := sessions[deposit]
		if !ok {
			return nil, errors.New("session not found")
		}

		nonce := [musig2.PubNonceSize]byte{}
		copy(nonce[:], sigAndNonce.Nonce)
		haveAllNonces, err := signer.MuSig2RegisterNonces(
			ctx, session.SessionID,
			[][musig2.PubNonceSize]byte{nonce},
		)
		if err != nil {
			return nil, fmt.Errorf("error registering nonces: "+
				"%w", err)
		}

		if !haveAllNonces {
			return nil, errors.New("expected all nonces to be " +
				"registered")
		}

		taprootSigHash, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, tx,
			depositsToIdx[deposit], prevOutFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("error calculating taproot "+
				"sig hash: %w", err)
		}

		copy(sigHash[:], taprootSigHash)

		// Sign the tx.
		_, err = signer.MuSig2Sign(
			ctx, session.SessionID, sigHash, false,
		)
		if err != nil {
			return nil, fmt.Errorf("error signing tx: %w", err)
		}

		// Combine the signature with the client signature.
		haveAllSigs, sig, err := signer.MuSig2CombineSig(
			ctx, session.SessionID,
			[][]byte{sigAndNonce.Sig},
		)
		if err != nil {
			return nil, fmt.Errorf("error combining signature: "+
				"%w", err)
		}

		if !haveAllSigs {
			return nil, errors.New("expected all signatures to " +
				"be combined")
		}

		tx.TxIn[depositsToIdx[deposit]].Witness = wire.TxWitness{
			sig,
		}
	}

	return tx, nil
}

func (m *Manager) createWithdrawalTx(ctx context.Context,
	outpoints []wire.OutPoint, deposits []*deposit.Deposit,
	prevOuts map[wire.OutPoint]*wire.TxOut,
	selectedWithdrawalAmount btcutil.Amount, withdrawAddr btcutil.Address,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, []byte, error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	withdrawalAmount, changeAmount, err := CalculateWithdrawalTxValues(
		deposits, selectedWithdrawalAmount, feeRate,
		withdrawAddr, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error calculating funding tx "+
			"values: %w", err)
	}

	// For the user's convenience, we check that the change amount is lower
	// than each input's value. If the change amount is higher than an
	// input's value, we wouldn't have to include that input in the
	// transaction, saving fees.
	for outpoint, txOut := range prevOuts {
		if changeAmount >= btcutil.Amount(txOut.Value) {
			return nil, nil, fmt.Errorf("change amount %v "+
				"is higher than an input value %v of input %v",
				changeAmount, btcutil.Amount(txOut.Value),
				outpoint)
		}
	}

	withdrawScript, err := txscript.PayToAddrScript(withdrawAddr)
	if err != nil {
		return nil, nil, err
	}

	// Create the withdrawal output.
	msgTx.AddTxOut(&wire.TxOut{
		Value:    int64(withdrawalAmount),
		PkScript: withdrawScript,
	})

	if changeAmount > 0 {
		// Send change back to the same static address.
		staticAddress, err := m.cfg.AddressManager.GetStaticAddress(ctx)
		if err != nil {
			log.Errorf("error retrieving taproot address %v", err)

			return nil, nil, fmt.Errorf("withdrawal failed")
		}

		changeAddress, err := btcutil.NewAddressTaproot(
			schnorr.SerializePubKey(staticAddress.TaprootKey),
			m.cfg.ChainParams,
		)
		if err != nil {
			return nil, nil, err
		}

		changeScript, err := txscript.PayToAddrScript(changeAddress)
		if err != nil {
			return nil, nil, err
		}

		msgTx.AddTxOut(&wire.TxOut{
			Value:    int64(changeAmount),
			PkScript: changeScript,
		})
	}

	// Create psbt for the withdrawal.
	psbtx, err := psbt.NewFromUnsignedTx(msgTx)
	if err != nil {
		return nil, nil, err
	}

	pInputs := make([]psbt.PInput, len(outpoints))
	for i, op := range outpoints {
		prevOut := prevOuts[op]
		pInputs[i] = psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    prevOut.Value,
				PkScript: prevOut.PkScript,
			},
		}
	}
	psbtx.Inputs = pInputs

	// Serialize the psbt to send it to the client.
	var psbtBuf bytes.Buffer
	err = psbtx.Serialize(&psbtBuf)
	if err != nil {
		return nil, nil, err
	}

	return msgTx, psbtBuf.Bytes(), nil
}

func CalculateWithdrawalTxValues(deposits []*deposit.Deposit,
	localAmount btcutil.Amount, feeRate chainfee.SatPerKWeight,
	withdrawalAddress btcutil.Address,
	commitmentType lnrpc.CommitmentType) (btcutil.Amount, btcutil.Amount,
	error) {

	if withdrawalAddress == nil &&
		commitmentType == lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {

		return 0, 0, fmt.Errorf("either address or commitment type " +
			"must be specified")
	}

	var (
		err                  error
		withdrawalFundingAmt btcutil.Amount
		changeAmount         btcutil.Amount
		dustLimit            = lnwallet.DustLimitForSize(input.P2TRSize)
		isChannelOpen        = commitmentType != lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE
	)

	totalDepositAmount := btcutil.Amount(0)
	for _, d := range deposits {
		totalDepositAmount += d.Value
	}

	// Estimate the open channel transaction fee without change.
	hasChange := false
	weight, err := WithdrawalTxWeight(
		len(deposits), withdrawalAddress, commitmentType, hasChange,
	)
	if err != nil {
		return 0, 0, err
	}
	feeWithoutChange := feeRate.FeeForWeight(weight)

	// If the user selected a local amount for the channel, check if a
	// change output is needed.
	if localAmount > 0 {
		// Estimate the transaction weight with change.
		hasChange = true
		weightWithChange, err := WithdrawalTxWeight(
			len(deposits), withdrawalAddress, commitmentType,
			hasChange,
		)
		if err != nil {
			return 0, 0, err
		}
		feeWithChange := feeRate.FeeForWeight(weightWithChange)

		// The available change that can cover fees is the total
		// selected deposit amount minus the local channel amount.
		change := totalDepositAmount - localAmount

		switch {
		case change-feeWithChange >= dustLimit:
			// If the change can cover the fees without turning into
			// dust, add a non-dust change output.
			changeAmount = change - feeWithChange
			withdrawalFundingAmt = localAmount

		case change-feeWithoutChange >= 0:
			// If the change is dust, we give it to the miners.
			withdrawalFundingAmt = localAmount

		default:
			// If the fees eat into our local channel amount, we
			// fail to open the channel.
			return 0, 0, fmt.Errorf("the change doesn't " +
				"cover for fees. Consider lowering the fee " +
				"rate or decrease the local amount")
		}
	} else {
		// If the user wants to open the channel with the total value of
		// deposits, we don't need a change output.
		withdrawalFundingAmt = totalDepositAmount - feeWithoutChange
	}

	if withdrawalFundingAmt < dustLimit {
		return 0, 0, fmt.Errorf("withdrawal amount is below dust limit")
	}

	if changeAmount < 0 {
		return 0, 0, fmt.Errorf("change amount is negative")
	}

	// Ensure that the channel funding amount is at least in the amount of
	// lnd's minimum channel size.
	if isChannelOpen && withdrawalFundingAmt < funding.MinChanFundingSize {
		return 0, 0, fmt.Errorf("channel funding amount %v is lower "+
			"than the minimum channel funding size %v",
			withdrawalFundingAmt, funding.MinChanFundingSize)
	}

	// For the user's convenience, we check that the change amount is lower
	// than each input's value. If the change amount is higher than an
	// input's value, we wouldn't have to include that input in the
	// transaction, saving fees.
	for _, d := range deposits {
		if changeAmount >= d.Value {
			return 0, 0, fmt.Errorf("change amount %v is "+
				"higher than an input value %v of input %v",
				changeAmount, d.Value, d.OutPoint.String())
		}
	}

	return withdrawalFundingAmt, changeAmount, nil
}

// WithdrawalTxWeight returns the weight for the withdrawal transaction.
func WithdrawalTxWeight(numInputs int, sweepAddress btcutil.Address,
	commitmentType lnrpc.CommitmentType,
	hasChange bool) (lntypes.WeightUnit, error) {

	var weightEstimator input.TxWeightEstimator
	for i := 0; i < numInputs; i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	if commitmentType != lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
		switch commitmentType {
		case lnrpc.CommitmentType_SIMPLE_TAPROOT:
			weightEstimator.AddP2TROutput()

		default:
			weightEstimator.AddP2WSHOutput()
		}
	} else {
		// Get the weight of the sweep output.
		switch sweepAddress.(type) {
		case *btcutil.AddressWitnessPubKeyHash:
			weightEstimator.AddP2WKHOutput()

		case *btcutil.AddressWitnessScriptHash:
			weightEstimator.AddP2WSHOutput()

		case *btcutil.AddressTaproot:
			weightEstimator.AddP2TROutput()

		default:
			return 0, fmt.Errorf("invalid sweep address type %T",
				sweepAddress)
		}
	}

	// If there's a change output add the weight of the static address.
	if hasChange {
		weightEstimator.AddP2TROutput()
	}

	return weightEstimator.Weight(), nil
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
