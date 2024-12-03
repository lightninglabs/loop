package hyperloop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ID is a unique identifier for a hyperloop.
type ID [32]byte

// NewHyperLoopId creates a new hyperloop id.
func NewHyperLoopId() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}

// ParseHyperloopId parses the given id into a hyperloop id.
func ParseHyperloopId(id []byte) (ID, error) {
	var newID ID
	if len(id) != 32 {
		return newID, fmt.Errorf("invalid hyperloop id")
	}
	copy(newID[:], id)
	return newID, nil
}

// String returns a human-readable string representation of the hyperloop id.
func (i ID) String() string {
	return hex.EncodeToString(i[:])
}

type Hyperloop struct {
	// ID is the hyperloop identifier, this can be the same between multiple
	// hyperloops, as swaps can be batched.
	ID ID

	// PublishTime is the time where we request the hyperloop output to be
	// published.
	PublishTime time.Time

	// State is the current state of the hyperloop.
	State fsm.StateType

	// SwapHash is the hash of the swap preimage and identifier of the htlc
	// that goes onchain.
	SwapHash lntypes.Hash

	// SwapPreimage is the preimage of the swap hash.
	SwapPreimage lntypes.Preimage

	// SwapInvoice is the invoice that the server sends to the client and the
	// client pays.
	SwapInvoice string

	// InitiationHeight is the height at which the hyperloop was initiated.
	InitiationHeight int32

	// Amt is the requested amount of the hyperloop.
	Amt btcutil.Amount

	// SweepAddr is the address we sweep the funds to after the hyperloop is
	// completed.
	SweepAddr btcutil.Address

	// KeyDesc is the key descriptor used to derive the keys for the hyperloop.
	KeyDesc *keychain.KeyDescriptor

	// ServerKey is the key of the server used in the hyperloop musig2 output.
	ServerKey *btcec.PublicKey

	// Participants are all the participants of the hyperloop including the
	// requesting client.
	Participants []*HyperloopParticipant

	// HtlcFeeRates are the fee rates for the htlc tx.
	HtlcFeeRates []chainfee.SatPerKWeight

	// HyperloopExpiry is the csv expiry of the hyperloop output.
	HyperLoopExpiry uint32

	// ConfirmedOutpoint is the outpoint of the confirmed hyperloop output.
	ConfirmedOutpoint *wire.OutPoint

	// ConfirmedValue is the value of the confirmed hyperloop output.
	ConfirmedValue btcutil.Amount

	// ConfirmationHeight is the height at which the hyperloop output was
	// confirmed.
	ConfirmationHeight int32

	// HtlcExpiry is the expiry of each of the htlc swap outputs.
	HtlcExpiry int32

	// HtlcMusig2Sessions are the musig2 session for the htlc tx.
	HtlcMusig2Sessions map[int64]*input.MuSig2SessionInfo

	// HtlcServerNonces are the nonces of the server for the htlc tx.
	HtlcServerNonces map[int64][66]byte

	// HtlcRawTxns are the raw txns for the htlc tx.
	HtlcRawTxns map[int64]*wire.MsgTx

	// HtlcFullSigs are the full signatures for the htlc tx.
	HtlcFullSigs map[int64][]byte

	// SweeplessSweepMusig2Session is the musig2 session for the sweepless
	// sweep tx.
	SweeplessSweepMusig2Session *input.MuSig2SessionInfo

	// SweepServerNonce is the nonce of the server for the sweepless sweep
	// tx.
	SweepServerNonce [66]byte

	// SweeplessSweepRawTx is the raw tx for the sweepless sweep tx.
	SweeplessSweepRawTx *wire.MsgTx
}

// newHyperLoop creates a new hyperloop with the given parameters.
func newHyperLoop(publishTime time.Time, amt btcutil.Amount,
	keyDesc *keychain.KeyDescriptor, preimage lntypes.Preimage,
	sweepAddr btcutil.Address, initiationHeight int32) *Hyperloop {

	return &Hyperloop{
		PublishTime:        publishTime,
		SwapPreimage:       preimage,
		SwapHash:           preimage.Hash(),
		KeyDesc:            keyDesc,
		Amt:                amt,
		SweepAddr:          sweepAddr,
		InitiationHeight:   initiationHeight,
		HtlcMusig2Sessions: make(map[int64]*input.MuSig2SessionInfo),
		HtlcServerNonces:   make(map[int64][66]byte),
		HtlcFullSigs:       make(map[int64][]byte),
		HtlcRawTxns:        make(map[int64]*wire.MsgTx),
	}
}

