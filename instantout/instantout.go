package instantout

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// InstantOut holds the necessary information to execute an instant out swap.
type InstantOut struct {
	// SwapHash is the hash of the swap.
	SwapHash lntypes.Hash

	// swapPreimage is the preimage that is used for the swap.
	swapPreimage lntypes.Preimage

	// State is the current state of the swap.
	State fsm.StateType

	// cltvExpiry is the expiry of the swap.
	cltvExpiry int32

	// outgoingChanSet optionally specifies the short channel ids of the
	// channels that may be used to loop out.
	outgoingChanSet loopdb.ChannelSet

	// reservations are the reservations that are used in as inputs for the
	// instant out swap.
	reservations []*reservation.Reservation

	// protocolVersion is the version of the protocol that is used for the
	// swap.
	protocolVersion ProtocolVersion

	// initiationHeight is the height at which the swap was initiated.
	initiationHeight int32

	// value is the amount that is swapped.
	value btcutil.Amount

	// keyLocator is the key locator that is used for the swap.
	keyLocator keychain.KeyLocator

	// clientPubkey is the pubkey of the client that is used for the swap.
	clientPubkey *btcec.PublicKey

	// serverPubkey is the pubkey of the server that is used for the swap.
	serverPubkey *btcec.PublicKey

	// swapInvoice is the invoice that is used for the swap.
	swapInvoice string

	// htlcFeeRate is the fee rate that is used for the htlc transaction.
	htlcFeeRate chainfee.SatPerKWeight

	// sweepAddress is the address that is used to sweep the funds to.
	sweepAddress btcutil.Address

	// finalizedHtlcTx is the finalized htlc transaction that is used in the
	// non-cooperative path for the instant out swap.
	finalizedHtlcTx *wire.MsgTx

	// SweepTxHash is the hash of the sweep transaction.
	SweepTxHash *chainhash.Hash

	// finalizedSweeplessSweepTx is the transaction that is used to sweep
	// the funds in the cooperative path.
	finalizedSweeplessSweepTx *wire.MsgTx

	// sweepConfirmationHeight is the height at which the sweep
	// transaction was confirmed.
	sweepConfirmationHeight uint32
}

// getHtlc returns the swap.htlc for the instant out.
func (i *InstantOut) getHtlc(chainParams *chaincfg.Params) (*swap.Htlc, error) {
	return swap.NewHtlcV2(
		i.cltvExpiry, pubkeyTo33ByteSlice(i.serverPubkey),
		pubkeyTo33ByteSlice(i.clientPubkey), i.SwapHash, chainParams,
	)
}

// createMusig2Session creates a musig2 session for the instant out.
func (i *InstantOut) createMusig2Session(ctx context.Context,
	signer lndclient.SignerClient) ([]*input.MuSig2SessionInfo,
	[][]byte, error) {

	// Create the htlc musig2 context.
	musig2Sessions := make([]*input.MuSig2SessionInfo, len(i.reservations))
	clientNonces := make([][]byte, len(i.reservations))

	// Create the sessions and nonces from the reservations.
	for idx, reservation := range i.reservations {
		session, err := reservation.Musig2CreateSession(ctx, signer)
		if err != nil {
			return nil, nil, err
		}

		musig2Sessions[idx] = session
		clientNonces[idx] = session.PublicNonce[:]
	}

	return musig2Sessions, clientNonces, nil
}

// getInputReservation returns the input reservation for the instant out.
func (i *InstantOut) getInputReservations() (InputReservations, error) {
	if len(i.reservations) == 0 {
		return nil, errors.New("no reservations")
	}

	inputs := make(InputReservations, len(i.reservations))
	for idx, reservation := range i.reservations {
		pkScript, err := reservation.GetPkScript()
		if err != nil {
			return nil, err
		}

		inputs[idx] = InputReservation{
			Outpoint: *reservation.Outpoint,
			Value:    reservation.Value,
			PkScript: pkScript,
		}
	}

	return inputs, nil
}

