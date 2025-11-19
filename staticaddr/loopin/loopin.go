package loopin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/zpay32"
)

// StaticAddressLoopIn represents the in-memory loop-in information.
type StaticAddressLoopIn struct {
	// SwapHash is the hashed preimage of the swap invoice. It represents
	// the primary identifier of the swap.
	SwapHash lntypes.Hash

	// SwapPreimage is the preimage that is used for the swap.
	SwapPreimage lntypes.Preimage

	// HtlcCltvExpiry is the expiry of the swap.
	HtlcCltvExpiry int32

	// MaxSwapFee is the swap fee in sats that the user accepted when
	// initiating the swap. It is the upper limit for the QuotedSwapFee.
	MaxSwapFee btcutil.Amount

	// InitiationHeight is the height at which the swap was initiated.
	InitiationHeight uint32

	// InitiationTime is the time at which the swap was initiated.
	InitiationTime time.Time

	// ProtocolVersion is the protocol version of the static address.
	ProtocolVersion version.AddressProtocolVersion

	// Label contains an optional label for the swap.
	Label string

	// Htlc key fields.

	// ClientPubkey is the pubkey of the client that is used for the swap.
	ClientPubkey *btcec.PublicKey

	// ServerPubkey is the pubkey of the server that is used for the swap.
	ServerPubkey *btcec.PublicKey

	// HtlcKeyLocator is the locator of the server's htlc key.
	HtlcKeyLocator keychain.KeyLocator

	// Static address loop-in fields.

	// SwapInvoice is the invoice that needs to be paid by the server to
	// complete the loop-in swap.
	SwapInvoice string

	// LastHop is an optional parameter that specifies the last hop to be
	// used for a loop in swap.
	LastHop []byte

	// The swap payment timeout allows the user to specify an upper limit
	// for the amount of time the server is allowed to take to fulfill the
	// off-chain swap payment. If the timeout is reached the swap will be
	// aborted on the server side and the client can retry the swap with
	// different parameters.
	PaymentTimeoutSeconds uint32

	// QuotedSwapFee is the swap fee in sats that the server returned in the
	// swap quote.
	QuotedSwapFee btcutil.Amount

	// The outpoints in the format txid:vout that are part of the loop-in
	// swap.
	// TODO(hieblmi): Replace this with a getter method that fetches the
	//      outpoints from the deposits.
	DepositOutpoints []string

	// SelectedAmount is the amount that the user selected for the swap. If
	// the user did not select an amount, the amount of all deposits is
	// used.
	SelectedAmount btcutil.Amount

	// Fast indicates whether the client requested fast publication behavior
	// on the server side for this static loop in.
	Fast bool

	// state is the current state of the swap.
	state fsm.StateType

	// Non-persistent convenience fields.

	// Initiator is an optional identification string that will be appended
	// to the user agent string sent to the server to give information about
	// the usage of loop. This initiator part is meant for user interfaces
	// to add their name to give the full picture of the binary used
	// (loopd, lit) and the method used for triggering the swap
	// (loop cli, autolooper, lit ui, other 3rd party ui).
	Initiator string

	// Private indicates whether the destination node should be considered
	// private. In which case, loop will generate hop hints to assist with
	// probing and payment.
	Private bool

	// Optional route hints to reach the destination through private
	// channels.
	RouteHints [][]zpay32.HopHint

	// Deposits are the deposits that are part of the loop-in swap. They
	// implicitly carry the swap amount.
	Deposits []*deposit.Deposit

	// AddressParams are the parameters of the address that is used for the
	// swap.
	AddressParams *address.Parameters

	// Address is the address script that is used for the swap.
	Address *script.StaticAddress

	// HTLC fields.

	// HtlcTxFeeRate is the fee rate that is used for the htlc transaction.
	HtlcTxFeeRate chainfee.SatPerKWeight

	// HtlcTxHighFeeRate is the fee rate that is used for the htlc
	// transaction.
	HtlcTxHighFeeRate chainfee.SatPerKWeight

	// HtlcTxExtremelyHighFeeRate is the fee rate that is used for the htlc
	// transaction.
	HtlcTxExtremelyHighFeeRate chainfee.SatPerKWeight

	// HtlcTimeoutSweepTxHash is the hash of the htlc timeout sweep tx.
	HtlcTimeoutSweepTxHash *chainhash.Hash

	// HtlcTimeoutSweepAddress
	HtlcTimeoutSweepAddress btcutil.Address

	mu sync.Mutex
}

func (l *StaticAddressLoopIn) getHtlc(chainParams *chaincfg.Params) (*swap.Htlc,
	error) {

	return swap.NewHtlcV2(
		l.HtlcCltvExpiry, pubkeyTo33ByteSlice(l.ClientPubkey),
		pubkeyTo33ByteSlice(l.ServerPubkey), l.SwapHash, chainParams,
	)
}