// addInitialHyperLoopInfo adds the initial server info to the hyperloop.
func (h *Hyperloop) addInitialHyperLoopInfo(id ID,
	hyperLoopExpiry uint32, htlcExpiry int32, serverKey *btcec.PublicKey,
	swapInvoice string) {

	h.ID = id
	h.HtlcExpiry = htlcExpiry
	h.HyperLoopExpiry = hyperLoopExpiry
	h.ServerKey = serverKey
	h.SwapInvoice = swapInvoice
}

// String returns a human-readable string representation of the hyperloop.
// It encodes the first 3 bytes of the ID as a hex string adds a colon and then
// the first 3 bytes of the swap hash.
func (h *Hyperloop) String() string {
	return fmt.Sprintf("%x:%x", h.ID[:3], h.SwapHash[:3])
}

// HyperloopParticipant is a helper struct to store the participant information
// of the hyperloop.
type HyperloopParticipant struct {
	pubkey       *btcec.PublicKey
	sweepAddress btcutil.Address
}

// startHtlcSessions starts the musig2 sessions for each fee rate of the htlc
// tx.
func (h *Hyperloop) startHtlcFeerateSessions(ctx context.Context,
	signer lndclient.SignerClient) error {

	for _, feeRate := range h.HtlcFeeRates {
		session, err := h.startHtlcSession(ctx, signer)
		if err != nil {
			return err
		}

		h.HtlcMusig2Sessions[int64(feeRate)] = session
	}

	return nil
}

// startHtlcSession starts a musig2 session for the htlc tx.
func (h *Hyperloop) startHtlcSession(ctx context.Context,
	signer lndclient.SignerClient) (*input.MuSig2SessionInfo, error) {

	rootHash, err := h.getHyperLoopRootHash()
	if err != nil {
		return nil, err
	}

	allPubkeys, err := h.getHyperLoopPubkeys()
	if err != nil {
		return nil, err
	}

	var rawKeys [][]byte
	for _, key := range allPubkeys {
		key := key
		rawKeys = append(rawKeys, key.SerializeCompressed())
	}

	htlcSession, err := signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &h.KeyDesc.KeyLocator,
		rawKeys,
		lndclient.MuSig2TaprootTweakOpt(rootHash, false),
	)
	if err != nil {
		return nil, err
	}

	return htlcSession, nil
}

func (h *Hyperloop) getHtlcFeeRateMusig2Nonces() map[int64][]byte {
	nonces := make(map[int64][]byte)
	for feeRate, session := range h.HtlcMusig2Sessions {
		nonces[feeRate] = session.PublicNonce[:]
	}

	return nonces
}

// startSweeplessSession starts the musig2 session for the sweepless sweep tx.
func (h *Hyperloop) startSweeplessSession(ctx context.Context,
	signer lndclient.SignerClient) error {

	rootHash, err := h.getHyperLoopRootHash()
	if err != nil {
		return err
	}

	allPubkeys, err := h.getHyperLoopPubkeys()
	if err != nil {
		return err
	}

	var rawKeys [][]byte
	for _, key := range allPubkeys {
		key := key
		rawKeys = append(rawKeys, key.SerializeCompressed())
	}

	sweeplessSweepSession, err := signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2,
		&h.KeyDesc.KeyLocator, rawKeys,
		lndclient.MuSig2TaprootTweakOpt(rootHash, false),
	)
	if err != nil {
		return err
	}

	h.SweeplessSweepMusig2Session = sweeplessSweepSession

	return nil
}

// registerHtlcNonces registers the htlc nonces for the hyperloop.
func (h *Hyperloop) registerHtlcNonces(ctx context.Context,
	signer lndclient.SignerClient, nonceMap map[int64][][66]byte) error {

	for feeRate, nonces := range nonceMap {
		session, ok := h.HtlcMusig2Sessions[feeRate]
		if !ok {
			return fmt.Errorf("missing session for fee rate %d",
				feeRate)
		}
		noncesWithoutOwn := h.removeOwnNonceFromSet(
			session, nonces,
		)

		serverNonce, ok := h.HtlcServerNonces[feeRate]
		if !ok {
			return fmt.Errorf("missing server nonce for fee rate "+
				"%d", feeRate)
		}
		noncesWithoutOwn = append(noncesWithoutOwn, serverNonce)

		haveAllNonces, err := signer.MuSig2RegisterNonces(
			ctx, session.SessionID, noncesWithoutOwn,
		)
		if err != nil {
			return err
		}

		if !haveAllNonces {
			return fmt.Errorf("not all nonces registered")
		}
	}

	return nil
}

