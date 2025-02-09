package withdraw

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrWithdrawingInactiveDeposits is returned when the user tries to
	// withdraw inactive deposits.
	ErrWithdrawingInactiveDeposits = errors.New("deposits to be " +
		"withdrawn are unknown or inactive")

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
	txHash             string
	withdrawalPkScript string
	err                error
}

// Manager manages the withdrawal state machines.
type Manager struct {
	cfg *ManagerConfig

	// initChan signals the daemon that the withdrawal manager has completed
	// its initialization.
	initChan chan struct{}

	// newWithdrawalRequestChan receives a list of outpoints that should be
	// withdrawn. The request is forwarded to the managers main loop.
	newWithdrawalRequestChan chan newWithdrawalRequest

	// exitChan signals subroutines that the withdrawal manager is exiting.
	exitChan chan struct{}

	// errChan forwards errors from the withdrawal manager to the server.
	errChan chan error

	// initiationHeight stores the currently best known block height.
	initiationHeight uint32

	// finalizedWithdrawalTx are the finalized withdrawal transactions that
	// are published to the network and re-published on block arrivals.
	finalizedWithdrawalTxns map[chainhash.Hash]*wire.MsgTx
}

// NewManager creates a new deposit withdrawal manager.
func NewManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:                      cfg,
		initChan:                 make(chan struct{}),
		finalizedWithdrawalTxns:  make(map[chainhash.Hash]*wire.MsgTx),
		exitChan:                 make(chan struct{}),
		newWithdrawalRequestChan: make(chan newWithdrawalRequest),
		errChan:                  make(chan error),
	}
}

