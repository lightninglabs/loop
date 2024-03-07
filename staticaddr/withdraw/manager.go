package withdraw

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrClosingInactiveDeposits is returned when the user tries to close
	// inactive deposits.
	ErrClosingInactiveDeposits = errors.New("deposits to be closed are " +
		"unknown or inactive")

	// MinConfs is the minimum number of confirmations we require for a
	// deposit to be considered available for loop-ins, coop-spends and
	// timeouts.
	MinConfs int32 = 3

	// Is the default confirmation target for the withdrawal transaction.
	defaultConfTarget int32 = 3
)

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	WithdrawalServerClient staticaddressrpc.WithdrawalServerClient

	AddressManager AddressManager

	DepositManager DepositManager

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

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
}

// NewManager creates a new deposit manager.
func NewManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:      cfg,
		initChan: make(chan struct{}),
	}
}

// Run runs the deposit closure manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.runCtx = ctx

	m.Lock()
	m.currentHeight, m.initiationHeight = currentHeight, currentHeight
	m.Unlock()

	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(m.runCtx) //nolint:lll
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

		case err := <-newBlockErrChan:
			return err

		case <-m.runCtx.Done():
			return m.runCtx.Err()
		}
	}
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
		return fmt.Errorf("no outpoints selected for closure")
	}

	// Ensure that the deposits are in a state in which they can be
	// withdrawn.
	deposits, allActive := m.cfg.DepositManager.AllDepositsActive(outpoints)
	if !allActive {
		return ErrClosingInactiveDeposits
	}

	err := m.cfg.DepositManager.TransitionDeposits(
		outpoints, deposit.OnWithdraw, deposit.Withdrawing,
	)
	if err != nil {
		return err
	}

	// Create a musig2 session for each deposit.
	withdrawalSessions, clientNonces, err := m.createMusig2Sessions(
		ctx, deposits,
	)
	if err != nil {
		return err
	}

	// Get the fee rate for the withdrawal sweep.
	withdrawalSweepFeeRate, err := m.cfg.WalletKit.EstimateFeeRate(
		m.runCtx, defaultConfTarget,
	)
	if err != nil {
		return err
	}

	// Generate closing address from our local lnd wallet
	closingAddress, err := m.cfg.WalletKit.NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return err
	}

	resp, err := m.cfg.WithdrawalServerClient.WithdrawDeposits(
		m.runCtx,
		&staticaddressrpc.ServerWithdrawRequest{
			Outpoints:       toServerOutpoints(outpoints),
			ClientNonces:    clientNonces,
			ClientSweepAddr: closingAddress.String(),
			MusigTxFeeRate:  uint64(withdrawalSweepFeeRate),
		},
	)
	if err != nil {
		return err
	}

	addressParams, err := m.cfg.AddressManager.GetStaticAddressParameters(
		ctx,
	)
	if err != nil {
		return fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %v", err)
	}

	prevOuts := m.toPrevOuts(deposits, addressParams.PkScript)
	totalValue := withdrawalValue(prevOuts)
	withdrawalTx, err := m.createWithdrawalTx(
		prevOuts, totalValue, closingAddress, withdrawalSweepFeeRate,
	)
	if err != nil {
		return err
	}

	coopServerNonces, err := toNonces(resp.ServerNonces)
	if err != nil {
		return err
	}

	// Next we'll get our sweep tx signatures.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	_, err = m.signMusig2Tx(
		m.runCtx, prevOutFetcher, outpoints, m.cfg.Signer, withdrawalTx,
		withdrawalSessions, coopServerNonces,
	)
	if err != nil {
		return err
	}

	// Now we'll finalize the sweepless sweep transaction.
	finalizedWithdrawalTx, err := m.finalizeMusig2Transaction(
		m.runCtx, outpoints, m.cfg.Signer, withdrawalSessions,
		withdrawalTx, resp.Musig2SweepSigs,
	)
	if err != nil {
		return err
	}

	txLabel := fmt.Sprintf("deposit-withdrawal-%v",
		finalizedWithdrawalTx.TxHash())

	// Publish the sweepless sweep transaction.
	err = m.cfg.WalletKit.PublishTransaction(
		m.runCtx, finalizedWithdrawalTx, txLabel,
	)
	if err != nil {
		return err
	}

	txHash := finalizedWithdrawalTx.TxHash()
	pkScript, err := txscript.PayToAddrScript(closingAddress)
	if err != nil {
		return err
	}
	confChan, errChan, err := m.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		m.runCtx, &txHash, pkScript, MinConfs,
		int32(m.initiationHeight),
	)
	if err != nil {
		return err
	}

	go func() {
		select {
		case <-confChan:
			err = m.cfg.DepositManager.TransitionDeposits(
				outpoints, deposit.OnWithdrawn,
				deposit.Withdrawn,
			)
			if err != nil {
				log.Errorf("Error transitioning deposits: %v",
					err)
			}

		case err := <-errChan:
			log.Errorf("Error waiting for confirmation: %v", err)

		case <-m.runCtx.Done():
			log.Errorf("Withdrawal tx confirmation wait canceled")
		}
	}()

	return err
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
func toNonces(nonces [][]byte) ([][66]byte, error) {
	res := make([][66]byte, 0, len(nonces))
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
func byteSliceTo66ByteSlice(b []byte) ([66]byte, error) {
	if len(b) != 66 {
		return [66]byte{}, fmt.Errorf("invalid byte slice length")
	}

	var res [66]byte
	copy(res[:], b)

	return res, nil
}

func (m *Manager) createWithdrawalTx(prevOuts map[wire.OutPoint]*wire.TxOut,
	withdrawlAmount btcutil.Amount, clientSweepAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction.
	for o, _ := range prevOuts {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: o,
		})
	}

	// Estimate the fee
	weight, err := withdrawalFee(len(prevOuts), clientSweepAddress)
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
func withdrawalFee(numInputs int, sweepAddress btcutil.Address) (int64,
	error) {

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

	return int64(weightEstimator.Weight()), nil
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

func toServerOutpoints(outpoints []wire.OutPoint) []*staticaddressrpc.ServerOutPoint { //nolint:lll
	var result []*staticaddressrpc.ServerOutPoint
	for _, o := range outpoints {
		outP := o
		outpoint := &staticaddressrpc.ServerOutPoint{
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