// registerSweeplessSweepNonces registers the sweepless sweep nonces for the
// hyperloop.
func (h *Hyperloop) registerSweeplessSweepNonces(ctx context.Context,
	signer lndclient.SignerClient, nonces [][66]byte) error {

	nonces = append(nonces, h.SweepServerNonce)

	noncesWithoutOwn := h.removeOwnNonceFromSet(
		h.SweeplessSweepMusig2Session, nonces,
	)

	haveAllNonces, err := signer.MuSig2RegisterNonces(
		ctx, h.SweeplessSweepMusig2Session.SessionID, noncesWithoutOwn,
	)
	if err != nil {
		return err
	}
	if !haveAllNonces {
		return fmt.Errorf("not all nonces registered")
	}

	return nil
}

// removeOwnNonceFromSet removes the own nonce from the set of nonces.
func (h *Hyperloop) removeOwnNonceFromSet(session *input.MuSig2SessionInfo,
	nonces [][66]byte) [][66]byte {

	var ownNonce [66]byte
	copy(ownNonce[:], session.PublicNonce[:])

	var noncesWithoutOwn [][66]byte
	for _, nonce := range nonces {
		nonce := nonce
		if nonce != ownNonce {
			noncesWithoutOwn = append(noncesWithoutOwn, nonce)
		}
	}

	return noncesWithoutOwn
}

// setHtlcFeeRates sets the fee rates for the htlc tx.
func (h *Hyperloop) setHtlcFeeRates(feeRates []int64) {
	h.HtlcFeeRates = make([]chainfee.SatPerKWeight, len(feeRates))
	for i, feeRate := range feeRates {
		h.HtlcFeeRates[i] = chainfee.SatPerKWeight(feeRate)
	}
}

// getSigForTx returns the signature for the given hyperloop tx with the
// specified musig2 session id.
func (h *Hyperloop) getSigForTx(ctx context.Context,
	signer lndclient.SignerClient, tx *wire.MsgTx,
	musig2SessionId [32]byte) ([]byte, error) {

	sigHash, err := h.getTxSighash(tx)
	if err != nil {
		return nil, err
	}

	sig, err := signer.MuSig2Sign(
		ctx, musig2SessionId, sigHash, false,
	)
	if err != nil {
		return nil, err
	}

	return sig, nil
}

// getTxSighash returns the sighash for the given hyperloop tx.
func (h *Hyperloop) getTxSighash(tx *wire.MsgTx) ([32]byte, error) {
	prevOutFetcher, err := h.getHyperLoopPrevOutputFetcher()
	if err != nil {
		return [32]byte{}, err
	}

	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	taprootSigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault,
		tx, 0, prevOutFetcher,
	)
	if err != nil {
		return [32]byte{}, err
	}

	var digest [32]byte
	copy(digest[:], taprootSigHash)

	return digest, nil
}

// getHyperLoopPrevOutputFetcher returns a canned prev output fetcher for the
// hyperloop. This is required for the signature generation and verification.
func (h *Hyperloop) getHyperLoopPrevOutputFetcher() (txscript.PrevOutputFetcher,
	error) {

	hyperloopScript, err := h.getHyperLoopScript()
	if err != nil {
		return nil, err
	}

	return txscript.NewCannedPrevOutputFetcher(
		hyperloopScript, int64(h.ConfirmedValue),
	), nil
}

// getHtlc returns the htlc for the participant that is the client.
func (h *Hyperloop) getHtlc(chainParams *chaincfg.Params) (*swap.Htlc,
	error) {

	serverKey, err := get33Bytes(h.ServerKey)
	if err != nil {
		return nil, err
	}

	clientKey, err := get33Bytes(h.KeyDesc.PubKey)
	if err != nil {
		return nil, err
	}

	htlcV2, err := swap.NewHtlcV2(
		h.HtlcExpiry, serverKey, clientKey,
		h.SwapHash, chainParams,
	)
	if err != nil {
		return nil, err
	}

	return htlcV2, nil
}

// getHyperLoopScript returns the pkscript for the hyperloop output.
func (h *Hyperloop) getHyperLoopScript() ([]byte, error) {
	pubkeys, err := h.getHyperLoopPubkeys()
	if err != nil {
		return nil, err
	}

	return HyperLoopScript(h.HyperLoopExpiry, h.ServerKey, pubkeys)
}

