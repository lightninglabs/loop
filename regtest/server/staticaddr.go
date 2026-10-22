package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	staticloopin "github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// A static Loop In uses a substantially longer safety window than a
	// regular Loop In. Keep this in sync with
	// staticaddr/loopin.DefaultLoopInOnChainCltvDelta.
	staticLoopInCltvDelta = int32(
		staticloopin.DefaultLoopInOnChainCltvDelta,
	)

	// The server accepts deposits after their first confirmation. The CSV
	// lifetime policy mirrors staticaddr/loopin.IsSwappable: the deposit
	// must outlive the HTLC by DepositHtlcDelta blocks.
	staticDepositMinConfirmations = int64(1)
	staticDepositMinLifetime      = int64(
		staticloopin.DefaultLoopInOnChainCltvDelta +
			staticloopin.DepositHtlcDelta,
	)

	// The three transactions are pre-signed before the server pays the
	// invoice. The latter two are fee-bump fallbacks for the first one.
	staticStandardFeeRate = chainfee.SatPerKWeight(253)
	staticHighFeeRate     = chainfee.SatPerKWeight(500)
	staticExtremeFeeRate  = chainfee.SatPerKWeight(1_000)

	staticSweepConfTarget = int32(3)
	staticSweeplessWait   = 10 * time.Second
	staticSweeplessRetry  = 500 * time.Millisecond
)

var staticFundingFeeRates = [...]chainfee.SatPerKWeight{
	staticStandardFeeRate,
	staticHighFeeRate,
	staticExtremeFeeRate,
}

// staticAddress contains both participants' public data and the locator of the
// server key in lnd. The private key never leaves lnd's signer.
type staticAddress struct {
	clientKey *btcec.PublicKey
	serverKey *serverKey
	expiry    uint32
	contract  *script.StaticAddress
	pkScript  []byte
}

// staticFundingRound is one fully deterministic static-address-to-HTLC
// transaction and the server MuSig2 sessions used to authorize its inputs.
type staticFundingRound struct {
	feeRate  chainfee.SatPerKWeight
	tx       *wire.MsgTx
	sessions []*input.MuSig2SessionInfo
	finalTx  *wire.MsgTx
}

// staticSweeplessRound is the preferred cooperative spend of the original
// static-address deposits. The server sends its PSBT and nonces only after the
// Lightning payment succeeds. If the client does not co-sign it promptly, the
// already finalized HTLC funding transactions remain the safe fallback.
type staticSweeplessRound struct {
	tx       *wire.MsgTx
	psbt     []byte
	sessions map[string]*input.MuSig2SessionInfo
	result   chan error
	finalTx  *wire.MsgTx
	closed   bool
}

// staticLoopInSwap contains everything required to complete the safe fallback
// flow. The client and server first authorize three funding transactions. Only
// then does the server pay the invoice, publish one funding transaction and
// claim its HTLC output with the payment preimage.
type staticLoopInSwap struct {
	mu sync.Mutex

	hash               lntypes.Hash
	depositStrings     []string
	deposits           []wire.OutPoint
	prevOuts           map[wire.OutPoint]*wire.TxOut
	address            *staticAddress
	changePkScript     []byte
	changeDescriptor   bool
	totalDepositAmount btcutil.Amount
	swapAmount         btcutil.Amount
	requestedAmount    uint64
	invoice            string
	lastHop            *route.Vertex
	lastHopBytes       []byte
	paymentTimeout     time.Duration
	paymentTimeoutSecs uint32
	fast               bool
	htlcClientKey      *btcec.PublicKey
	htlcServerKey      *serverKey
	htlc               *swap.Htlc
	htlcExpiry         int32
	initiationHeight   int32
	fundingRounds      [len(staticFundingFeeRates)]*staticFundingRound
	sweepless          *staticSweeplessRound
	backupFinalized    bool
	workerStarted      bool
	abandoned          bool
	signingFailed      error
	paymentPreimage    lntypes.Preimage
	fundingTxHash      *[32]byte
	successSweepTxHash *[32]byte
}

// ServerNewAddress creates the server half of a static address. Repeating the
// call with the same client key is idempotent for the lifetime of this
// disposable regtest process.
func (s *Server) ServerNewAddress(ctx context.Context,
	req *swapserverrpc.ServerNewAddressRequest) (
	*swapserverrpc.ServerNewAddressResponse, error) {

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ProtocolVersion != swapserverrpc.StaticAddressProtocolVersion_V0 {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"unsupported static address protocol version %d",
			req.ProtocolVersion,
		)
	}
	clientKey, err := parseKey("static address client key", req.ClientKey)
	if err != nil {
		return nil, err
	}

	addressID := string(clientKey.SerializeCompressed())
	s.mu.RLock()
	existing := s.addresses[addressID]
	s.mu.RUnlock()
	if existing != nil {
		return existing.addressResponse(), nil
	}

	serverKey, err := s.deriveKey(ctx, swap.StaticAddressKeyFamily)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "derive static address server key: %v", err,
		)
	}
	contract, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(s.cfg.StaticAddressExpiry),
		clientKey, serverKey.pubKey,
	)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "create static address contract: %v", err,
		)
	}
	pkScript, err := contract.StaticAddressScript()
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "create static address script: %v", err,
		)
	}

	address := &staticAddress{
		clientKey: clientKey,
		serverKey: serverKey,
		expiry:    s.cfg.StaticAddressExpiry,
		contract:  contract,
		pkScript:  pkScript,
	}

	// Resolve a concurrent duplicate in favor of the first completed call.
	s.mu.Lock()
	if existing = s.addresses[addressID]; existing == nil {
		s.addresses[addressID] = address
		existing = address
	}
	s.mu.Unlock()

	return existing.addressResponse(), nil
}

func (a *staticAddress) addressResponse() *swapserverrpc.
	ServerNewAddressResponse {

	return &swapserverrpc.ServerNewAddressResponse{
		Params: &swapserverrpc.ServerAddressParameters{
			ServerKey: bytes.Clone(a.serverKey.pubKey.SerializeCompressed()),
			Expiry:    a.expiry,
		},
	}
}