// createHtlcTransaction creates the htlc transaction for the instant out.
func (i *InstantOut) createHtlcTransaction(network *chaincfg.Params) (
	*wire.MsgTx, error) {

	if network == nil {
		return nil, errors.New("no network provided")
	}

	inputReservations, err := i.getInputReservations()
	if err != nil {
		return nil, err
	}

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// add the reservation inputs
	for _, reservation := range inputReservations {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: reservation.Outpoint,
		})
	}

	// Estimate the fee
	weight := htlcWeight(len(inputReservations))
	fee := i.htlcFeeRate.FeeForWeight(weight)
	if fee > i.value/5 {
		return nil, errors.New("fee is higher than 20% of " +
			"sweep value")
	}

	htlc, err := i.getHtlc(network)
	if err != nil {
		return nil, err
	}

	// Create the sweep output
	sweepOutput := &wire.TxOut{
		Value:    int64(i.value) - int64(fee),
		PkScript: htlc.PkScript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

// createSweeplessSweepTx creates the sweepless sweep transaction for the
// instant out.
func (i *InstantOut) createSweeplessSweepTx(feerate chainfee.SatPerKWeight) (
	*wire.MsgTx, error) {

	inputReservations, err := i.getInputReservations()
	if err != nil {
		return nil, err
	}

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// add the reservation inputs
	for _, reservation := range inputReservations {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: reservation.Outpoint,
		})
	}

	// Estimate the fee
	weight := sweeplessSweepWeight(len(inputReservations))
	fee := feerate.FeeForWeight(weight)
	if fee > i.value/5 {
		return nil, errors.New("fee is higher than 20% of " +
			"sweep value")
	}

	pkscript, err := txscript.PayToAddrScript(i.sweepAddress)
	if err != nil {
		return nil, err
	}

	// Create the sweep output
	sweepOutput := &wire.TxOut{
		Value:    int64(i.value) - int64(fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

// signMusig2Tx adds the server nonces to the musig2 sessions and signs the
// transaction.
func (i *InstantOut) signMusig2Tx(ctx context.Context,
	signer lndclient.SignerClient, tx *wire.MsgTx,
	musig2sessions []*input.MuSig2SessionInfo,
	counterPartyNonces [][66]byte) ([][]byte, error) {

	inputs, err := i.getInputReservations()
	if err != nil {
		return nil, err
	}

	prevOutFetcher := inputs.GetPrevoutFetcher()
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	sigs := make([][]byte, len(inputs))

	for idx, reservation := range inputs {
		if !reflect.DeepEqual(tx.TxIn[idx].PreviousOutPoint,
			reservation.Outpoint) {

			return nil, fmt.Errorf("tx input does not match " +
				"reservation")
		}

		taprootSigHash, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault,
			tx, idx, prevOutFetcher,
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

// finalizeMusig2Transaction creates the finalized transactions for either
// the htlc or the cooperative close.
func (i *InstantOut) finalizeMusig2Transaction(ctx context.Context,
	signer lndclient.SignerClient,
	musig2Sessions []*input.MuSig2SessionInfo,
	tx *wire.MsgTx, serverSigs [][]byte) (*wire.MsgTx, error) {

	inputs, err := i.getInputReservations()
	if err != nil {
		return nil, err
	}

	for idx := range inputs {
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

// generateHtlcSweepTx creates the htlc sweep transaction for the instant out.
func (i *InstantOut) generateHtlcSweepTx(ctx context.Context,
	signer lndclient.SignerClient, feeRate chainfee.SatPerKWeight,
	network *chaincfg.Params, blockheight uint32) (
	*wire.MsgTx, error) {

	if network == nil {
		return nil, errors.New("no network provided")
	}

	if i.finalizedHtlcTx == nil {
		return nil, errors.New("no finalized htlc tx")
	}

	htlc, err := i.getHtlc(network)
	if err != nil {
		return nil, err
	}

	// Create the sweep transaction.
	sweepTx := wire.NewMsgTx(2)
	sweepTx.LockTime = blockheight

	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddP2TROutput()

	err = htlc.AddSuccessToEstimator(&weightEstimator)
	if err != nil {
		return nil, err
	}

	htlcHash := i.finalizedHtlcTx.TxHash()

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
	sweepPkScript, err := txscript.PayToAddrScript(i.sweepAddress)
	if err != nil {
		return nil, err
	}

	fee := feeRate.FeeForWeight(int64(weightEstimator.Weight()))

	htlcOutValue := i.finalizedHtlcTx.TxOut[0].Value
	output := &wire.TxOut{
		Value:    htlcOutValue - int64(fee),
		PkScript: sweepPkScript,
	}

	sweepTx.AddTxOut(output)

	signDesc := lndclient.SignDescriptor{
		WitnessScript: htlc.SuccessScript(),
		Output: &wire.TxOut{
			Value:    htlcOutValue,
			PkScript: htlc.PkScript,
		},
		HashType:   htlc.SigHash(),
		InputIndex: 0,
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: i.keyLocator,
		},
	}

	rawSigs, err := signer.SignOutputRaw(
		ctx, sweepTx, []*lndclient.SignDescriptor{&signDesc}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("sign output error: %v", err)
	}
	sig := rawSigs[0]

	// Add witness stack to the tx input.
	sweepTx.TxIn[0].Witness, err = htlc.GenSuccessWitness(
		sig, i.swapPreimage,
	)
	if err != nil {
		return nil, err
	}

	return sweepTx, nil
}

// htlcWeight returns the weight for the htlc transaction.
func htlcWeight(numInputs int) int64 {
	var weightEstimator input.TxWeightEstimator
	for i := 0; i < numInputs; i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	weightEstimator.AddP2WSHOutput()

	return int64(weightEstimator.Weight())
}

// sweeplessSweepWeight returns the weight for the sweepless sweep transaction.
func sweeplessSweepWeight(numInputs int) int64 {
	var weightEstimator input.TxWeightEstimator
	for i := 0; i < numInputs; i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	weightEstimator.AddP2TROutput()

	return int64(weightEstimator.Weight())
}

// pubkeyTo33ByteSlice converts a pubkey to a 33 byte slice.
func pubkeyTo33ByteSlice(pubkey *btcec.PublicKey) [33]byte {
	var pubkeyBytes [33]byte
	copy(pubkeyBytes[:], pubkey.SerializeCompressed())

	return pubkeyBytes
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
