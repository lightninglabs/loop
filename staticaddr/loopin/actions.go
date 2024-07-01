package loopin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"github.com/lightninglabs/loop/labels"
	"reflect"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	defaultConfTarget = 3
)

// RequestContext is the context passed to the instant out FSM when
// it is initialized.
type RequestContext struct {
	depositOutpoints []string
	maxSwapFee       btcutil.Amount
	maxMinerFee      btcutil.Amount
	lastHop          *route.Vertex
	label            string
	userAgent        string
	private          bool
	routeHints       [][]zpay32.HopHint
}

// ValidateRequestAction ...
func (f *FSM) ValidateRequestAction(eventCtx fsm.EventContext) fsm.EventType {
	req, ok := eventCtx.(*RequestContext)
	if !ok {
		return f.HandleError(fmt.Errorf("unexpected context type"))
	}

	if len(req.depositOutpoints) == 0 {
		return f.HandleError(fmt.Errorf("no deposits specified"))
	}
	f.loopIn.DepositOutpoints = req.depositOutpoints

	// Retrieve all deposits referenced by the outpoints and ensure that
	// they are in state Deposited.
	deposits, active := f.cfg.DepositManager.AllStringOutpointsActiveDeposits( //nolint:lll
		f.loopIn.DepositOutpoints, deposit.Deposited,
	)
	if !active {
		return f.HandleError(fmt.Errorf("not all deposits are active"))
	}

	f.loopIn.Deposits = deposits

	f.loopIn.Value = deposit.TotalDepositAmount(f.loopIn.Deposits)

	// Check that the label is valid.
	if err := labels.Validate(req.label); err != nil {
		return f.HandleError(err)
	}

	// Private and route hints are mutually exclusive as setting private
	// means we retrieve our own route hints from the connected node.
	if len(req.routeHints) != 0 && req.private {
		return f.HandleError(fmt.Errorf("route hints and private are " +
			"mutually exclusive"))
	}

	// If private is set, we generate route hints.
	var err error
	if req.private {
		// If last_hop is set, we'll only add channels with peers set to
		// the last_hop parameter.
		includeNodes := make(map[route.Vertex]struct{})
		if req.lastHop != nil {
			includeNodes[*req.lastHop] = struct{}{}
		}

		// Because the Private flag is set, we'll generate our own set
		// of hop hints.
		req.routeHints, err = loop.SelectHopHints(
			f.ctx, f.cfg.LndClient, f.loopIn.Value,
			loop.DefaultMaxHopHints, includeNodes,
		)
		if err != nil {
			return f.HandleError(err)
		}
	}

	// Request current server loop in terms and use these to calculate the
	// swap fee that we should subtract from the swap amount in the payment
	// request that we send to the server. We pass nil as optional route
	// hints as hop hint selection when generating invoices with private
	// channels is an LND side black box feature. Advanced users will quote
	// directly anyway and there they have the option to add specific route
	// hints.
	// The quote call will also request a probe from the server to ensure
	// feasibility of a loop-in for the totalDepositAmount.
	quote, err := f.cfg.SwapClient.Server.GetLoopInQuote(
		f.ctx, f.loopIn.Value, f.cfg.NodePubkey, req.lastHop,
		req.routeHints, req.userAgent, uint32(len(deposits)),
	)
	if err != nil {
		return f.HandleError(err)
	}

	if quote.SwapFee > req.maxSwapFee {
		log.Warnf("Swap fee %v exceeding maximum of %v",
			quote.SwapFee, req.maxSwapFee)

		return f.HandleError(loop.ErrSwapFeeTooHigh)
	}

	// Calculate the swap invoice amount. The pre-pay is added which
	// effectively forces the server to pay us back our prepayment on a
	// successful swap.
	swapInvoiceAmt := f.loopIn.Value - quote.SwapFee

	// Generate random preimage.
	var swapPreimage lntypes.Preimage
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		log.Error("Cannot generate preimage")
	}
	f.loopIn.SwapPreimage = swapPreimage
	f.loopIn.SwapHash = sha256.Sum256(swapPreimage[:])

	// Derive a client key for the HTLC.
	keyDesc, err := f.cfg.WalletKit.DeriveNextKey(
		f.ctx, swap.StaticAddressKeyFamily,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.ClientPubkey = keyDesc.PubKey
	f.loopIn.HtlcKeyLocator = keyDesc.KeyLocator

	var clientKey [33]byte
	copy(clientKey[:], keyDesc.PubKey.SerializeCompressed())
	fmt.Printf("client htlc key: %x\n", clientKey)
	fmt.Printf("swap hash %x\n", f.loopIn.SwapHash[:])
	fmt.Printf("preimage %x\n", f.loopIn.SwapPreimage[:])

	// Create the swap invoice in lnd.
	_, swapInvoice, err := f.cfg.LndClient.AddInvoice(
		f.ctx, &invoicesrpc.AddInvoiceData{
			Preimage:   &swapPreimage,
			Value:      lnwire.NewMSatFromSatoshis(swapInvoiceAmt),
			Memo:       "static address loop-in",
			Expiry:     3600 * 24 * 365,
			RouteHints: req.routeHints,
		},
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.SwapInvoice = swapInvoice

	f.loopIn.StaticAddressProtocolVersion = version.AddressProtocolVersion(
		version.CurrentRPCProtocolVersion(),
	)

	loopInReq := &swapserverrpc.ServerStaticAddressLoopInRequest{
		SwapHash:         f.loopIn.SwapHash[:],
		DepositOutpoints: f.loopIn.DepositOutpoints,
		HtlcClientKey:    f.loopIn.ClientPubkey.SerializeCompressed(),
		SwapInvoice:      f.loopIn.SwapInvoice,
		ProtocolVersion:  version.CurrentRPCProtocolVersion(),
		UserAgent:        req.userAgent,
	}
	if req.lastHop != nil {
		loopInReq.LastHop = req.lastHop[:]
		f.loopIn.LastHop = req.lastHop[:]
	}

	loopInResp, err := f.cfg.StaticAddressServerClient.ServerStaticAddressLoopIn( //nolint:lll
		f.ctx, loopInReq,
	)
	if err != nil {
		return f.HandleError(err)
	}

	var id ID
	err = id.FromByteSlice(loopInResp.LoopInId)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.ID = id

	serverPubkey, err := btcec.ParsePubKey(loopInResp.HtlcServerKey)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.ServerPubkey = serverPubkey
	fmt.Printf("server htlc key %x\n", f.loopIn.ServerPubkey.SerializeCompressed())

	f.loopIn.HtlcExpiry = loopInResp.HtlcExpiry

	f.htlcServerNonces, err = toNonces(loopInResp.HtlcServerNonces)

	f.loopIn.HtlcFeeRate = chainfee.SatPerKWeight(loopInResp.HtlcFeeRate)

	// Lock the deposits and transition them to the LoopingIn state.
	err = f.cfg.DepositManager.TransitionDeposits(
		deposits, deposit.OnLoopinInitiated, deposit.LoopingIn,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnValidRequest
}

// SignHtlcTxAction ...
func (f *FSM) SignHtlcTxAction(_ fsm.EventContext) fsm.EventType {
	// Create htlc and get pkScript to sign the deposit <-> htlc tx, then
	// send sig to server.
	htlc, err := swap.NewHtlcV2(
		f.loopIn.HtlcExpiry, pubkeyTo33ByteSlice(f.loopIn.ClientPubkey),
		pubkeyTo33ByteSlice(f.loopIn.ServerPubkey), f.loopIn.SwapHash,
		f.cfg.ChainParams,
	)
	if err != nil {
		return f.HandleError(err)
	}

	fmt.Printf("LoopIn htlc pkscript: %x\n", htlc.PkScript)

	// /////////////////////// HTLC MUSIG_2 STUFF
	f.loopIn.AddressParams, err = f.cfg.AddressManager.GetStaticAddressParameters(
		f.ctx,
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.loopIn.Address, err = f.cfg.AddressManager.GetStaticAddress(f.ctx)
	if err != nil {
		return f.HandleError(err)
	}

	// Create a musig2 session for each deposit.
	htlcSessions, clientHtlcNonces, err := createMusig2Sessions(
		f.ctx, f.loopIn.Deposits, f.loopIn.AddressParams,
		f.loopIn.Address, f.cfg.Signer,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.htlcMusig2Sessions = htlcSessions
	f.htlcClientNonces, err = toNonces(clientHtlcNonces)
	if err != nil {
		return f.HandleError(err)
	}

	prevOuts := toPrevOuts(
		f.loopIn.Deposits, f.loopIn.AddressParams.PkScript,
	)
	totalValue := totalValue(prevOuts)

	outpoints := toOutpoints(f.loopIn.Deposits)
	htlcTx, err := createHtlcTx(
		outpoints, totalValue, htlc.Address, f.loopIn.HtlcFeeRate,
	)
	if err != nil {
		return f.HandleError(err)
	}
	fmt.Printf("client nonces: %x\n", clientHtlcNonces)
	fmt.Printf("server nonces: %x\n", f.htlcServerNonces)

	var buffer bytes.Buffer
	htlcTx.Serialize(&buffer)

	fmt.Printf("htlc tx: %x\n", buffer)

	// Next we'll get our htlc tx signatures.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	htlcSigs, err := signMusig2Tx(
		f.ctx, prevOutFetcher, outpoints, f.cfg.Signer, htlcTx,
		htlcSessions, f.htlcServerNonces,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.htlcClientSigs = htlcSigs
	fmt.Printf("client htlc sigs: %x\n", htlcSigs)

	// Push Htlc sigs to server
	pushHtlcReq := &swapserverrpc.PushStaticAddressHtlcSigsRequest{
		LoopInId:         f.loopIn.ID[:],
		HtlcClientNonces: clientHtlcNonces,
		HtlcClientSigs:   htlcSigs,
	}
	sigPushResp, err := f.cfg.StaticAddressServerClient.PushStaticAddressHtlcSigs( //nolint:lll
		f.ctx, pushHtlcReq,
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.sweeplessServerNonces, err = toNonces(
		sigPushResp.SweeplessServerNonces,
	)
	sweeplessAddress, err := btcutil.DecodeAddress(
		sigPushResp.SweeplessSweepAddr, f.cfg.ChainParams,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.SweeplessSweepAddress = sweeplessAddress
	f.loopIn.SweeplessFeeRate = chainfee.SatPerKWeight(
		sigPushResp.SweeplessFeeRate,
	)

	return OnHtlcTxSigned
}

// MonitorInvoiceAction ...
func (f *FSM) MonitorInvoiceAction(_ fsm.EventContext) fsm.EventType {
	// Monitor the swap invoice.
	updateChan, errChan, err := f.cfg.InvoicesClient.SubscribeSingleInvoice(
		f.ctx, f.loopIn.SwapHash,
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case err := <-errChan:
			return f.HandleError(err)

		case update := <-updateChan:
			f.Debugf("received off-chain payment: %v update: %v",
				f.loopIn.SwapHash, update.State)

			switch update.State {
			case invoices.ContractOpen:
			case invoices.ContractAccepted:
			case invoices.ContractSettled:
				// Unlock the deposits and transition them to
				// the LoopedIn state.
				err = f.cfg.DepositManager.TransitionDeposits(
					f.loopIn.Deposits,
					deposit.OnLoopedIn,
					deposit.LoopedIn,
				)
				if err != nil {
					return f.HandleError(err)
				}
				return OnPaymentReceived

			case invoices.ContractCanceled:
				return f.HandleError(fmt.Errorf("invoice " +
					"canceled"))

			default:
				return f.HandleError(fmt.Errorf("unexpected "+
					"invoice state: %v", update.State))
			}

		case <-time.After(5 * time.Minute):
			return f.HandleError(fmt.Errorf("timeout waiting for " +
				"invoice to be accepted"))

		case <-f.ctx.Done():
			return f.HandleError(f.ctx.Err())
		}
	}

	return f.HandleError(fmt.Errorf("unexpected payment state"))
}

func (f *FSM) SignSweeplessSweepAction(_ fsm.EventContext) fsm.EventType {
	// Create a musig2 session for each deposit.
	musig2Sessions, clientNonces, err := createMusig2Sessions(
		f.ctx, f.loopIn.Deposits, f.loopIn.AddressParams,
		f.loopIn.Address, f.cfg.Signer,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.sweeplessMusig2Sessions = musig2Sessions
	f.sweeplessClientNonces, err = toNonces(clientNonces)
	if err != nil {
		return f.HandleError(err)
	}

	prevOuts := toPrevOuts(
		f.loopIn.Deposits, f.loopIn.AddressParams.PkScript,
	)
	totalValue := totalValue(prevOuts)

	outpoints := toOutpoints(f.loopIn.Deposits)
	sweeplessTx, err := createSweeplessSweepTx(
		outpoints, totalValue, f.loopIn.SweeplessSweepAddress,
		f.loopIn.SweeplessFeeRate,
	)
	if err != nil {
		return f.HandleError(err)
	}
	fmt.Printf("sweeplessTx: %v\n", sweeplessTx.TxHash())
	fmt.Printf("sweepless client nonces: %x\n", f.sweeplessClientNonces)
	fmt.Printf("sweepless server nonces: %x\n", f.sweeplessServerNonces)

	// Next we'll get our htlc tx signatures.
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sweeplessSigs, err := signMusig2Tx(
		f.ctx, prevOutFetcher, outpoints, f.cfg.Signer, sweeplessTx,
		f.sweeplessMusig2Sessions, f.sweeplessServerNonces,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.sweeplessClientSigs = sweeplessSigs
	fmt.Printf("client htlc sigs: %x\n", sweeplessSigs)

	// Push Htlc sigs to server
	pushSweeplessSigsReq := &swapserverrpc.PushStaticAddressSweeplessSigsRequest{
		LoopInId:              f.loopIn.ID[:],
		SweeplessClientNonces: fromNonces(f.sweeplessClientNonces),
		SweeplessClientSigs:   f.sweeplessClientSigs,
	}
	_, err = f.cfg.StaticAddressServerClient.PushStaticAddressSweeplessSigs( //nolint:lll
		f.ctx, pushSweeplessSigsReq,
	)
	if err != nil {
		return f.HandleError(err)
	}

	return OnSweeplessSweepSigned
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

// createMusig2Sessions creates a musig2 session for a number of deposits.
func createMusig2Sessions(ctx context.Context, deposits []*deposit.Deposit,
	addressParams *address.Parameters, address *script.StaticAddress,
	signer lndclient.SignerClient) ([]*input.MuSig2SessionInfo, [][]byte,
	error) {

	musig2Sessions := make([]*input.MuSig2SessionInfo, len(deposits))
	clientNonces := make([][]byte, len(deposits))

	// Create the sessions and nonces from the deposits.
	for i := 0; i < len(deposits); i++ {
		session, err := createMusig2Session(
			ctx, addressParams, address, signer,
		)
		if err != nil {
			return nil, nil, err
		}

		musig2Sessions[i] = session
		clientNonces[i] = session.PublicNonce[:]
	}

	return musig2Sessions, clientNonces, nil
}

// Musig2CreateSession creates a musig2 session for the deposit.
func createMusig2Session(ctx context.Context, addressParams *address.Parameters,
	address *script.StaticAddress,
	signer lndclient.SignerClient) (*input.MuSig2SessionInfo, error) {

	signers := [][]byte{
		addressParams.ClientPubkey.SerializeCompressed(),
		addressParams.ServerPubkey.SerializeCompressed(),
	}

	expiryLeaf := address.TimeoutLeaf

	rootHash := expiryLeaf.TapHash()

	return signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &addressParams.KeyLocator,
		signers, lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
	)
}

func toPrevOuts(deposits []*deposit.Deposit,
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

func totalValue(prevOuts map[wire.OutPoint]*wire.TxOut) btcutil.Amount {
	var value btcutil.Amount
	for _, prevOut := range prevOuts {
		value += btcutil.Amount(prevOut.Value)
	}
	return value
}

func createHtlcTx(outpoints []wire.OutPoint, withdrawlAmount btcutil.Amount,
	sweepAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	// Calculate htlc tx fee for server provided fee rate.
	weight := htlcWeight(len(outpoints))
	fee := feeRate.FeeForWeight(weight)

	pkscript, err := txscript.PayToAddrScript(sweepAddress)
	if err != nil {
		return nil, err
	}

	// Create the sweep output
	sweepOutput := &wire.TxOut{
		Value:    int64(withdrawlAmount) - int64(fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

func createSweeplessSweepTx(outpoints []wire.OutPoint,
	withdrawlAmount btcutil.Amount, clientSweepAddress btcutil.Address,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	// First Create the tx.
	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order the server
	// signed them.
	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
		})
	}

	// Calculate tx fee for server provided fee rate.
	weight, err := sweeplessSweepWeight(len(outpoints), clientSweepAddress)
	if err != nil {
		return nil, err
	}
	fee := feeRate.FeeForWeight(weight)

	pkscript, err := txscript.PayToAddrScript(clientSweepAddress)
	if err != nil {
		return nil, err
	}

	// Create the sweep output
	sweepOutput := &wire.TxOut{
		Value:    int64(withdrawlAmount) - int64(fee),
		PkScript: pkscript,
	}

	msgTx.AddTxOut(sweepOutput)

	return msgTx, nil
}

// signMusig2Tx adds the server nonces to the musig2 sessions and signs the
// transaction.
func signMusig2Tx(ctx context.Context,
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

// sweeplessSweepWeight ...
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

func fromNonces(nonces [][musig2.PubNonceSize]byte) [][]byte {
	var result [][]byte
	for _, nonce := range nonces {
		temp := make([]byte, musig2.PubNonceSize)
		copy(temp, nonce[:])
		result = append(result, temp)
	}

	return result
}