// ServerStaticAddressLoopIn validates the selected deposits and creates three
// real server-side MuSig2 signing sessions for each deposit. No off-chain
// payment is attempted until PushStaticAddressHtlcSigs has produced complete,
// executable funding transactions.
func (s *Server) ServerStaticAddressLoopIn(ctx context.Context,
	req *swapserverrpc.ServerStaticAddressLoopInRequest) (
	*swapserverrpc.ServerStaticAddressLoopInResponse, error) {

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if req.ProtocolVersion != swapserverrpc.StaticAddressProtocolVersion_V0 {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"unsupported static address protocol version %d",
			req.ProtocolVersion,
		)
	}
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	existing := s.staticSwaps[hash]
	s.mu.RUnlock()
	if existing != nil {
		if !existing.matchesRequest(req) {
			return nil, status.Error(
				codes.AlreadyExists,
				"swap hash already exists with different parameters",
			)
		}

		return existing.initiationResponse(), nil
	}

	htlcClientKey, err := parseKey("HTLC client key", req.HtlcClientPubKey)
	if err != nil {
		return nil, err
	}
	if len(req.DepositOutpoints) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "at least one deposit is required",
		)
	}
	height, err := s.currentHeight(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get height: %v", err)
	}

	depositStrings := slices.Clone(req.DepositOutpoints)
	deposits := make([]wire.OutPoint, len(depositStrings))
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(depositStrings))
	seen := make(map[wire.OutPoint]struct{}, len(depositStrings))
	var (
		address *staticAddress
		total   btcutil.Amount
	)
	for i, outpointString := range depositStrings {
		outpoint, err := wire.NewOutPointFromString(outpointString)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument, "invalid deposit outpoint %q: %v",
				outpointString, err,
			)
		}
		if _, ok := seen[*outpoint]; ok {
			return nil, status.Errorf(
				codes.InvalidArgument, "duplicate deposit %v", outpoint,
			)
		}
		seen[*outpoint] = struct{}{}
		depositStrings[i] = outpoint.String()

		prevOut, confirmations, err := s.fetchStaticDeposit(*outpoint)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument, "fetch deposit %v: %v", outpoint,
				err,
			)
		}
		if err := validateStaticDepositPolicy(
			*outpoint, height, confirmations, s.cfg.StaticAddressExpiry,
		); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		depositAddress := s.addressForPkScript(prevOut.PkScript)
		if depositAddress == nil {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"deposit %v does not pay a registered static address",
				outpoint,
			)
		}
		if address != nil && address != depositAddress {
			return nil, status.Error(
				codes.InvalidArgument,
				"all deposits must use the same static address",
			)
		}
		address = depositAddress
		deposits[i] = *outpoint
		prevOuts[*outpoint] = prevOut
		if prevOut.Value < 0 || int64(total) > math.MaxInt64-prevOut.Value {
			return nil, status.Error(
				codes.InvalidArgument, "deposit total overflows int64",
			)
		}
		total += btcutil.Amount(prevOut.Value)
	}

	changePkScript, err := s.validateStaticDescriptors(req, address, seen)
	if err != nil {
		return nil, err
	}

	swapAmount := total
	if req.Amount != 0 {
		if req.Amount > math.MaxInt64 {
			return nil, status.Error(codes.InvalidArgument, "amount overflows")
		}
		swapAmount = btcutil.Amount(req.Amount)
	}
	if err := s.validateAmount(swapAmount); err != nil {
		return nil, err
	}
	if swapAmount > total {
		return nil, status.Error(
			codes.InvalidArgument, "swap amount exceeds selected deposits",
		)
	}
	changeAmount := total - swapAmount
	if changeAmount > 0 &&
		changeAmount < lnwallet.DustLimitForSize(input.P2TRSize) {

		return nil, status.Error(
			codes.InvalidArgument, "swap leaves a dust change output",
		)
	}
	if req.ChangeOutput != nil &&
		req.ChangeOutput.Amount != int64(changeAmount) {

		return nil, status.Errorf(
			codes.InvalidArgument,
			"change output amount %d does not match expected %d",
			req.ChangeOutput.Amount, changeAmount,
		)
	}

	expectedInvoiceAmount := swapAmount - s.swapFee(swapAmount)
	if _, err := s.validateInvoice(
		req.SwapInvoice, hash, expectedInvoiceAmount,
	); err != nil {
		return nil, err
	}

	var lastHop *route.Vertex
	if len(req.LastHop) != 0 {
		vertex, err := route.NewVertexFromBytes(req.LastHop)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument, "invalid last hop: %v", err,
			)
		}
		lastHop = &vertex
	}

	htlcServerKey, err := s.deriveKey(ctx, swap.StaticAddressKeyFamily)
	if err != nil {
		return nil, status.Errorf(
			codes.Internal, "derive HTLC server key: %v", err,
		)
	}
	htlcExpiry := height + staticLoopInCltvDelta
	htlc, err := swap.NewHtlcV2(
		htlcExpiry, keyBytes(htlcClientKey), keyBytes(htlcServerKey.pubKey),
		hash, s.cfg.Lnd.ChainParams,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create HTLC: %v", err)
	}

	paymentTimeout := s.cfg.PaymentTimeout
	if req.PaymentTimeoutSeconds != 0 {
		paymentTimeout = time.Duration(req.PaymentTimeoutSeconds) * time.Second
	}
	staticSwap := &staticLoopInSwap{
		hash:               hash,
		depositStrings:     depositStrings,
		deposits:           deposits,
		prevOuts:           prevOuts,
		address:            address,
		changePkScript:     changePkScript,
		changeDescriptor:   req.ChangeOutput != nil,
		totalDepositAmount: total,
		swapAmount:         swapAmount,
		requestedAmount:    req.Amount,
		invoice:            req.SwapInvoice,
		lastHop:            lastHop,
		lastHopBytes:       bytes.Clone(req.LastHop),
		paymentTimeout:     paymentTimeout,
		paymentTimeoutSecs: req.PaymentTimeoutSeconds,
		fast:               req.Fast,
		htlcClientKey:      htlcClientKey,
		htlcServerKey:      htlcServerKey,
		htlc:               htlc,
		htlcExpiry:         htlcExpiry,
		initiationHeight:   height,
	}

	for i, feeRate := range staticFundingFeeRates {
		round, err := s.newStaticFundingRound(ctx, staticSwap, feeRate)
		if err != nil {
			s.cleanupStaticSessions(context.WithoutCancel(ctx), staticSwap)
			return nil, status.Errorf(
				codes.Internal, "create funding signing round: %v", err,
			)
		}
		staticSwap.fundingRounds[i] = round
	}

	// Lock the selected UTXOs atomically with insertion. A concurrent
	// duplicate hash receives the first response; a different swap cannot
	// reserve an already selected deposit.
	s.mu.Lock()
	if existing = s.staticSwaps[hash]; existing != nil {
		s.mu.Unlock()
		s.cleanupStaticSessions(context.WithoutCancel(ctx), staticSwap)
		if !existing.matchesRequest(req) {
			return nil, status.Error(
				codes.AlreadyExists,
				"swap hash already exists with different parameters",
			)
		}

		return existing.initiationResponse(), nil
	}
	for _, outpointString := range depositStrings {
		if owner, ok := s.lockedUTXOs[outpointString]; ok {
			s.mu.Unlock()
			s.cleanupStaticSessions(context.WithoutCancel(ctx), staticSwap)
			return nil, status.Errorf(
				codes.Aborted, "deposit %s is locked by swap %v",
				outpointString, owner,
			)
		}
	}
	for _, outpointString := range depositStrings {
		s.lockedUTXOs[outpointString] = hash
	}
	s.staticSwaps[hash] = staticSwap
	s.mu.Unlock()

	return staticSwap.initiationResponse(), nil
}