// getHyperLoopRootHash returns the root hash of the hyperloop.
func (h *Hyperloop) getHyperLoopRootHash() ([]byte, error) {
	pubkeys, err := h.getHyperLoopPubkeys()
	if err != nil {
		return nil, err
	}

	_, _, leaf, err := TaprootKey(
		h.HyperLoopExpiry, h.ServerKey, pubkeys,
	)
	if err != nil {
		return nil, err
	}

	rootHash := leaf.TapHash()

	return rootHash[:], nil
}

// getHyperLoopPubkeys returns the pubkeys of all participants and the server.
func (h *Hyperloop) getHyperLoopPubkeys() ([]*btcec.PublicKey, error) {
	pubkeys := make([]*btcec.PublicKey, 0, len(h.Participants))
	for _, participant := range h.Participants {
		participant := participant
		if participant.pubkey == nil {
			return nil, fmt.Errorf("missing participant pubkey")
		}
		pubkeys = append(pubkeys, participant.pubkey)
	}

	pubkeys = append(pubkeys, h.ServerKey)

	return pubkeys, nil
}

// getHtlcFee returns the absolute fee for the htlc tx.
func (h *Hyperloop) getHtlcFee(feeRate chainfee.SatPerKWeight,
) btcutil.Amount {

	var weightEstimate input.TxWeightEstimator

	// Add the input.
	weightEstimate.AddTaprootKeySpendInput(txscript.SigHashAll)

	// Add the outputs.
	for i := 0; i < len(h.Participants); i++ {
		weightEstimate.AddP2WSHOutput()
	}

	weight := weightEstimate.Weight()

	return feeRate.FeeForWeight(weight)
}

// registerHtlcTx registers the htlc tx for the hyperloop and checks that it is
// valid.
func (h *Hyperloop) registerHtlcTx(chainParams *chaincfg.Params,
	feeRate int64, htlcTx *wire.MsgTx) error {

	myHtlc, err := h.getHtlc(chainParams)
	if err != nil {
		return err
	}

	// Get the Htlc Fee.
	absoluteFee := h.getHtlcFee(chainfee.SatPerKWeight(feeRate))
	// Get the fee per output.
	feePerOutput := absoluteFee / btcutil.Amount(len(h.Participants))

	// First we'll check that our htlc output is part of the tx.
	foundPkScript := false
	for _, txOut := range htlcTx.TxOut {
		if bytes.Equal(txOut.PkScript, myHtlc.PkScript) {
			foundPkScript = true
			if btcutil.Amount(txOut.Value) != h.Amt-feePerOutput {
				return fmt.Errorf("invalid htlc amount, "+
					"expected %v got %v", h.Amt,
					txOut.Value)
			}
			break
		}
	}
	if !foundPkScript {
		return fmt.Errorf("htlc output not found")
	}

	// Check that one of the inputs is the confirmed hyperloop output.
	var foundInput bool
	for _, txIn := range htlcTx.TxIn {
		if txIn.PreviousOutPoint == *h.ConfirmedOutpoint {
			foundInput = true
			break
		}
	}

	if !foundInput {
		return fmt.Errorf("sweepless sweep input not found")
	}

	// Check that the sequence number is correct.
	// Todo config this or check otherwise e.g. against invoice expiries,
	// or the hyperloop expiry.
	if htlcTx.TxIn[0].Sequence != 5 {
		return fmt.Errorf("invalid htlc sequence, expected 5 got %v",
			htlcTx.TxIn[0].Sequence)
	}

	// TODO (sputn1ck) calculate actual fee and check that it is correct.
	h.HtlcRawTxns[feeRate] = htlcTx

	return nil
}

// registerHtlcSig registers the htlc signature for the hyperloop tx and
// checks that it is valid.
func (h *Hyperloop) registerHtlcSig(feeRate int64, sig []byte) error {
	htlcTx, ok := h.HtlcRawTxns[feeRate]
	if !ok {
		return fmt.Errorf("missing htlc tx")
	}

	return h.checkSigForTx(htlcTx, sig)
}

// checkSigForTx checks the given signature for the given version of the
// hyperloop tx.
func (h *Hyperloop) checkSigForTx(tx *wire.MsgTx, sig []byte) error {
	tx.TxIn[0].Witness = [][]byte{
		sig,
	}

	pkscript, err := h.getHyperLoopScript()
	if err != nil {
		return err
	}

	prevOutFetcher, err := h.getHyperLoopPrevOutputFetcher()
	if err != nil {
		return err
	}
	hashCache := txscript.NewTxSigHashes(
		tx, prevOutFetcher,
	)

	engine, err := txscript.NewEngine(
		pkscript, tx, 0, txscript.StandardVerifyFlags,
		nil, hashCache, int64(h.ConfirmedValue), prevOutFetcher,
	)
	if err != nil {
		return err
	}

	return engine.Execute()
}

