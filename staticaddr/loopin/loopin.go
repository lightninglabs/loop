package loopin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
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

	// ProtocolVersion is the protocol version of the static address.
	ProtocolVersion version.AddressProtocolVersion

	// Label contains an optional label for the swap.
	Label string

	// Htlc key fields

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

	// QuotedSwapFee is the swap fee in sats that the server returned in the
	// swap quote.
	QuotedSwapFee btcutil.Amount

	DepositOutpoints []string

	// State is the current state of the swap.
	state fsm.StateType

	// Non-persistent convenience fields

	Initiator string

	Private bool

	RouteHints [][]zpay32.HopHint

	Deposits []*deposit.Deposit

	AddressParams *address.Parameters

	Address *script.StaticAddress

	// SweeplessSweepAddress is the address that is used to sweep the
	// deposits as part of the loop in swap.
	SweeplessSweepAddress btcutil.Address

	// SweeplessFeeRate is the fee rate that is used for the htlc
	// transaction.
	SweeplessSweepFeeRate chainfee.SatPerKWeight

	// HTLC Fields

	// HtlcTx is the htlc transaction that is used in the non-cooperative
	// path for the static address loop-in swap. It is not finalized as it
	// lacks the server signatures.
	HtlcTx *wire.MsgTx

	// HtlcTxFeeRate is the fee rate that is used for the htlc transaction.
	HtlcTxFeeRate chainfee.SatPerKWeight

	// HtlcTimeoutSweepTx
	HtlcTimeoutSweepTx *wire.MsgTx

	// HtlcTimeoutSweepAddress
	HtlcTimeoutSweepAddress btcutil.Address

	sync.Mutex
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

// createSweeplessSweepTx creates the sweepless sweep transaction that the
// server wishes to publish. It spends the deposit outpoints to a
// server-specified sweep address.
func (l *StaticAddressLoopIn) createSweeplessSweepTx() (*wire.MsgTx, error) {
	// Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	outpoints := l.Outpoints()
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	// Calculate tx fee for the server provided fee rate.
	weight, err := sweeplessSweepWeight(
		len(outpoints), l.SweeplessSweepAddress,
	)
	if err != nil {
		return nil, err
	}
	fee := l.SweeplessSweepFeeRate.FeeForWeight(weight)

	pkscript, err := txscript.PayToAddrScript(l.SweeplessSweepAddress)
	if err != nil {
		return nil, err
	}

	// Create the sweep output.
	sweepOutput := &wire.TxOut{
		Value:    int64(l.TotalDepositAmount()) - int64(fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

// sweeplessSweepWeight returns weight units for a sweepless sweep transaction
// with N taproot inputs and one sweep output.
func sweeplessSweepWeight(numInputs int,
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

// signMusig2Tx adds the server nonces to the musig2 sessions and signs the
// transaction.
func (l *StaticAddressLoopIn) signMusig2Tx(ctx context.Context,
	tx *wire.MsgTx, signer lndclient.SignerClient,
	musig2sessions []*input.MuSig2SessionInfo,
	counterPartyNonces [][musig2.PubNonceSize]byte) ([][]byte, error) {

	prevOuts := l.toPrevOuts(l.Deposits, l.AddressParams.PkScript)
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
func (l *StaticAddressLoopIn) createHtlcTx(chainParams *chaincfg.Params) (
	*wire.MsgTx, error) {

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

	// Calculate htlc tx fee for server provided fee rate.
	weight := htlcWeight(len(outpoints))
	fee := l.HtlcTxFeeRate.FeeForWeight(weight)

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
		Value:    int64(l.TotalDepositAmount()) - int64(fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

// isHtlcTimedOut returns true if the htlc's cltv expiry is reached.
func (l *StaticAddressLoopIn) isHtlcTimedOut(height int32) bool {
	return height >= l.HtlcCltvExpiry
}

// htlcWeight returns the weight for the htlc transaction.
func htlcWeight(numInputs int) lntypes.WeightUnit {
	var weightEstimator input.TxWeightEstimator
	for i := 0; i < numInputs; i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	weightEstimator.AddP2WSHOutput()

	return weightEstimator.Weight()
}

// createHtlcSweepTx creates the htlc sweep transaction for the timout path of
// the loop-in htlc.
func (l *StaticAddressLoopIn) createHtlcSweepTx(ctx context.Context,
	signer lndclient.SignerClient, sweepAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight, network *chaincfg.Params,
	blockHeight uint32) (*wire.MsgTx, error) {

	if network == nil {
		return nil, errors.New("no network provided")
	}

	if l.HtlcTx == nil {
		return nil, errors.New("no finalized htlc tx")
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

	htlcHash := l.HtlcTx.TxHash()

	// Add the htlc input.
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  htlcHash,
			Index: 0,
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

	htlcOutValue := l.HtlcTx.TxOut[0].Value
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
		return nil, fmt.Errorf("sign output error: %v", err)
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

func (l *StaticAddressLoopIn) toPrevOuts(deposits []*deposit.Deposit,
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

// GetState returns the current state of the loop-in swap.
func (l *StaticAddressLoopIn) GetState() fsm.StateType {
	l.Lock()
	defer l.Unlock()

	return l.state
}

// SetState sets the current state of the loop-in swap.
func (l *StaticAddressLoopIn) SetState(state fsm.StateType) {
	l.Lock()
	defer l.Unlock()

	l.state = state
}

// IsInState returns true if the deposit is in the given state.
func (l *StaticAddressLoopIn) IsInState(state fsm.StateType) bool {
	l.Lock()
	defer l.Unlock()

	return l.state == state
}

// IsInFinalState returns true if the deposit is final.
func (l *StaticAddressLoopIn) IsInFinalState() bool {
	l.Lock()
	defer l.Unlock()

	return l.state == HtlcTimeoutSwept || l.state == Succeeded ||
		l.state == SucceededSweeplessSigFailed || l.state == Failed
}