func (s *Server) addressForPkScript(pkScript []byte) *staticAddress {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, address := range s.addresses {
		if bytes.Equal(address.pkScript, pkScript) {
			return address
		}
	}

	return nil
}

// fetchStaticDeposit returns an exact prevout plus Bitcoin Core's current
// confirmation count. Looking up the UTXO and then the raw transaction lets
// the server both reject spent deposits and avoid deriving satoshi values from
// the JSON-RPC floating-point amount.
func (s *Server) fetchStaticDeposit(outpoint wire.OutPoint) (*wire.TxOut,
	int64, error) {

	unspent, err := s.cfg.Bitcoin.GetTxOut(
		&outpoint.Hash, outpoint.Index, true,
	)
	if err != nil {
		return nil, 0, err
	}
	if unspent == nil {
		return nil, 0, fmt.Errorf("outpoint %v is spent or unknown", outpoint)
	}

	rawTx, err := s.cfg.Bitcoin.GetRawTransaction(&outpoint.Hash)
	if err != nil {
		return nil, 0, err
	}
	tx := rawTx.MsgTx()
	if int(outpoint.Index) >= len(tx.TxOut) {
		return nil, 0, fmt.Errorf(
			"outpoint %v has invalid output index", outpoint,
		)
	}
	prevOut := tx.TxOut[outpoint.Index]
	if unspent.ScriptPubKey.Hex != fmt.Sprintf("%x", prevOut.PkScript) {
		return nil, 0, fmt.Errorf("outpoint %v script mismatch", outpoint)
	}

	return &wire.TxOut{
		Value:    prevOut.Value,
		PkScript: bytes.Clone(prevOut.PkScript),
	}, unspent.Confirmations, nil
}

func validateStaticDepositPolicy(outpoint wire.OutPoint, currentHeight int32,
	confirmations int64, csvExpiry uint32) error {

	if confirmations < staticDepositMinConfirmations {
		return fmt.Errorf(
			"deposit %v is unconfirmed; at least %d confirmation is required",
			outpoint, staticDepositMinConfirmations,
		)
	}
	confirmationHeight := int64(currentHeight) - confirmations + 1
	if confirmationHeight <= 0 {
		return fmt.Errorf(
			"deposit %v has invalid confirmation height %d", outpoint,
			confirmationHeight,
		)
	}
	remainingLifetime := confirmationHeight + int64(csvExpiry) -
		int64(currentHeight)
	if remainingLifetime < staticDepositMinLifetime {
		return fmt.Errorf(
			"deposit %v has %d blocks of residual CSV lifetime; at least %d are required",
			outpoint, remainingLifetime, staticDepositMinLifetime,
		)
	}

	return nil
}

// revalidateStaticDeposits verifies the admission snapshot immediately before
// the server takes irreversible action. The script and value comparison also
// guards against a backend inconsistency returning a different output for the
// same outpoint.
func (s *Server) revalidateStaticDeposits(l *staticLoopInSwap) error {
	for _, outpoint := range l.deposits {
		current, _, err := s.fetchStaticDeposit(outpoint)
		if err != nil {
			return fmt.Errorf("revalidate deposit %v: %w", outpoint, err)
		}
		expected := l.prevOuts[outpoint]
		if expected == nil || current.Value != expected.Value ||
			!bytes.Equal(current.PkScript, expected.PkScript) {

			return fmt.Errorf(
				"revalidate deposit %v: prevout changed", outpoint,
			)
		}
	}

	return nil
}

func (s *Server) validateStaticDescriptors(req *swapserverrpc.
	ServerStaticAddressLoopInRequest, address *staticAddress,
	deposits map[wire.OutPoint]struct{}) ([]byte, error) {

	for outpoint, descriptor := range req.DepositToClientPubkeys {
		parsedOutpoint, err := wire.NewOutPointFromString(outpoint)
		if err != nil {
			return nil, status.Errorf(
				codes.InvalidArgument, "invalid descriptor outpoint %q: %v",
				outpoint, err,
			)
		}
		if _, ok := deposits[*parsedOutpoint]; !ok {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"descriptor outpoint %s is not a selected deposit", outpoint,
			)
		}
		if descriptor == nil {
			return nil, status.Errorf(
				codes.InvalidArgument, "nil descriptor for %s", outpoint,
			)
		}
		if !bytes.Equal(
			descriptor.Pubkey, address.clientKey.SerializeCompressed(),
		) || !bytes.Equal(descriptor.PkScript, address.pkScript) {

			return nil, status.Errorf(
				codes.InvalidArgument, "descriptor mismatch for %s", outpoint,
			)
		}
	}
	if req.ChangeOutput != nil {
		change := req.ChangeOutput
		if change.StaticAddress == nil {
			return nil, status.Error(
				codes.InvalidArgument, "change output descriptor mismatch",
			)
		}
		changeAddress := s.addressForPkScript(change.StaticAddress.PkScript)
		if changeAddress == nil || !bytes.Equal(
			change.StaticAddress.Pubkey,
			changeAddress.clientKey.SerializeCompressed(),
		) {

			return nil, status.Error(
				codes.InvalidArgument, "change output descriptor mismatch",
			)
		}

		return bytes.Clone(changeAddress.pkScript), nil
	}

	return bytes.Clone(address.pkScript), nil
}

func (l *staticLoopInSwap) matchesRequest(
	req *swapserverrpc.ServerStaticAddressLoopInRequest) bool {

	if req == nil {
		return false
	}
	if !bytes.Equal(req.SwapHash, l.hash[:]) ||
		!bytes.Equal(
			req.HtlcClientPubKey, l.htlcClientKey.SerializeCompressed(),
		) || req.SwapInvoice != l.invoice ||
		!bytes.Equal(req.LastHop, l.lastHopBytes) ||
		req.PaymentTimeoutSeconds != l.paymentTimeoutSecs ||
		req.Fast != l.fast ||
		(req.ChangeOutput != nil) != l.changeDescriptor {

		return false
	}
	if len(req.DepositOutpoints) != len(l.depositStrings) {
		return false
	}
	for i, serialized := range req.DepositOutpoints {
		outpoint, err := wire.NewOutPointFromString(serialized)
		if err != nil || outpoint.String() != l.depositStrings[i] {
			return false
		}
	}

	if req.Amount != l.requestedAmount {
		return false
	}
	if req.ChangeOutput != nil {
		descriptor := req.ChangeOutput.GetStaticAddress()
		if descriptor == nil || !bytes.Equal(
			descriptor.PkScript, l.changePkScript,
		) || req.ChangeOutput.Amount != int64(
			l.totalDepositAmount-l.swapAmount,
		) {

			return false
		}
	}

	return true
}

