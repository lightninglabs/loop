package hyperloop

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

type ID [32]byte

func NewHyperLoopId() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}

func ParseHyperloopId(id []byte) (ID, error) {
	var newID ID
	if len(id) != 32 {
		return newID, fmt.Errorf("invalid hyperloop id")
	}
	copy(newID[:], id)
	return newID, nil
}

type HyperLoop struct {
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
	Participants []*HyperLoopParticipant

	// HyperloopExpiry is the csv expiry of the hyperloop output.
	HyperLoopExpiry uint32

	// ConfirmedOutpoint is the outpoint of the confirmed hyperloop output.
	ConfirmedOutpoint *wire.OutPoint

	// ConfirmationHeight is the height at which the hyperloop output was
	// confirmed.
	ConfirmationHeight int32

	// HtlcFeeRate is the fee rate we'll use for the htlc tx.
	HtlcFeeRate chainfee.SatPerKWeight

	// HtlcExpiry is the expiry of each of the htlc swap outputs.
	HtlcExpiry int32

	// HtlcMusig2Session is the musig2 session for the htlc tx.
	HtlcMusig2Session *input.MuSig2SessionInfo

	// HtlcFullSig is the completed and valid signature for the htlc tx.
	HtlcFullSig []byte

	// SweeplessSweepFeeRate is the fee rate we'll use for the sweepless sweep
	// tx.
	SweeplessSweepFeeRate chainfee.SatPerKWeight

	// SweeplessSweepMusig2Session is the musig2 session for the sweepless
	// sweep tx.
	SweeplessSweepMusig2Session *input.MuSig2SessionInfo

	// SweepServerNonce is the nonce of the server for the sweepless sweep tx.
	SweepServerNonce [66]byte

	// sweeplessSweepAddrMap is a map of swap hashes to addresses for the
	// sweepless sweep tx.
	sweeplessSweepAddrMap map[lntypes.Hash]btcutil.Address
}

// newHyperLoop creates a new hyperloop with the given parameters.
func newHyperLoop(id ID, publishTime time.Time, amt btcutil.Amount,
	keyDesc *keychain.KeyDescriptor, preimage lntypes.Preimage,
	sweepAddr btcutil.Address) *HyperLoop {

	return &HyperLoop{
		ID:                    id,
		PublishTime:           publishTime,
		SwapPreimage:          preimage,
		SwapHash:              preimage.Hash(),
		KeyDesc:               keyDesc,
		Amt:                   amt,
		SweepAddr:             sweepAddr,
		sweeplessSweepAddrMap: make(map[lntypes.Hash]btcutil.Address),
	}
}

// addInitialHyperLoopInfo adds the initial server info to the hyperloop.
func (h *HyperLoop) addInitialHyperLoopInfo(HtlcFeeRate chainfee.SatPerKWeight,
	HyperLoopExpiry uint32, HtlcExpiry int32, serverKey *btcec.PublicKey,
	swapInvoice string) {
	h.HtlcFeeRate = HtlcFeeRate
	h.HtlcExpiry = HtlcExpiry
	h.HyperLoopExpiry = HyperLoopExpiry
	h.ServerKey = serverKey
	h.SwapInvoice = swapInvoice
}

// String returns a human-readable string representation of the hyperloop.
// It encodes the first 3 bytes of the ID as a hex string adds a colon and then
// the first 3 bytes of the swap hash.
func (h *HyperLoop) String() string {
	return fmt.Sprintf("%x:%x", h.ID[:3], h.SwapHash[:3])
}

type HyperLoopParticipant struct {
	SwapHash     lntypes.Hash
	Amt          btcutil.Amount
	Pubkey       *btcec.PublicKey
	SweepAddress btcutil.Address
}

// addParticipant adds a participant to the hyperloop and adds the sweep address
// to the sweepless sweep address map.
func (h *HyperLoop) registerParticipants(participants []*HyperLoopParticipant) {
	h.Participants = participants
	for _, p := range participants {
		h.sweeplessSweepAddrMap[p.SwapHash] = p.SweepAddress
	}
}

// startHtlcSession starts the musig2 session for the htlc tx.
func (h *HyperLoop) startHtlcSession(ctx context.Context,
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

	htlcSession, err := signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &h.KeyDesc.KeyLocator,
		rawKeys,
		lndclient.MuSig2TaprootTweakOpt(rootHash, false),
	)
	if err != nil {
		return err
	}

	h.HtlcMusig2Session = htlcSession
	return nil
}

