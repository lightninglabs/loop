package withdraw

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

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

// Manager manages the address state machines.
type Manager struct {
	cfg *ManagerConfig

	runCtx context.Context

	sync.Mutex

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

	// initiationHeight stores the currently best known block height.
	initiationHeight uint32

	// currentHeight stores the currently best known block height.
	currentHeight uint32

	// activeWithdrawals stores pending withdrawals by their withdrawal
	// address.
	activeWithdrawals map[string][]*deposit.Deposit

	// finalizedWithdrawalTx are the finalized withdrawal transactions that
	// are published to the network and re-published on block arrivals.
	finalizedWithdrawalTxns map[chainhash.Hash]*wire.MsgTx
}

// NewManager creates a new deposit withdrawal manager.
func NewManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:                     cfg,
		initChan:                make(chan struct{}),
		activeWithdrawals:       make(map[string][]*deposit.Deposit),
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
}

// Run runs the deposit withdrawal manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.runCtx = ctx

	m.Lock()
	m.currentHeight, m.initiationHeight = currentHeight, currentHeight
	m.Unlock()

	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(m.runCtx) //nolint:lll
	if err != nil {
		return err
	}

	err = m.recover()
	if err != nil {
		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	for {
		select {
		case height := <-newBlockChan:
			m.Lock()
			m.currentHeight = uint32(height)
			m.Unlock()

			err = m.republishWithdrawals()
			if err != nil {
				log.Errorf("Error republishing withdrawals: %v",
					err)
			}

		case err = <-newBlockErrChan:
			return err

		case <-m.runCtx.Done():
			return m.runCtx.Err()
		}
	}
}

func (m *Manager) recover() error {
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

	// Group the deposits by their withdrawal address.
	depositsByWithdrawalAddress := make(map[string][]*deposit.Deposit)
	for _, d := range activeDeposits {
		sweepAddress := d.WithdrawalSweepAddress
		if sweepAddress == "" {
			continue
		}

		depositsByWithdrawalAddress[sweepAddress] = append(
			depositsByWithdrawalAddress[sweepAddress], d,
		)
	}

	// We can now reinstate each cluster of deposits for a withdrawal.
	for address, deposits := range depositsByWithdrawalAddress {
		withdrawalAddress, err := btcutil.DecodeAddress(
			address, m.cfg.ChainParams,
		)
		if err != nil {
			return err
		}

		err = m.cfg.DepositManager.TransitionDeposits(
			deposits, deposit.OnWithdrawInitiated,
			deposit.Withdrawing,
		)
		if err != nil {
			return err
		}

		finalizedTx, err := m.createFinalizedWithdrawalTx(
			m.runCtx, deposits, withdrawalAddress,
		)
		if err != nil {
			return err
		}

		err = m.publishFinalizedWithdrawalTx(finalizedTx)
		if err != nil {
			return err
		}

		err = m.handleWithdrawal(
			deposits, finalizedTx.TxHash(), withdrawalAddress,
		)
		if err != nil {
			return err
		}

		m.Lock()
		m.activeWithdrawals[address] = deposits
		m.Unlock()
	}

	return nil
}

// WaitInitComplete waits until the address manager has completed its setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Static address withdrawal manager initiation " +
		"complete.")

	<-m.initChan
}

// WithdrawDeposits starts a deposits withdrawal flow.
func (m *Manager) WithdrawDeposits(ctx context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return fmt.Errorf("no outpoints selected to withdraw")
	}

	// Ensure that the deposits are in a state in which they can be
	// withdrawn.
	deposits, allActive := m.cfg.DepositManager.AllOutpointsActiveDeposits(
		outpoints, deposit.Deposited)

	if !allActive {
		return ErrWithdrawingInactiveDeposits
	}

	// Generate the withdrawal address from our local lnd wallet.
	withdrawalAddress, err := m.cfg.WalletKit.NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return err
	}

	// Attach the withdrawal address to the deposits. After a client restart
	// we can use this address as an indicator to continue the withdrawal.
	// If there are multiple deposits with the same withdrawal address, we
	// bundle them together in the same withdrawal transaction.
	for _, d := range deposits {
		d.Lock()
		d.WithdrawalSweepAddress = withdrawalAddress.String()
		d.Unlock()
	}

	// Transition the deposits to the withdrawing state. This updates each
	// deposits withdrawal address. If a transition fails, we'll return an
	// error and abort the withdrawal. An error in transition is likely due
	// to an error in the state machine. The already transitioned deposits
	// should be reset to the Deposit state after a restart.
	err = m.cfg.DepositManager.TransitionDeposits(
		deposits, deposit.OnWithdrawInitiated, deposit.Withdrawing,
	)
	if err != nil {
		return err
	}

	finalizedTx, err := m.createFinalizedWithdrawalTx(
		ctx, deposits, withdrawalAddress,
	)
	if err != nil {
		return err
	}

	err = m.publishFinalizedWithdrawalTx(finalizedTx)
	if err != nil {
		return err
	}

	err = m.handleWithdrawal(
		deposits, finalizedTx.TxHash(), withdrawalAddress,
	)
	if err != nil {
		return err
	}

	m.Lock()
	m.activeWithdrawals[withdrawalAddress.String()] = deposits
	m.Unlock()

	return nil
}