func (l *staticLoopInSwap) initiationResponse() *swapserverrpc.
	ServerStaticAddressLoopInResponse {

	infos := make([]*swapserverrpc.ServerHtlcSigningInfo, 0,
		len(l.fundingRounds))
	for _, round := range l.fundingRounds {
		nonces := make([][]byte, len(round.sessions))
		for i, session := range round.sessions {
			nonces[i] = bytes.Clone(session.PublicNonce[:])
		}
		infos = append(infos, &swapserverrpc.ServerHtlcSigningInfo{
			Nonces:  nonces,
			FeeRate: uint64(round.feeRate),
		})
	}

	return &swapserverrpc.ServerStaticAddressLoopInResponse{
		HtlcServerPubKey: bytes.Clone(
			l.htlcServerKey.pubKey.SerializeCompressed(),
		),
		HtlcExpiry:         l.htlcExpiry,
		StandardHtlcInfo:   infos[0],
		HighFeeHtlcInfo:    infos[1],
		ExtremeFeeHtlcInfo: infos[2],
	}
}

func (s *Server) newStaticFundingRound(ctx context.Context,
	l *staticLoopInSwap, feeRate chainfee.SatPerKWeight) (
	*staticFundingRound, error) {

	tx, err := createStaticFundingTx(l, feeRate)
	if err != nil {
		return nil, err
	}

	round := &staticFundingRound{
		feeRate:  feeRate,
		tx:       tx,
		sessions: make([]*input.MuSig2SessionInfo, len(l.deposits)),
	}
	signers := [][]byte{
		l.address.clientKey.SerializeCompressed(),
		l.address.serverKey.pubKey.SerializeCompressed(),
	}
	rootHash := l.address.contract.RootHash
	for i := range l.deposits {
		session, err := s.cfg.Lnd.Signer.MuSig2CreateSession(
			ctx, input.MuSig2Version100RC2,
			&l.address.serverKey.locator, signers,
			lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
		)
		if err != nil {
			for _, created := range round.sessions {
				if created != nil {
					_ = s.cfg.Lnd.Signer.MuSig2Cleanup(
						context.WithoutCancel(ctx), created.SessionID,
					)
				}
			}

			return nil, err
		}
		round.sessions[i] = session
	}

	return round, nil
}

func createStaticFundingTx(l *staticLoopInSwap,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	tx := wire.NewMsgTx(2)
	for _, outpoint := range l.deposits {
		// Keep this literal in sync with the client. In particular, its
		// zero sequence is part of the signed transaction.
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	}

	changeAmount := l.totalDepositAmount - l.swapAmount
	var weight input.TxWeightEstimator
	for range l.deposits {
		weight.AddTaprootKeySpendInput(txscript.SigHashDefault)
	}
	weight.AddP2WSHOutput()
	if changeAmount > 0 {
		weight.AddP2TROutput()
	}
	fee := feeRate.FeeForWeight(weight.Weight())
	if fee <= 0 || fee >= l.swapAmount {
		return nil, fmt.Errorf("invalid funding fee %d", fee)
	}

	htlcValue := l.swapAmount - fee
	if htlcValue < lnwallet.DustLimitForSize(input.P2WSHSize) {
		return nil, fmt.Errorf("HTLC output is dust: %d", htlcValue)
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(htlcValue),
		PkScript: bytes.Clone(l.htlc.PkScript),
	})
	if changeAmount > 0 {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(changeAmount),
			PkScript: bytes.Clone(l.changePkScript),
		})
	}

	return tx, nil
}

// PushStaticAddressHtlcSigs completes all three server MuSig2 signing rounds.
// A duplicate call after successful finalization is idempotent. An otherwise
// empty request is the protocol's abandonment signal.
func (s *Server) PushStaticAddressHtlcSigs(ctx context.Context,
	req *swapserverrpc.PushStaticAddressHtlcSigsRequest) (
	*swapserverrpc.PushStaticAddressHtlcSigsResponse, error) {

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	staticSwap := s.staticSwaps[hash]
	s.mu.RUnlock()
	if staticSwap == nil {
		return nil, status.Error(codes.NotFound, "static swap not found")
	}

	staticSwap.mu.Lock()
	defer staticSwap.mu.Unlock()

	if staticSwap.backupFinalized {
		return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
	}
	infos := []*swapserverrpc.ClientHtlcSigningInfo{
		req.StandardHtlcInfo,
		req.HighFeeHtlcInfo,
		req.ExtremeFeeHtlcInfo,
	}
	if staticSwap.abandoned {
		if emptyStaticFundingInfos(infos) {
			return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
		}

		return nil, status.Error(codes.FailedPrecondition, "swap abandoned")
	}
	if staticSwap.signingFailed != nil {
		return nil, status.Errorf(
			codes.FailedPrecondition, "previous signing failed: %v",
			staticSwap.signingFailed,
		)
	}

	if emptyStaticFundingInfos(infos) {
		staticSwap.abandoned = true
		s.cleanupStaticSessions(context.WithoutCancel(ctx), staticSwap)
		s.releaseStaticLocks(staticSwap)

		return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
	}

	for i, info := range infos {
		finalTx, err := s.finalizeStaticFundingRound(
			ctx, staticSwap, staticSwap.fundingRounds[i], info,
		)
		if err != nil {
			staticSwap.signingFailed = err
			s.cleanupStaticSessions(context.WithoutCancel(ctx), staticSwap)
			s.releaseStaticLocks(staticSwap)

			return nil, status.Errorf(
				codes.InvalidArgument,
				"finalize funding signatures at fee tier %d: %v", i, err,
			)
		}
		staticSwap.fundingRounds[i].finalTx = finalTx
	}

	staticSwap.backupFinalized = true
	if !staticSwap.workerStarted {
		staticSwap.workerStarted = true
		s.goSwap(func(runCtx context.Context) {
			s.runStaticLoopIn(runCtx, staticSwap)
		})
	}

	return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
}

func emptyStaticFundingInfos(
	infos []*swapserverrpc.ClientHtlcSigningInfo) bool {

	for _, info := range infos {
		if info != nil && (len(info.Nonces) != 0 || len(info.Sigs) != 0) {
			return false
		}
	}

	return true
}