// createMusig2Sessions creates a musig2 session for a number of deposits.
func (l *StaticAddressLoopIn) createMusig2Sessions(ctx context.Context,
	signer lndclient.SignerClient) ([]*input.MuSig2SessionInfo, [][]byte,
	error) {

	musig2Sessions := make([]*input.MuSig2SessionInfo, len(l.Deposits))
	clientNonces := make([][]byte, len(l.Deposits))

	// Create the sessions and nonces from the deposits.
	for i := 0; i < len(l.Deposits); i++ {
		session, err := l.createMusig2Session(ctx, signer)
		if err != nil {
			return nil, nil, err
		}

		musig2Sessions[i] = session
		clientNonces[i] = session.PublicNonce[:]
	}

	return musig2Sessions, clientNonces, nil
}

// Musig2CreateSession creates a musig2 session for the deposit.
func (l *StaticAddressLoopIn) createMusig2Session(ctx context.Context,
	signer lndclient.SignerClient) (*input.MuSig2SessionInfo, error) {

	signers := [][]byte{
		l.AddressParams.ClientPubkey.SerializeCompressed(),
		l.AddressParams.ServerPubkey.SerializeCompressed(),
	}

	expiryLeaf := l.Address.TimeoutLeaf

	rootHash := expiryLeaf.TapHash()

	return signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &l.AddressParams.KeyLocator,
		signers, lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
	)
}

// signMusig2Tx adds the server nonces to the musig2 sessions and signs the
// transaction.
func (l *StaticAddressLoopIn) signMusig2Tx(ctx context.Context,
	tx *wire.MsgTx, signer lndclient.SignerClient,
	musig2sessions []*input.MuSig2SessionInfo,
	counterPartyNonces [][musig2.PubNonceSize]byte) ([][]byte, error) {

	prevOuts, err := staticutil.ToPrevOuts(
		l.Deposits, l.AddressParams.PkScript,
	)
	if err != nil {
		return nil, err
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	outpoints := l.Outpoints()
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

// createHtlcTx creates the transaction that spend the deposit outpoints into
// a htlc outpoint.
func (l *StaticAddressLoopIn) createHtlcTx(chainParams *chaincfg.Params,
	feeRate chainfee.SatPerKWeight, maxFeePercentage float64) (*wire.MsgTx,
	error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	outpoints := l.Outpoints()
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	// Determine the swap amount. If the user selected a specific amount, we
	// use that and use the difference to the total deposit amount as the
	// change.
	var (
		swapAmt      = l.TotalDepositAmount()
		changeAmount btcutil.Amount
	)
	if l.SelectedAmount > 0 {
		swapAmt = l.SelectedAmount
		changeAmount = l.TotalDepositAmount() - l.SelectedAmount
	}

	// Calculate htlc tx fee for server provided fee rate.
	hasChange := changeAmount > 0
	weight := l.htlcWeight(hasChange)
	fee := feeRate.FeeForWeight(weight)

	// Check if the server breaches our fee limits.
	feeLimit := btcutil.Amount(float64(swapAmt) * maxFeePercentage)
	if fee > feeLimit {
		return nil, fmt.Errorf("htlc tx fee %v exceeds max fee %v",
			fee, feeLimit)
	}

	htlc, err := l.getHtlc(chainParams)
	if err != nil {
		return nil, err
	}

	pkscript, err := txscript.PayToAddrScript(htlc.Address)
	if err != nil {
		return nil, err
	}

	// Create the sweep output
	sweepOutput := &wire.TxOut{
		Value:    int64(swapAmt - fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	// We expect change to be sent back to our static address output script.
	if changeAmount > 0 {
		msgTx.AddTxOut(&wire.TxOut{
			Value:    int64(changeAmount),
			PkScript: l.AddressParams.PkScript,
		})
	}

	return msgTx, nil
}

// isHtlcTimedOut returns true if the htlc's cltv expiry is reached. In this
// case the client is able to broadcast a timeout refund transaction that can be
// included in blocks greater than height.
func (l *StaticAddressLoopIn) isHtlcTimedOut(height int32) bool {
	return height >= l.HtlcCltvExpiry
}

// htlcWeight returns the weight for the htlc transaction.
func (l *StaticAddressLoopIn) htlcWeight(hasChange bool) lntypes.WeightUnit {
	var weightEstimator input.TxWeightEstimator
	for i := 0; i < len(l.Deposits); i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	weightEstimator.AddP2WSHOutput()

	if hasChange {
		weightEstimator.AddP2TROutput()
	}

	return weightEstimator.Weight()
}

// createHtlcSweepTx creates the htlc sweep transaction for the timeout path of
// the loop-in htlc.
func (l *StaticAddressLoopIn) createHtlcSweepTx(ctx context.Context,
	signer lndclient.SignerClient, sweepAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight, network *chaincfg.Params,
	blockHeight uint32, maxFeePercentage float64) (*wire.MsgTx, error) {

	if network == nil {
		return nil, errors.New("no network provided")
	}

	htlc, err := l.getHtlc(network)
	if err != nil {
		return nil, err
	}

	// Create the sweep transaction.
	sweepTx := wire.NewMsgTx(2)
	sweepTx.LockTime = blockHeight

	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddP2TROutput()

	err = htlc.AddSuccessToEstimator(&weightEstimator)
	if err != nil {
		return nil, err
	}

	htlcTx, err := l.createHtlcTx(
		network, l.HtlcTxFeeRate, maxFeePercentage,
	)
	if err != nil {
		return nil, err
	}

	// Check if the htlc tx has a change output. If so we need to select the
	// non-change output index to construct the sweep with.
	htlcInputIndex := uint32(0)
	if len(htlcTx.TxOut) == 2 {
		// If the first htlc tx output matches our static address
		// script we need to select the second output to sweep from.
		if bytes.Equal(
			htlcTx.TxOut[0].PkScript, l.AddressParams.PkScript,
		) {

			htlcInputIndex = 1
		}
	}

	// Add the htlc input.
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  htlcTx.TxHash(),
			Index: htlcInputIndex,
		},
		SignatureScript: htlc.SigScript,
		Sequence:        htlc.SuccessSequence(),
	})

	// Add the sweep output.
	sweepPkScript, err := txscript.PayToAddrScript(sweepAddress)
	if err != nil {
		return nil, err
	}

	fee := feeRate.FeeForWeight(weightEstimator.Weight())

	htlcOutValue := htlcTx.TxOut[0].Value
	output := &wire.TxOut{
		Value:    htlcOutValue - int64(fee),
		PkScript: sweepPkScript,
	}

	sweepTx.AddTxOut(output)

	signDesc := lndclient.SignDescriptor{
		WitnessScript: htlc.TimeoutScript(),
		Output: &wire.TxOut{
			Value:    htlcOutValue,
			PkScript: htlc.PkScript,
		},
		HashType:   htlc.SigHash(),
		InputIndex: 0,
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: l.HtlcKeyLocator,
		},
	}
	signDesc.SignMethod = input.WitnessV0SignMethod

	rawSigs, err := signer.SignOutputRaw(
		ctx, sweepTx, []*lndclient.SignDescriptor{&signDesc}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("sign output error: %w", err)
	}
	sig := rawSigs[0]

	// Add witness stack to the tx input.
	sweepTx.TxIn[0].Witness, err = htlc.GenTimeoutWitness(sig)
	if err != nil {
		return nil, err
	}

	return sweepTx, nil
}