func (m *Manager) createFinalizedWithdrawalTx(ctx context.Context,
	deposits []*deposit.Deposit, withdrawalAddress btcutil.Address) (
	*wire.MsgTx, error) {

	// Create a musig2 session for each deposit.
	withdrawalSessions, clientNonces, err := m.createMusig2Sessions(
		ctx, deposits,
	)
	if err != nil {
		return nil, err
	}

	// Get the fee rate for the withdrawal sweep.
	withdrawalSweepFeeRate, err := m.cfg.WalletKit.EstimateFeeRate(
		m.runCtx, defaultConfTarget,
	)
	if err != nil {
		return nil, err
	}

	outpoints := toOutpoints(deposits)
	resp, err := m.cfg.StaticAddressServerClient.ServerWithdrawDeposits(
		m.runCtx, &staticaddressrpc.ServerWithdrawRequest{
			Outpoints:       toPrevoutInfo(outpoints),
			ClientNonces:    clientNonces,
			ClientSweepAddr: withdrawalAddress.String(),
			TxFeeRate:       uint64(withdrawalSweepFeeRate),
		},
	)
	if err != nil {
		return nil, err
	}

	addressParams, err := m.cfg.AddressManager.GetStaticAddressParameters(
		ctx,
	)
	if err != nil {
		return nil, fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %v", err)
	}

	prevOuts := m.toPrevOuts(deposits, addressParams.PkScript)
	totalValue := withdrawalValue(prevOuts)
	withdrawalTx, err := m.createWithdrawalTx(
		outpoints, totalValue, withdrawalAddress,
		withdrawalSweepFeeRate,
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
		m.runCtx, prevOutFetcher, outpoints, m.cfg.Signer, withdrawalTx,
		withdrawalSessions, coopServerNonces,
	)
	if err != nil {
		return nil, err
	}

	// Now we'll finalize the sweepless sweep transaction.
	finalizedTx, err := m.finalizeMusig2Transaction(
		m.runCtx, outpoints, m.cfg.Signer, withdrawalSessions,
		withdrawalTx, resp.Musig2SweepSigs,
	)
	if err != nil {
		return nil, err
	}

	m.finalizedWithdrawalTxns[finalizedTx.TxHash()] = finalizedTx

	return finalizedTx, nil
}

func (m *Manager) publishFinalizedWithdrawalTx(tx *wire.MsgTx) error {
	if tx == nil {
		return errors.New("can't publish, finalized withdrawal tx is " +
			"nil")
	}

	txLabel := fmt.Sprintf("deposit-withdrawal-%v", tx.TxHash())

	// Publish the withdrawal sweep transaction.
	err := m.cfg.WalletKit.PublishTransaction(m.runCtx, tx, txLabel)

	if err != nil {
		if !strings.Contains(err.Error(), "output already spent") {
			log.Errorf("%v: %v", txLabel, err)
		}
	}

	log.Debugf("published deposit withdrawal with txid: %v", tx.TxHash())

	return nil
}

func (m *Manager) handleWithdrawal(deposits []*deposit.Deposit,
	txHash chainhash.Hash, withdrawalAddress btcutil.Address) error {

	withdrawalPkScript, err := txscript.PayToAddrScript(withdrawalAddress)
	if err != nil {
		return err
	}

	m.Lock()
	confChan, errChan, err := m.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		m.runCtx, &txHash, withdrawalPkScript, MinConfs,
		int32(m.initiationHeight),
	)
	m.Unlock()
	if err != nil {
		return err
	}

	go func() {
		select {
		case <-confChan:
			err = m.cfg.DepositManager.TransitionDeposits(
				deposits, deposit.OnWithdrawn,
				deposit.Withdrawn,
			)
			if err != nil {
				log.Errorf("Error transitioning deposits: %v",
					err)
			}

			// Remove the withdrawal from the active withdrawals and
			// remove its finalized to stop republishing it on block
			// arrivals.
			m.Lock()
			delete(m.activeWithdrawals, withdrawalAddress.String())
			delete(m.finalizedWithdrawalTxns, txHash)
			m.Unlock()

		case err := <-errChan:
			log.Errorf("Error waiting for confirmation: %v", err)

		case <-m.runCtx.Done():
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

func (m *Manager) createWithdrawalTx(outpoints []wire.OutPoint,
	withdrawlAmount btcutil.Amount, clientSweepAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx,
	error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	// Estimate the fee.
	weight, err := withdrawalFee(len(outpoints), clientSweepAddress)
	if err != nil {
		return nil, err
	}

	pkscript, err := txscript.PayToAddrScript(clientSweepAddress)
	if err != nil {
		return nil, err
	}

	fee := feeRate.FeeForWeight(weight)

	// Create the sweep output
	sweepOutput := &wire.TxOut{
		Value:    int64(withdrawlAmount) - int64(fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

// withdrawalFee returns the weight for the withdrawal transaction.
func withdrawalFee(numInputs int,
	sweepAddress btcutil.Address) (lntypes.WeightUnit, error) {

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
			"deposit, %v", err)
	}

	signers := [][]byte{
		addressParams.ClientPubkey.SerializeCompressed(),
		addressParams.ServerPubkey.SerializeCompressed(),
	}

	address, err := m.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %v", err)
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

func (m *Manager) republishWithdrawals() error {
	for _, finalizedTx := range m.finalizedWithdrawalTxns {
		if finalizedTx == nil {
			log.Warnf("Finalized withdrawal tx is nil")
			continue
		}

		err := m.publishFinalizedWithdrawalTx(finalizedTx)
		if err != nil {
			log.Errorf("Error republishing withdrawal: %v", err)

			return err
		}
	}

	return nil
}