func (s *Server) finalizeStaticFundingRound(ctx context.Context,
	l *staticLoopInSwap, round *staticFundingRound,
	info *swapserverrpc.ClientHtlcSigningInfo) (*wire.MsgTx, error) {

	if info == nil {
		return nil, errors.New("missing signing info")
	}
	if len(info.Nonces) != len(l.deposits) ||
		len(info.Sigs) != len(l.deposits) {

		return nil, fmt.Errorf(
			"got %d nonces and %d signatures for %d deposits",
			len(info.Nonces), len(info.Sigs), len(l.deposits),
		)
	}

	tx := round.tx.Copy()
	prevFetcher := txscript.NewMultiPrevOutFetcher(l.prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	for i := range l.deposits {
		if len(info.Nonces[i]) != musig2.PubNonceSize {
			return nil, fmt.Errorf("nonce %d must be %d bytes", i,
				musig2.PubNonceSize)
		}
		if len(info.Sigs[i]) != input.MuSig2PartialSigSize {
			return nil, fmt.Errorf("partial signature %d must be %d bytes",
				i, input.MuSig2PartialSigSize)
		}

		var clientNonce [musig2.PubNonceSize]byte
		copy(clientNonce[:], info.Nonces[i])
		haveAllNonces, err := s.cfg.Lnd.Signer.MuSig2RegisterNonces(
			ctx, round.sessions[i].SessionID,
			[][musig2.PubNonceSize]byte{clientNonce},
		)
		if err != nil {
			return nil, err
		}
		if !haveAllNonces {
			return nil, errors.New("MuSig2 session is missing nonces")
		}

		digestBytes, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, tx, i, prevFetcher,
		)
		if err != nil {
			return nil, err
		}
		var digest [32]byte
		copy(digest[:], digestBytes)

		if _, err := s.cfg.Lnd.Signer.MuSig2Sign(
			ctx, round.sessions[i].SessionID, digest, false,
		); err != nil {
			return nil, err
		}
		haveAllSigs, finalSig, err :=
			s.cfg.Lnd.Signer.MuSig2CombineSig(
				ctx, round.sessions[i].SessionID,
				[][]byte{info.Sigs[i]},
			)
		if err != nil {
			return nil, err
		}
		if !haveAllSigs || len(finalSig) != 64 {
			return nil, errors.New("MuSig2 signature did not finalize")
		}
		tx.TxIn[i].Witness = wire.TxWitness{finalSig}
	}

	if err := validateStaticFundingTx(tx, l.prevOuts); err != nil {
		return nil, fmt.Errorf("funding transaction validation failed: %w", err)
	}

	return tx, nil
}

func validateStaticFundingTx(tx *wire.MsgTx,
	prevOuts map[wire.OutPoint]*wire.TxOut) error {

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	for i, txIn := range tx.TxIn {
		prevOut := prevOuts[txIn.PreviousOutPoint]
		if prevOut == nil {
			return fmt.Errorf("missing prevout for input %d", i)
		}
		vm, err := txscript.NewEngine(
			prevOut.PkScript, tx, i, txscript.StandardVerifyFlags,
			nil, sigHashes, prevOut.Value, prevFetcher,
		)
		if err != nil {
			return err
		}
		if err := vm.Execute(); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) cleanupStaticSessions(ctx context.Context,
	l *staticLoopInSwap) {

	for _, round := range l.fundingRounds {
		if round == nil || round.finalTx != nil {
			continue
		}
		for _, session := range round.sessions {
			if session != nil {
				_ = s.cfg.Lnd.Signer.MuSig2Cleanup(ctx, session.SessionID)
			}
		}
	}
}

func (s *Server) releaseStaticLocks(l *staticLoopInSwap) {
	s.mu.Lock()
	for _, outpoint := range l.depositStrings {
		if s.lockedUTXOs[outpoint] == l.hash {
			delete(s.lockedUTXOs, outpoint)
		}
	}
	s.mu.Unlock()
}

func (s *Server) publishStaticRiskAccepted(hash lntypes.Hash) {
	s.notifications.publish(&swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.
			SubscribeNotificationsResponse_StaticLoopInRiskAccepted{
			StaticLoopInRiskAccepted: &swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification{
				SwapHash: bytes.Clone(hash[:]),
			},
		},
	})
}

func (s *Server) publishStaticRiskRejected(hash lntypes.Hash) {
	s.notifications.publish(&swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.
			SubscribeNotificationsResponse_StaticLoopInRiskRejected{
			StaticLoopInRiskRejected: &swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification{
				SwapHash: bytes.Clone(hash[:]),
			},
		},
	})
}

func (s *Server) newStaticSweeplessRound(ctx context.Context,
	l *staticLoopInSwap) (*staticSweeplessRound, error) {

	sweepAddress, err := s.cfg.Lnd.WalletKit.NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return nil, fmt.Errorf("derive sweepless destination: %w", err)
	}
	sweepPkScript, err := txscript.PayToAddrScript(sweepAddress)
	if err != nil {
		return nil, fmt.Errorf("create sweepless destination script: %w", err)
	}
	feeRate, err := s.cfg.Lnd.WalletKit.EstimateFeeRate(
		ctx, staticSweepConfTarget,
	)
	if err != nil {
		return nil, fmt.Errorf("estimate sweepless fee: %w", err)
	}
	if feeRate < staticStandardFeeRate {
		feeRate = staticStandardFeeRate
	}

	changeAmount := l.totalDepositAmount - l.swapAmount
	var weight input.TxWeightEstimator
	for range l.deposits {
		weight.AddTaprootKeySpendInput(txscript.SigHashDefault)
	}
	weight.AddP2TROutput()
	if changeAmount > 0 {
		weight.AddP2TROutput()
	}
	fee := feeRate.FeeForWeight(weight.Weight())
	serverAmount := l.swapAmount - fee
	if fee <= 0 || serverAmount <= 0 ||
		serverAmount < lnwallet.DustLimitForSize(input.P2TRSize) {

		return nil, fmt.Errorf(
			"invalid sweepless fee/output: fee=%d output=%d", fee,
			serverAmount,
		)
	}

	tx := wire.NewMsgTx(2)
	for _, outpoint := range l.deposits {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(serverAmount),
		PkScript: sweepPkScript,
	})
	if changeAmount > 0 {
		tx.AddTxOut(&wire.TxOut{
			Value:    int64(changeAmount),
			PkScript: bytes.Clone(l.changePkScript),
		})
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("create sweepless PSBT: %w", err)
	}
	for i, outpoint := range l.deposits {
		prevOut := l.prevOuts[outpoint]
		if prevOut == nil {
			return nil, fmt.Errorf("missing prevout for %v", outpoint)
		}
		packet.Inputs[i].WitnessUtxo = &wire.TxOut{
			Value:    prevOut.Value,
			PkScript: bytes.Clone(prevOut.PkScript),
		}
	}
	var serialized bytes.Buffer
	if err := packet.Serialize(&serialized); err != nil {
		return nil, fmt.Errorf("serialize sweepless PSBT: %w", err)
	}

	round := &staticSweeplessRound{
		tx:       tx,
		psbt:     serialized.Bytes(),
		sessions: make(map[string]*input.MuSig2SessionInfo, len(l.deposits)),
		result:   make(chan error, 1),
	}
	signers := [][]byte{
		l.address.clientKey.SerializeCompressed(),
		l.address.serverKey.pubKey.SerializeCompressed(),
	}
	rootHash := l.address.contract.RootHash
	for _, outpoint := range l.deposits {
		session, err := s.cfg.Lnd.Signer.MuSig2CreateSession(
			ctx, input.MuSig2Version100RC2,
			&l.address.serverKey.locator, signers,
			lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
		)
		if err != nil {
			s.cleanupStaticSweeplessSessions(
				context.WithoutCancel(ctx), round,
			)

			return nil, fmt.Errorf("create sweepless session: %w", err)
		}
		round.sessions[outpoint.String()] = session
	}

	return round, nil
}

