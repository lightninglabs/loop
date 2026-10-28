package withdraw

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

func (m *Manager) withdrawalAddress(ctx context.Context, destAddr string,
	prevWithdrawalTx *wire.MsgTx) (btcutil.Address, error) {

	var prevPkScript []byte
	if prevWithdrawalTx != nil {
		// The withdrawal output is always the first output.
		if len(prevWithdrawalTx.TxOut) == 0 {
			return nil, errors.New("previous withdrawal tx has no " +
				"outputs")
		}
		prevPkScript = prevWithdrawalTx.TxOut[0].PkScript
	}

	switch {
	case destAddr != "":
		address, err := btcutil.DecodeAddress(
			destAddr, m.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}
		if prevPkScript == nil {
			return address, nil
		}

		pkScript, err := txscript.PayToAddrScript(address)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(pkScript, prevPkScript) {
			return nil, ErrDiffWithdrawalAddress
		}

		return address, nil

	case prevPkScript != nil:
		_, addresses, _, err := txscript.ExtractPkScriptAddrs(
			prevPkScript, m.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}
		if len(addresses) != 1 {
			return nil, errors.New("unable to decode the address " +
				"of the previous withdrawal tx")
		}

		return addresses[0], nil

	default:
		return m.cfg.WalletKit.NextAddr(
			ctx, lnwallet.DefaultAccountName,
			walletrpc.AddressType_TAPROOT_PUBKEY, false,
		)
	}
}

// CreateFinalizedWithdrawalTx creates and signs a finalized withdrawal
// transaction that can be broadcast to the network. It returns the
// signed *wire.MsgTx representation and the unsigned psbt.
func (m *Manager) CreateFinalizedWithdrawalTx(ctx context.Context,
	deposits []*deposit.Deposit, withdrawalAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight,
	selectedWithdrawalAmount int64,
	commitmentType lnrpc.CommitmentType) (*wire.MsgTx, []byte, error) {

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
		feeRate, commitmentType,
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
		if sigAndNonce == nil {
			return nil, fmt.Errorf("missing signing info for "+
				"deposit %v", deposit)
		}

		session, ok := sessions[deposit]
		if !ok {
			return nil, errors.New("session not found")
		}

		if len(sigAndNonce.Nonce) != musig2.PubNonceSize {
			return nil, fmt.Errorf("invalid nonce length for "+
				"deposit %v: got %d, want %d", deposit,
				len(sigAndNonce.Nonce), musig2.PubNonceSize)
		}

		if len(sigAndNonce.Sig) != input.MuSig2PartialSigSize {
			return nil, fmt.Errorf("invalid partial signature "+
				"length for deposit %v: got %d, want %d",
				deposit, len(sigAndNonce.Sig),
				input.MuSig2PartialSigSize)
		}

		var nonce [musig2.PubNonceSize]byte
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
	feeRate chainfee.SatPerKWeight,
	commitmentType lnrpc.CommitmentType) (*wire.MsgTx, []byte, error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
			Sequence:         wire.MaxTxInSequenceNum - 2,
		})
	}

	withdrawalAmount, changeAmount, err := CalculateWithdrawalTxValues(
		deposits, selectedWithdrawalAmount, feeRate,
		withdrawAddr, commitmentType,
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

// CalculateWithdrawalTxValues calculates the values of the withdrawal
// transaction. It returns the withdrawal amount, the change amount, and an
// error if any.
func CalculateWithdrawalTxValues(deposits []*deposit.Deposit,
	selectedAmount btcutil.Amount, feeRate chainfee.SatPerKWeight,
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

	// Estimate the withdrawal transaction fee without change.
	hasChange := false
	weight, err := WithdrawalTxWeight(
		len(deposits), withdrawalAddress, commitmentType, hasChange,
	)
	if err != nil {
		return 0, 0, err
	}
	feeWithoutChange := feeRate.FeeForWeight(weight)

	// If the user selected an amount to withdraw, check if a change output
	// is needed.
	if selectedAmount > 0 {
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
		// selected deposit amount minus the selected amount.
		change := totalDepositAmount - selectedAmount

		switch {
		case change-feeWithChange >= dustLimit:
			// If the change can cover the fees without turning into
			// dust, add a non-dust change output.
			changeAmount = change - feeWithChange
			withdrawalFundingAmt = selectedAmount

		case change-feeWithoutChange >= 0:
			// If the change is dust, we give it to the miners.
			withdrawalFundingAmt = selectedAmount

		default:
			// If the fees eat into our selected amount, we fail the
			// withdrawal.
			return 0, 0, fmt.Errorf("the change doesn't " +
				"cover for fees. Consider lowering the fee " +
				"rate or decrease the selected amount")
		}
	} else {
		// If the user wants to withdraw the total value of deposits, we
		// don't need a change output.
		withdrawalFundingAmt = totalDepositAmount - feeWithoutChange
	}

	if withdrawalFundingAmt < dustLimit {
		return 0, 0, fmt.Errorf("withdrawal amount is below dust limit")
	}

	if changeAmount < 0 {
		return 0, 0, fmt.Errorf("change amount is negative")
	}

	// In case of a channel open, ensure that the channel funding amount is
	// at least in the amount of lnd's minimum channel size.
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
	for range numInputs {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	if commitmentType != lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
		switch commitmentType {
		case lnrpc.CommitmentType_SIMPLE_TAPROOT,
			lnrpc.CommitmentType_TAPROOT:
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