// startSweeplessSession starts the musig2 session for the sweepless sweep tx.
func (h *HyperLoop) startSweeplessSession(ctx context.Context,
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
func (h *HyperLoop) registerHtlcNonces(ctx context.Context,
	signer lndclient.SignerClient, nonces [][66]byte) error {

	noncesWithoutOwn, err := h.removeOwnNonceFromSet(
		h.HtlcMusig2Session, nonces,
	)
	if err != nil {
		return err
	}
	haveAllNonces, err := signer.MuSig2RegisterNonces(
		ctx, h.HtlcMusig2Session.SessionID, noncesWithoutOwn,
	)
	if err != nil {
		return err
	}

	if !haveAllNonces {
		return fmt.Errorf("not all nonces registered")
	}

	return nil
}

// registerSweeplessSweepNonces registers the sweepless sweep nonces for the
// hyperloop.
func (h *HyperLoop) registerSweeplessSweepNonces(ctx context.Context,
	signer lndclient.SignerClient, nonces [][66]byte) error {

	nonces = append(nonces, h.SweepServerNonce)

	noncesWithoutOwn, err := h.removeOwnNonceFromSet(
		h.SweeplessSweepMusig2Session, nonces,
	)
	if err != nil {
		return err
	}

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
func (h *HyperLoop) removeOwnNonceFromSet(session *input.MuSig2SessionInfo,
	nonces [][66]byte) ([][66]byte, error) {

	var ownNonce [66]byte
	copy(ownNonce[:], session.PublicNonce[:])

	var noncesWithoutOwn [][66]byte
	for _, nonce := range nonces {
		nonce := nonce
		if nonce != ownNonce {
			noncesWithoutOwn = append(noncesWithoutOwn, nonce)
		}
	}

	return noncesWithoutOwn, nil
}

// getHyperLoopSweeplessSweepTx returns the sweepless sweep tx for the hyperloop.
func (h *HyperLoop) getHyperLoopSweeplessSweepTx() (*wire.MsgTx, error) {

	sweeplessSweepTx := wire.NewMsgTx(2)

	// Add the hyperloop input.
	sweeplessSweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *h.ConfirmedOutpoint,
	})

	// Create a map of addresses to amounts.
	sweepPkScriptAmtMap := make(map[btcutil.Address]btcutil.Amount)
	for _, participant := range h.Participants {
		participant := participant
		sweepPkScriptAmtMap[h.sweeplessSweepAddrMap[participant.SwapHash]] += participant.Amt
	}

	sweeplessSweepFee, err := h.getSweeplessSweepFee()
	if err != nil {
		return nil, err
	}
	feePerOutput := sweeplessSweepFee / btcutil.Amount(len(sweepPkScriptAmtMap))

	// Add all the sweepless sweep outputs.
	for addr, amt := range sweepPkScriptAmtMap {
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
		log.Infof("Adding output %x %v", pkScript, amt)
		sweeplessSweepTx.AddTxOut(&wire.TxOut{
			PkScript: []byte(pkScript),
			Value:    int64(amt - feePerOutput),
		})
		sweeplessSweepTx.AddTxOut(&wire.TxOut{
			PkScript: []byte(pkScript),
			Value:    int64(amt - feePerOutput),
		})
	}

	txsort.InPlaceSort(sweeplessSweepTx)

	return sweeplessSweepTx, nil
}

// getHtlcFee returns the absolute fee for the htlc tx.
func (h *HyperLoop) getHtlcFee() btcutil.Amount {
	var weightEstimate input.TxWeightEstimator

	// Add the input.
	weightEstimate.AddTaprootKeySpendInput(txscript.SigHashAll)

	// Add the outputs.
	for i := 0; i < len(h.Participants); i++ {
		weightEstimate.AddP2WSHOutput()
	}

	weight := weightEstimate.Weight()

	return h.HtlcFeeRate.FeeForWeight(weight)
}

// getSweeplessSweepFee returns the absolute fee for the sweepless sweep tx.
func (h *HyperLoop) getSweeplessSweepFee() (btcutil.Amount, error) {
	var weightEstimate input.TxWeightEstimator

	// Add the input.
	weightEstimate.AddTaprootKeySpendInput(txscript.SigHashAll)

	// Add the outputs.
	for _, sweepAddr := range h.sweeplessSweepAddrMap {
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
			return 0, fmt.Errorf("estimate fee: unknown address type %T",
				sweepAddr)
		}
	}

	weight := weightEstimate.Weight()

	return h.SweeplessSweepFeeRate.FeeForWeight(weight), nil
}

// getHtlcSig returns the signature for the htlc tx.
func (h *HyperLoop) getHtlcSig(ctx context.Context,
	signer lndclient.SignerClient, chainParams *chaincfg.Params) (
	[]byte, error) {

	// First get the hyperloop htlc tx.
	htlcTx, err := h.getHyperLoopHtlcTx(chainParams)
	if err != nil {
		return nil, err
	}

	return h.getSigForTx(ctx, signer, htlcTx, h.HtlcMusig2Session.SessionID)
}

// getSweeplessSweepSig returns the signature for the sweepless sweep tx.
func (h *HyperLoop) getSweeplessSweepSig(ctx context.Context,
	signer lndclient.SignerClient) ([]byte, error) {

	// First get the hyperloop sweepless sweep tx.
	sweeplessSweepTx, err := h.getHyperLoopSweeplessSweepTx()
	if err != nil {
		return nil, err
	}

	return h.getSigForTx(
		ctx, signer, sweeplessSweepTx, h.SweeplessSweepMusig2Session.SessionID,
	)
}