func (s *Server) cleanupStaticSweeplessSessions(ctx context.Context,
	round *staticSweeplessRound) {

	if round == nil {
		return
	}
	for _, session := range round.sessions {
		_ = s.cfg.Lnd.Signer.MuSig2Cleanup(ctx, session.SessionID)
	}
}

func (s *Server) publishStaticSweeplessRequest(l *staticLoopInSwap,
	round *staticSweeplessRound) {

	nonces := make(map[string][]byte, len(round.sessions))
	for outpoint, session := range round.sessions {
		nonces[outpoint] = bytes.Clone(session.PublicNonce[:])
	}
	prevOuts := make([]*swapserverrpc.PrevoutInfo, 0, len(l.deposits))
	for _, outpoint := range l.deposits {
		prevOut := l.prevOuts[outpoint]
		prevOuts = append(prevOuts, &swapserverrpc.PrevoutInfo{
			TxidBytes:   bytes.Clone(outpoint.Hash[:]),
			OutputIndex: outpoint.Index,
			Value:       uint64(prevOut.Value),
			PkScript:    bytes.Clone(prevOut.PkScript),
		})
	}

	s.notifications.publish(&swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.
			SubscribeNotificationsResponse_StaticLoopInSweep{
			StaticLoopInSweep: &swapserverrpc.
				ServerStaticLoopInSweepNotification{
				SweepTxPsbt:     bytes.Clone(round.psbt),
				SwapHash:        bytes.Clone(l.hash[:]),
				DepositToNonces: nonces,
				PrevoutInfo:     prevOuts,
			},
		},
	})
}

// tryStaticSweepless asks the now-paid client to co-sign a direct spend of the
// original deposits. Notifications are retried because the client's invoice
// subscription and its manager-level notification stream advance
// independently. Any timeout or signing/publication failure falls through to
// the fully signed HTLC path.
func (s *Server) tryStaticSweepless(ctx context.Context,
	l *staticLoopInSwap) bool {

	if err := s.revalidateStaticDeposits(l); err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v direct sweep preflight failed: %v",
			l.hash, err,
		)

		return false
	}

	round, err := s.newStaticSweeplessRound(ctx, l)
	if err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v direct sweep setup failed: %v", l.hash, err,
		)

		return false
	}

	l.mu.Lock()
	l.sweepless = round
	l.mu.Unlock()

	timeout := time.NewTimer(staticSweeplessWait)
	defer timeout.Stop()
	retry := time.NewTicker(staticSweeplessRetry)
	defer retry.Stop()
	s.publishStaticSweeplessRequest(l, round)

	for {
		select {
		case err := <-round.result:
			if err != nil {
				s.cfg.Logger.Printf(
					"static Loop In %v direct signing failed: %v",
					l.hash, err,
				)
				s.cleanupStaticSweeplessSessions(
					context.WithoutCancel(ctx), round,
				)

				return false
			}

			l.mu.Lock()
			finalTx := round.finalTx
			l.mu.Unlock()
			if finalTx == nil {
				return false
			}
			if err := s.revalidateStaticDeposits(l); err != nil {
				l.mu.Lock()
				round.closed = true
				l.mu.Unlock()
				s.cfg.Logger.Printf(
					"static Loop In %v direct settlement preflight "+
						"failed: %v", l.hash, err,
				)

				return false
			}
			err = s.cfg.Lnd.WalletKit.PublishTransaction(
				ctx, finalTx,
				fmt.Sprintf(
					"regtest-static-loop-in-direct-%x", l.hash[:6],
				),
			)
			if err != nil && !strings.Contains(err.Error(), "already") {
				s.cfg.Logger.Printf(
					"static Loop In %v direct publication failed: %v",
					l.hash, err,
				)

				return false
			}

			txHash := finalTx.TxHash()
			l.mu.Lock()
			var serializedHash [32]byte
			copy(serializedHash[:], txHash[:])
			l.successSweepTxHash = &serializedHash
			round.closed = true
			l.mu.Unlock()
			s.releaseStaticLocks(l)
			s.cfg.Logger.Printf(
				"static Loop In %v direct settlement complete: sweep=%v",
				l.hash, txHash,
			)

			return true

		case <-retry.C:
			s.publishStaticSweeplessRequest(l, round)

		case <-timeout.C:
			l.mu.Lock()
			round.closed = true
			l.mu.Unlock()
			s.cleanupStaticSweeplessSessions(
				context.WithoutCancel(ctx), round,
			)
			s.cfg.Logger.Printf(
				"static Loop In %v direct signing timed out; using HTLC",
				l.hash,
			)

			return false

		case <-ctx.Done():
			l.mu.Lock()
			round.closed = true
			l.mu.Unlock()
			s.cleanupStaticSweeplessSessions(
				context.WithoutCancel(ctx), round,
			)

			return false
		}
	}
}