// registerSweeplessSweepTx registers the sweepless sweep tx for the hyperloop
// and checks that it is valid.
func (h *Hyperloop) registerSweeplessSweepTx(sweeplessSweepTx *wire.MsgTx,
	feeRate chainfee.SatPerKWeight, totalSweepAmt btcutil.Amount) error {

	myPkScript, err := txscript.PayToAddrScript(h.SweepAddr)
	if err != nil {
		return err
	}

	// Create a map of addresses
	sweepPkScriptAmtMap := make(map[btcutil.Address]interface{})
	for _, participant := range h.Participants {
		participant := participant
		sweepPkScriptAmtMap[participant.sweepAddress] = 0
	}

	sweeplessSweepFee, err := h.getSweeplessSweepFee(
		feeRate, sweepPkScriptAmtMap,
	)
	if err != nil {
		return err
	}

	feePerOutput := sweeplessSweepFee / btcutil.Amount(len(sweepPkScriptAmtMap))

	var foundPkScript bool

	// Check that the output is as we expect, where we sweep all the funds
	// to the sweep address.
	for _, txOut := range sweeplessSweepTx.TxOut {
		if bytes.Equal(txOut.PkScript, myPkScript) {
			foundPkScript = true
			if btcutil.Amount(txOut.Value) != totalSweepAmt-feePerOutput {
				return fmt.Errorf("invalid sweepless sweep"+
					" amount, expected %v got %v",
					totalSweepAmt-feePerOutput, txOut.Value)
			}
			break
		}
	}

	if !foundPkScript {
		return fmt.Errorf("sweepless sweep output not found")
	}

	// Check that one of the inputs is the confirmed hyperloop output.
	var foundInput bool
	for _, txIn := range sweeplessSweepTx.TxIn {
		if txIn.PreviousOutPoint == *h.ConfirmedOutpoint {
			foundInput = true
			break
		}
	}

	if !foundInput {
		return fmt.Errorf("sweepless sweep input not found")
	}

	h.SweeplessSweepRawTx = sweeplessSweepTx

	return nil
}

// getSweeplessSweepFee returns the absolute fee for the sweepless sweep tx.
func (h *Hyperloop) getSweeplessSweepFee(feeRate chainfee.SatPerKWeight,
	sweepAddrMap map[btcutil.Address]interface{}) (btcutil.Amount,
	error) {

	var weightEstimate input.TxWeightEstimator

	// Add the input.
	weightEstimate.AddTaprootKeySpendInput(txscript.SigHashAll)

	// Add the outputs.
	for sweepAddr := range sweepAddrMap {
		switch sweepAddr.(type) {
		case *btcutil.AddressWitnessScriptHash:
			weightEstimate.AddP2WSHOutput()

		case *btcutil.AddressWitnessPubKeyHash:
			weightEstimate.AddP2WKHOutput()

		case *btcutil.AddressScriptHash:
			weightEstimate.AddP2SHOutput()

		case *btcutil.AddressPubKeyHash:
			weightEstimate.AddP2PKHOutput()

		case *btcutil.AddressTaproot:
			weightEstimate.AddP2TROutput()

		default:
			return 0, fmt.Errorf("estimate fee: unknown address"+
				" type %T", sweepAddr)
		}
	}

	weight := weightEstimate.Weight()

	return feeRate.FeeForWeight(weight), nil
}

// get33Bytes returns the 33 byte representation of the given pubkey.
func get33Bytes(pubkey *btcec.PublicKey) ([33]byte, error) {
	var key [33]byte
	if pubkey == nil {
		return key, fmt.Errorf("missing participant pubkey")
	}
	if len(pubkey.SerializeCompressed()) != 33 {
		return key, fmt.Errorf("invalid participant pubkey")
	}
	copy(key[:], pubkey.SerializeCompressed())

	return key, nil
}

// ToRpcHyperloop converts the hyperloop to the rpc representation.
func (h *Hyperloop) ToRpcHyperloop() *looprpc.HyperLoopOut {
	return &looprpc.HyperLoopOut{
		IdBytes:         h.ID[:],
		Id:              h.ID.String(),
		SwapHash:        h.SwapHash.String(),
		SwapHashBytes:   h.SwapHash[:],
		Amt:             uint64(h.Amt),
		State:           string(h.State),
		PublishTimeUnix: h.PublishTime.Unix(),
		SweepAddress:    h.SweepAddr.String(),
	}
}