// pubkeyTo33ByteSlice converts a pubkey to a 33 byte slice.
func pubkeyTo33ByteSlice(pubkey *btcec.PublicKey) [33]byte {
	var pubkeyBytes [33]byte
	copy(pubkeyBytes[:], pubkey.SerializeCompressed())

	return pubkeyBytes
}

// TotalDepositAmount returns the total amount of the deposits.
func (l *StaticAddressLoopIn) TotalDepositAmount() btcutil.Amount {
	var total btcutil.Amount
	if len(l.Deposits) == 0 {
		return total
	}

	for _, d := range l.Deposits {
		total += d.Value
	}
	return total
}

// RemainingPaymentTimeSeconds returns the remaining time in seconds until the
// payment timeout is reached. The remaining time is calculated from the
// initiation time of the swap. If more than the swaps configured payment
// timeout has passed, the remaining time will be negative.
func (l *StaticAddressLoopIn) RemainingPaymentTimeSeconds() int64 {
	elapsedSinceInitiation := time.Since(l.InitiationTime).Seconds()

	return int64(l.PaymentTimeoutSeconds) - int64(elapsedSinceInitiation)
}

// Outpoints returns the wire outpoints of the deposits.
func (l *StaticAddressLoopIn) Outpoints() []wire.OutPoint {
	outpoints := make([]wire.OutPoint, len(l.Deposits))
	for i, d := range l.Deposits {
		outpoints[i] = wire.OutPoint{
			Hash:  d.Hash,
			Index: d.Index,
		}
	}

	return outpoints
}

// GetState returns the current state of the loop-in swap.
func (l *StaticAddressLoopIn) GetState() fsm.StateType {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.state
}

// SetState sets the current state of the loop-in swap.
func (l *StaticAddressLoopIn) SetState(state fsm.StateType) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.state = state
}

// IsInState returns true if the deposit is in the given state.
func (l *StaticAddressLoopIn) IsInState(state fsm.StateType) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.state == state
}