// runStaticLoopIn pays only after fully signed funding safety transactions
// exist. It first attempts the cooperative direct spend. If the client cannot
// co-sign it, the server publishes one funding transaction, waits for its
// relative-lock prerequisite and claims the HTLC with the payment preimage.
func (s *Server) runStaticLoopIn(ctx context.Context,
	l *staticLoopInSwap) {

	// The funding signatures can arrive well after initiation. Check the
	// selected deposits again before announcing risk acceptance or paying the
	// invoice so a conflicting spend never turns into an off-chain loss.
	if err := s.revalidateStaticDeposits(l); err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v deposit preflight failed: %v", l.hash, err,
		)
		s.publishStaticRiskRejected(l.hash)
		s.releaseStaticLocks(l)

		return
	}

	s.publishStaticRiskAccepted(l.hash)
	payment, err := s.sendStaticPayment(
		ctx, l.invoice, l.paymentTimeout, l.lastHop,
	)
	if err != nil {
		s.cfg.Logger.Printf("static Loop In %v payment failed: %v", l.hash, err)
		s.publishStaticRiskRejected(l.hash)
		s.releaseStaticLocks(l)

		return
	}
	if payment.Preimage.Hash() != l.hash {
		s.cfg.Logger.Printf(
			"static Loop In %v payment returned wrong preimage", l.hash,
		)
		s.publishStaticRiskRejected(l.hash)

		return
	}

	l.mu.Lock()
	l.paymentPreimage = payment.Preimage
	l.mu.Unlock()
	if s.tryStaticSweepless(ctx, l) {
		return
	}
	if err := s.revalidateStaticDeposits(l); err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v fallback funding preflight failed: %v",
			l.hash, err,
		)
		s.releaseStaticLocks(l)

		return
	}

	fundingTx, err := s.publishStaticFundingTx(ctx, l)
	if err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v funding publication failed after payment: %v",
			l.hash, err,
		)
		return
	}
	fundingHash := fundingTx.TxHash()
	l.mu.Lock()
	var serializedFundingHash [32]byte
	copy(serializedFundingHash[:], fundingHash[:])
	l.fundingTxHash = &serializedFundingHash
	l.mu.Unlock()

	confChan, errChan, err := s.cfg.Lnd.ChainNotifier.
		RegisterConfirmationsNtfn(
			ctx, &fundingHash, l.htlc.PkScript, 1, l.initiationHeight,
		)
	if err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v funding confirmation registration failed: %v",
			l.hash, err,
		)
		return
	}

	confirmed := false
	for !confirmed && (confChan != nil || errChan != nil) {
		select {
		case _, ok := <-confChan:
			if !ok {
				confChan = nil
				continue
			}
			confirmed = true

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				s.cfg.Logger.Printf(
					"static Loop In %v funding confirmation failed: %v",
					l.hash, err,
				)

				return
			}

		case <-ctx.Done():
			return
		}
	}
	if !confirmed {
		s.cfg.Logger.Printf(
			"static Loop In %v funding confirmation stream closed", l.hash,
		)

		return
	}

	sweepTx, err := s.createStaticSuccessSweep(ctx, l, fundingTx)
	if err != nil {
		s.cfg.Logger.Printf(
			"static Loop In %v success sweep creation failed: %v", l.hash, err,
		)
		return
	}
	if err := s.cfg.Lnd.WalletKit.PublishTransaction(
		ctx, sweepTx, fmt.Sprintf("regtest-static-loop-in-success-%x", l.hash[:6]),
	); err != nil && !strings.Contains(err.Error(), "already") {
		s.cfg.Logger.Printf(
			"static Loop In %v success sweep publication failed: %v", l.hash,
			err,
		)
		return
	}

	sweepHash := sweepTx.TxHash()
	l.mu.Lock()
	var serializedSweepHash [32]byte
	copy(serializedSweepHash[:], sweepHash[:])
	l.successSweepTxHash = &serializedSweepHash
	l.mu.Unlock()
	s.releaseStaticLocks(l)
	s.cfg.Logger.Printf(
		"static Loop In %v fallback complete: funding=%v sweep=%v",
		l.hash, fundingHash, sweepHash,
	)
}

func (s *Server) sendStaticPayment(ctx context.Context, invoice string,
	timeout time.Duration, lastHop *route.Vertex) (lndclient.PaymentStatus,
	error) {

	statusChan, errChan, err := s.cfg.Lnd.Router.SendPayment(
		ctx, lndclient.SendPaymentRequest{
			Invoice:       invoice,
			MaxFee:        s.cfg.MaxSwapAmount,
			Timeout:       timeout,
			LastHopPubkey: lastHop,
			MaxParts:      10,
			Cancelable:    true,
		},
	)
	if err != nil {
		return lndclient.PaymentStatus{}, err
	}

	for statusChan != nil || errChan != nil {
		select {
		case payment, ok := <-statusChan:
			if !ok {
				statusChan = nil
				continue
			}
			switch payment.State {
			case lnrpc.Payment_SUCCEEDED:
				return payment, nil

			case lnrpc.Payment_FAILED:
				return payment, fmt.Errorf(
					"payment failed: %v", payment.FailureReason,
				)
			}

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				return lndclient.PaymentStatus{}, err
			}

		case <-ctx.Done():
			return lndclient.PaymentStatus{}, ctx.Err()
		}
	}

	return lndclient.PaymentStatus{}, errors.New("payment stream closed")
}

func (s *Server) publishStaticFundingTx(ctx context.Context,
	l *staticLoopInSwap) (*wire.MsgTx, error) {

	var publicationErrors []error
	for i, round := range l.fundingRounds {
		if round == nil || round.finalTx == nil {
			continue
		}
		err := s.cfg.Lnd.WalletKit.PublishTransaction(
			ctx, round.finalTx,
			fmt.Sprintf("regtest-static-loop-in-funding-%x-%d", l.hash[:6], i),
		)
		if err == nil || strings.Contains(err.Error(), "already") {
			return round.finalTx, nil
		}
		publicationErrors = append(publicationErrors, err)
	}
	if len(publicationErrors) == 0 {
		return nil, errors.New("no finalized static funding transaction")
	}

	return nil, errors.Join(publicationErrors...)
}

func (s *Server) createStaticSuccessSweep(ctx context.Context,
	l *staticLoopInSwap, fundingTx *wire.MsgTx) (*wire.MsgTx, error) {

	if len(fundingTx.TxOut) == 0 {
		return nil, errors.New("funding transaction has no HTLC output")
	}
	sweepAddress, err := s.cfg.Lnd.WalletKit.NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return nil, err
	}
	sweepPkScript, err := txscript.PayToAddrScript(sweepAddress)
	if err != nil {
		return nil, err
	}
	feeRate, err := s.cfg.Lnd.WalletKit.EstimateFeeRate(
		ctx, staticSweepConfTarget,
	)
	if err != nil {
		return nil, err
	}
	if feeRate < staticStandardFeeRate {
		feeRate = staticStandardFeeRate
	}
	var weight input.TxWeightEstimator
	if err := l.htlc.AddSuccessToEstimator(&weight); err != nil {
		return nil, err
	}
	weight.AddP2TROutput()
	fee := feeRate.FeeForWeight(weight.Weight())
	outputValue := btcutil.Amount(fundingTx.TxOut[0].Value) - fee
	if outputValue < lnwallet.DustLimitForSize(input.P2TRSize) {
		return nil, fmt.Errorf("success sweep output is dust: %d", outputValue)
	}

	fundingHash := fundingTx.TxHash()
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: fundingHash, Index: 0},
		SignatureScript:  bytes.Clone(l.htlc.SigScript),
		Sequence:         l.htlc.SuccessSequence(),
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(outputValue),
		PkScript: sweepPkScript,
	})

	signDesc := &lndclient.SignDescriptor{
		WitnessScript: l.htlc.SuccessScript(),
		Output:        fundingTx.TxOut[0],
		HashType:      l.htlc.SigHash(),
		InputIndex:    0,
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: l.htlcServerKey.locator,
			PubKey:     l.htlcServerKey.pubKey,
		},
		SignMethod: input.WitnessV0SignMethod,
	}
	rawSigs, err := s.cfg.Lnd.Signer.SignOutputRawKeyLocator(
		ctx, sweepTx, []*lndclient.SignDescriptor{signDesc},
		[]*wire.TxOut{fundingTx.TxOut[0]},
	)
	if err != nil {
		return nil, err
	}
	if len(rawSigs) != 1 {
		return nil, fmt.Errorf("expected one HTLC signature, got %d",
			len(rawSigs))
	}
	sweepTx.TxIn[0].Witness, err = l.htlc.GenSuccessWitness(
		rawSigs[0], l.paymentPreimage,
	)
	if err != nil {
		return nil, err
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		fundingTx.TxOut[0].PkScript, fundingTx.TxOut[0].Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)
	vm, err := txscript.NewEngine(
		fundingTx.TxOut[0].PkScript, sweepTx, 0,
		txscript.StandardVerifyFlags, nil, sigHashes,
		fundingTx.TxOut[0].Value, prevFetcher,
	)
	if err != nil {
		return nil, err
	}
	if err := vm.Execute(); err != nil {
		return nil, fmt.Errorf("success sweep validation failed: %w", err)
	}

	return sweepTx, nil
}