// getSigForTx returns the signature for the given hyperloop tx.
func (h *HyperLoop) getSigForTx(ctx context.Context,
	signer lndclient.SignerClient, tx *wire.MsgTx, musig2SessionId [32]byte) (
	[]byte, error) {

	sigHash, err := h.getTxSighash(tx)
	if err != nil {
		return nil, err
	}

	// Now we can get the sig.
	sig, err := signer.MuSig2Sign(
		ctx, musig2SessionId, sigHash, false,
	)
	if err != nil {
		return nil, err
	}

	return sig, nil
}

// getTxSighash returns the sighash for the given hyperloop tx.
func (h *HyperLoop) getTxSighash(tx *wire.MsgTx) ([32]byte, error) {
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
func (h *HyperLoop) getHyperLoopPrevOutputFetcher() (txscript.PrevOutputFetcher,
	error) {
	hyperloopScript, err := h.getHyperLoopScript()
	if err != nil {
		return nil, err
	}

	return txscript.NewCannedPrevOutputFetcher(
		hyperloopScript, int64(h.getHyperLoopTotalAmt()),
	), nil
}

// getHyperLoopTotalAmt returns the total amount of the hyperloop.
func (h *HyperLoop) getHyperLoopTotalAmt() btcutil.Amount {
	hyperLoopAmt := btcutil.Amount(0)
	for _, input := range h.Participants {
		input := input
		hyperLoopAmt += input.Amt
	}
	return hyperLoopAmt
}

// getHyperLoopHtlcTx returns the htlc spending tx for the hyperloop.
func (h *HyperLoop) getHyperLoopHtlcTx(chainParams *chaincfg.Params) (
	*wire.MsgTx, error) {

	htlcTx := wire.NewMsgTx(2)

	// Add the hyperloop input.
	htlcTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *h.ConfirmedOutpoint,
	})

	// Get the fee per output.
	htlcFee := h.getHtlcFee()
	feePerOutput := htlcFee / btcutil.Amount(len(h.Participants))

	// Add all the htlc outputs.
	for _, participant := range h.Participants {
		participant := participant
		serverkey, err := get33Bytes(h.ServerKey)
		if err != nil {
			return nil, err
		}

		clientkey, err := get33Bytes(participant.Pubkey)
		if err != nil {
			return nil, err
		}

		htlcv2, err := swap.NewHtlcV2(
			h.HtlcExpiry, serverkey, clientkey,
			participant.SwapHash, chainParams,
		)
		if err != nil {
			return nil, err
		}

		htlcTx.AddTxOut(&wire.TxOut{
			PkScript: htlcv2.PkScript,
			Value:    int64(participant.Amt - feePerOutput),
		})
	}

	return htlcTx, nil
}

// getHyperLoopScript returns the pkscript for the hyperloop output.
func (h *HyperLoop) getHyperLoopScript() ([]byte, error) {
	pubkeys, err := h.getHyperLoopPubkeys()
	if err != nil {
		return nil, err
	}

	return HyperLoopScript(h.HyperLoopExpiry, h.ServerKey, pubkeys)
}

// getHyperLoopRootHash returns the root hash of the hyperloop.
func (h *HyperLoop) getHyperLoopRootHash() ([]byte, error) {

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
func (h *HyperLoop) getHyperLoopPubkeys() ([]*btcec.PublicKey, error) {
	pubkeys := make([]*btcec.PublicKey, 0, len(h.Participants))
	for _, participant := range h.Participants {
		participant := participant
		if participant.Pubkey == nil {
			return nil, fmt.Errorf("missing participant pubkey")
		}
		pubkeys = append(pubkeys, participant.Pubkey)
	}

	pubkeys = append(pubkeys, h.ServerKey)

	return pubkeys, nil
}

// registerHtlcSig registers the htlc signature for the hyperloop tx and
// checks that it is valid.
func (h *HyperLoop) registerHtlcSig(chainParams *chaincfg.Params,
	sig []byte) error {

	h.HtlcFullSig = sig
	return h.checkHtlcSig(chainParams)
}

// checkHtlcSig checks that the htlc signature is valid for the hyperloop tx.
func (h *HyperLoop) checkHtlcSig(chainParams *chaincfg.Params) error {
	// First get the hyperloop htlc tx.
	htlcTx, err := h.getHyperLoopHtlcTx(chainParams)
	if err != nil {
		return err
	}

	return h.checkSigForTx(htlcTx, h.HtlcFullSig)
}

// checkSigForTx checks the given signature for the given version of the
// hyperloop tx.
func (h *HyperLoop) checkSigForTx(tx *wire.MsgTx, sig []byte) error {
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
		nil, hashCache, int64(h.getHyperLoopTotalAmt()), prevOutFetcher,
	)
	if err != nil {
		return err
	}

	return engine.Execute()
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