// Run runs the deposit withdrawal manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.initiationHeight = currentHeight

	newBlockChan, newBlockErrChan, err :=
		m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx)

	if err != nil {
		return err
	}

	err = m.recoverWithdrawals(ctx)
	if err != nil {
		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	var (
		txHash   string
		pkScript string
	)
	for {
		select {
		case <-newBlockChan:
			err = m.republishWithdrawals(ctx)
			if err != nil {
				log.Errorf("Error republishing withdrawals: %v",
					err)
			}

		case req := <-m.newWithdrawalRequestChan:
			txHash, pkScript, err = m.WithdrawDeposits(
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
				txHash:             txHash,
				withdrawalPkScript: pkScript,
				err:                err,
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
	// To recover withdrawals we skim through all active deposits and check
	// if they have a withdrawal address set. For the ones that do we
	// cluster those with equal withdrawal addresses and kick-off
	// their withdrawal. Each cluster represents a separate withdrawal
	// intent by the user.
	activeDeposits, err := m.cfg.DepositManager.GetActiveDepositsInState(
		deposit.Deposited,
	)
	if err != nil {
		return err
	}

	// Group the deposits by their finalized withdrawal transaction.
	depositsByWithdrawalTx := make(map[*wire.MsgTx][]*deposit.Deposit)
	for _, d := range activeDeposits {
		withdrawalTx := d.FinalizedWithdrawalTx
		if withdrawalTx == nil {
			continue
		}

		depositsByWithdrawalTx[withdrawalTx] = append(
			depositsByWithdrawalTx[withdrawalTx], d,
		)
	}

	// We can now reinstate each cluster of deposits for a withdrawal.
	for finalizedWithdrawalTx, deposits := range depositsByWithdrawalTx {
		tx := finalizedWithdrawalTx
		err = m.cfg.DepositManager.TransitionDeposits(
			ctx, deposits, deposit.OnWithdrawInitiated,
			deposit.Withdrawing,
		)
		if err != nil {
			return err
		}

		err = m.publishFinalizedWithdrawalTx(ctx, tx)
		if err != nil {
			return err
		}

		err = m.handleWithdrawal(
			ctx, deposits, tx.TxHash(), tx.TxOut[0].PkScript,
		)
		if err != nil {
			return err
		}

		m.finalizedWithdrawalTxns[tx.TxHash()] = tx
	}

	return nil
}

// WaitInitComplete waits until the address manager has completed its setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Static address withdrawal manager initiation " +
		"complete.")

	<-m.initChan
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

	// Ensure that the deposits are in a state in which they can be
	// withdrawn.
	deposits, allActive := m.cfg.DepositManager.AllOutpointsActiveDeposits(
		outpoints, deposit.Deposited,
	)

	if !allActive {
		return "", "", ErrWithdrawingInactiveDeposits
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

	// Attach the finalized withdrawal tx to the deposits. After a client
	// restart we can use this address as an indicator to republish the
	// withdrawal tx and continue the withdrawal.
	// Deposits with the same withdrawal tx are part of the same withdrawal.
	for _, d := range deposits {
		d.Lock()
		d.FinalizedWithdrawalTx = finalizedTx
		d.Unlock()
	}

	// Transition the deposits to the withdrawing state. This updates each
	// deposits withdrawal address. If a transition fails, we'll return an
	// error and abort the withdrawal. An error in transition is likely due
	// to an error in the state machine. The already transitioned deposits
	// should be reset to the Deposit state after a restart.
	err = m.cfg.DepositManager.TransitionDeposits(
		ctx, deposits, deposit.OnWithdrawInitiated, deposit.Withdrawing,
	)
	if err != nil {
		return "", "", err
	}

	err = m.publishFinalizedWithdrawalTx(ctx, finalizedTx)
	if err != nil {
		return "", "", err
	}

	withdrawalPkScript, err := txscript.PayToAddrScript(withdrawalAddress)
	if err != nil {
		return "", "", err
	}

	err = m.handleWithdrawal(
		ctx, deposits, finalizedTx.TxHash(), withdrawalPkScript,
	)
	if err != nil {
		return "", "", err
	}

	m.finalizedWithdrawalTxns[finalizedTx.TxHash()] = finalizedTx

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
			Outpoints:            toPrevoutInfo(outpoints),
			ClientNonces:         clientNonces,
			ClientWithdrawalAddr: withdrawalAddress.String(),
			WithdrawAmount:       int64(withdrawAmount),
			ChangeAmount:         int64(changeAmount),
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
	tx *wire.MsgTx) error {

	if tx == nil {
		return errors.New("can't publish, finalized withdrawal tx is " +
			"nil")
	}

	txLabel := fmt.Sprintf("deposit-withdrawal-%v", tx.TxHash())

	// Publish the withdrawal sweep transaction.
	err := m.cfg.WalletKit.PublishTransaction(ctx, tx, txLabel)

	if err != nil {
		if !strings.Contains(err.Error(), "output already spent") {
			log.Errorf("%v: %v", txLabel, err)
		}
	}

	log.Debugf("published deposit withdrawal with txid: %v", tx.TxHash())

	return nil
}

func (m *Manager) handleWithdrawal(ctx context.Context,
	deposits []*deposit.Deposit, txHash chainhash.Hash,
	withdrawalPkScript []byte) error {

	confChan, errChan, err := m.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, &txHash, withdrawalPkScript, MinConfs,
		int32(m.initiationHeight),
	)
	if err != nil {
		return err
	}

	go func() {
		select {
		case <-confChan:
			err = m.cfg.DepositManager.TransitionDeposits(
				ctx, deposits, deposit.OnWithdrawn,
				deposit.Withdrawn,
			)
			if err != nil {
				log.Errorf("Error transitioning deposits: %v",
					err)
			}

			// Remove the withdrawal from the active withdrawals and
			// remove its finalized to stop republishing it on block
			// arrivals.
			delete(m.finalizedWithdrawalTxns, txHash)

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
		n := n
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
	feeWithoutChange := feeRate.FeeForWeight(weight)

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
		feeWithChange := feeRate.FeeForWeight(weight)

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
			log.Errorf("error retrieving taproot address %w", err)

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
	for _, finalizedTx := range m.finalizedWithdrawalTxns {
		if finalizedTx == nil {
			log.Warnf("Finalized withdrawal tx is nil")
			continue
		}

		err := m.publishFinalizedWithdrawalTx(ctx, finalizedTx)
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
		return resp.txHash, resp.withdrawalPkScript, resp.err

	case <-m.exitChan:
		return "", "", fmt.Errorf("withdrawal manager has been " +
			"canceled")

	case <-ctx.Done():
		return "", "", fmt.Errorf("context canceled while waiting " +
			"for withdrawal response")
	}
}