// PushStaticAddressSweeplessSigs finalizes the preferred direct spend of the
// deposits. The transaction id, exact outpoint set and all resulting Taproot
// witnesses are validated before the worker is allowed to publish it.
func (s *Server) PushStaticAddressSweeplessSigs(ctx context.Context,
	req *swapserverrpc.PushStaticAddressSweeplessSigsRequest) (
	*swapserverrpc.PushStaticAddressSweeplessSigsResponse, error) {

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	staticSwap := s.staticSwaps[hash]
	s.mu.RUnlock()
	if staticSwap == nil {
		return nil, status.Error(codes.NotFound, "static swap not found")
	}

	staticSwap.mu.Lock()
	defer staticSwap.mu.Unlock()

	round := staticSwap.sweepless
	if round == nil {
		return nil, status.Error(
			codes.FailedPrecondition, "sweepless signing was not requested",
		)
	}
	if err := validateStaticSweeplessTxID(req.Txid, round.tx); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if round.finalTx != nil {
		return &swapserverrpc.PushStaticAddressSweeplessSigsResponse{}, nil
	}
	if round.closed {
		return nil, status.Error(
			codes.FailedPrecondition, "sweepless signing round is closed",
		)
	}
	if req.ErrorMessage != "" {
		if len(req.SigningInfo) != 0 {
			return nil, status.Error(
				codes.InvalidArgument,
				"error acknowledgement must not contain signatures",
			)
		}

		// "not finished" is expected while the client processes its
		// invoice settlement. The worker will retry the notification.
		return &swapserverrpc.PushStaticAddressSweeplessSigsResponse{}, nil
	}
	if len(req.SigningInfo) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "sweepless signatures are required",
		)
	}

	finalTx, err := s.finalizeStaticSweeplessRound(
		ctx, staticSwap, round, req.SigningInfo,
	)
	if err != nil {
		round.closed = true
		s.cleanupStaticSweeplessSessions(context.WithoutCancel(ctx), round)
		select {
		case round.result <- err:
		default:
		}

		return nil, status.Errorf(
			codes.InvalidArgument, "finalize sweepless signatures: %v", err,
		)
	}
	round.finalTx = finalTx
	select {
	case round.result <- nil:
	default:
	}

	return &swapserverrpc.PushStaticAddressSweeplessSigsResponse{}, nil
}

func validateStaticSweeplessTxID(serialized []byte, tx *wire.MsgTx) error {
	if len(serialized) != 32 {
		return errors.New("sweepless txid must be 32 bytes")
	}
	want := tx.TxHash()
	if !bytes.Equal(serialized, want[:]) {
		return errors.New("sweepless txid mismatch")
	}

	return nil
}

func (s *Server) finalizeStaticSweeplessRound(ctx context.Context,
	l *staticLoopInSwap, round *staticSweeplessRound,
	infos map[string]*swapserverrpc.ClientSweeplessSigningInfo) (
	*wire.MsgTx, error) {

	if len(infos) != len(l.deposits) {
		return nil, fmt.Errorf(
			"got signatures for %d deposits, expected %d", len(infos),
			len(l.deposits),
		)
	}
	for outpoint := range infos {
		if _, ok := round.sessions[outpoint]; !ok {
			return nil, fmt.Errorf(
				"signature supplied for unknown deposit %s", outpoint,
			)
		}
	}

	tx := round.tx.Copy()
	prevFetcher := txscript.NewMultiPrevOutFetcher(l.prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	for i, outpoint := range l.deposits {
		outpointString := outpoint.String()
		info := infos[outpointString]
		if info == nil {
			return nil, fmt.Errorf(
				"missing signing info for %s", outpointString,
			)
		}
		if len(info.Nonce) != musig2.PubNonceSize {
			return nil, fmt.Errorf(
				"nonce for %s must be %d bytes", outpointString,
				musig2.PubNonceSize,
			)
		}
		if len(info.Sig) != input.MuSig2PartialSigSize {
			return nil, fmt.Errorf(
				"partial signature for %s must be %d bytes", outpointString,
				input.MuSig2PartialSigSize,
			)
		}

		var clientNonce [musig2.PubNonceSize]byte
		copy(clientNonce[:], info.Nonce)
		session := round.sessions[outpointString]
		haveAllNonces, err := s.cfg.Lnd.Signer.MuSig2RegisterNonces(
			ctx, session.SessionID,
			[][musig2.PubNonceSize]byte{clientNonce},
		)
		if err != nil {
			return nil, err
		}
		if !haveAllNonces {
			return nil, fmt.Errorf(
				"MuSig2 session for %s is missing nonces", outpointString,
			)
		}

		digestBytes, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, tx, i, prevFetcher,
		)
		if err != nil {
			return nil, err
		}
		var digest [32]byte
		copy(digest[:], digestBytes)
		if _, err := s.cfg.Lnd.Signer.MuSig2Sign(
			ctx, session.SessionID, digest, false,
		); err != nil {
			return nil, err
		}
		haveAllSigs, finalSig, err := s.cfg.Lnd.Signer.MuSig2CombineSig(
			ctx, session.SessionID, [][]byte{info.Sig},
		)
		if err != nil {
			return nil, err
		}
		if !haveAllSigs || len(finalSig) != 64 {
			return nil, fmt.Errorf(
				"MuSig2 signature for %s did not finalize", outpointString,
			)
		}
		tx.TxIn[i].Witness = wire.TxWitness{finalSig}
	}

	if err := validateStaticFundingTx(tx, l.prevOuts); err != nil {
		return nil, fmt.Errorf("sweepless transaction validation failed: %w",
			err)
	}

	return tx, nil
}
