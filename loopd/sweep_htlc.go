package loopd

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultStatelessRecoveryKeyScanLimit is the maximum number of client keys
// scanned during stateless recovery.
const defaultStatelessRecoveryKeyScanLimit = 20_000

// statelessRecoveryKeyScanLogInterval controls how often a key scan reports
// progress.
const statelessRecoveryKeyScanLogInterval = 2_000

// loopOutStore abstracts the minimal store API needed to look up loop-out
// swaps.
type loopOutStore interface {
	// FetchLoopOutSwaps returns all loop-out swaps currently in the store.
	FetchLoopOutSwaps(ctx context.Context) ([]*loopdb.LoopOut, error)
}

// htlcChainNotifier defines the minimal notifier API to watch for a tx
// confirmation.
type htlcChainNotifier interface {
	RegisterConfirmationsNtfn(ctx context.Context, txid *chainhash.Hash,
		pkScript []byte, numConfs, heightHint int32,
		opts ...lndclient.NotifierOption) (
		chan *chainntnfs.TxConfirmation, chan error, error)
}

// htlcWallet abstracts the wallet calls used for sweeping.
type htlcWallet interface {
	// DeriveKey derives the key identified by the given locator.
	DeriveKey(ctx context.Context, locator *keychain.KeyLocator) (
		*keychain.KeyDescriptor, error)

	// NextAddr derives the next address from the given account and type.
	NextAddr(ctx context.Context, account string,
		addrType walletrpc.AddressType,
		change bool) (btcutil.Address, error)

	// PublishTransaction broadcasts the transaction with the given label.
	PublishTransaction(ctx context.Context, tx *wire.MsgTx,
		label string) error

	// MinRelayFee returns the current minimum relay fee in sat/kw.
	MinRelayFee(ctx context.Context) (chainfee.SatPerKWeight, error)
}

// htlcSigner signs the success path spend.
type htlcSigner interface {
	SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
		signDescriptors []*lndclient.SignDescriptor,
		prevOutputs []*wire.TxOut) ([][]byte, error)

	SignOutputRawKeyLocator(ctx context.Context, tx *wire.MsgTx,
		signDescriptors []*lndclient.SignDescriptor,
		prevOutputs []*wire.TxOut) ([][]byte, error)
}

// sweepHtlc spends a Loop HTLC output using the success path and a known
// preimage.
func sweepHtlc(ctx context.Context, req *looprpc.SweepHtlcRequest,
	chainParams *chaincfg.Params, store loopOutStore,
	notifier htlcChainNotifier, wallet htlcWallet,
	signer htlcSigner) (*looprpc.SweepHtlcResponse, error) {

	// Make sure that the request has all required inputs.
	if req.Outpoint == "" {
		return nil, status.Error(codes.InvalidArgument,
			"outpoint required")
	}
	if req.HtlcAddress == "" {
		return nil, status.Error(codes.InvalidArgument,
			"htlc_address required")
	}
	if req.SatPerVbyte == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"sat_per_vbyte required")
	}

	recovery := req.StatelessRecovery
	stateless := recovery != nil
	if stateless {
		if len(recovery.ServerPubkey) == 0 ||
			len(recovery.ClientPubkey) == 0 {

			return nil, status.Error(codes.InvalidArgument,
				"both server_pubkey and client_pubkey "+
					"are required")
		}
		if recovery.CltvExpiry <= 0 {
			return nil, status.Error(codes.InvalidArgument,
				"cltv_expiry required in stateless mode")
		}
		if recovery.SwapInitiationHeight <= 0 {
			return nil, status.Error(codes.InvalidArgument,
				"swap_initiation_height required in "+
					"stateless mode")
		}
		if len(req.Preimage) == 0 {
			return nil, status.Error(codes.InvalidArgument,
				"preimage required in stateless mode")
		}
	}

	// Parse the inputs.
	htlcAddr, err := btcutil.DecodeAddress(
		req.HtlcAddress, chainParams,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid htlc_address: %v", err)
	}

	htlcPkScript, err := txscript.PayToAddrScript(htlcAddr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid htlc_address script: %v", err)
	}

	htlcOutpoint, err := wire.NewOutPointFromString(req.Outpoint)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Destination address: honor a provided override immediately so request
	// validation stays independent from swap lookup.
	var sweepAddr btcutil.Address
	if req.DestAddress != "" {
		sweepAddr, err = btcutil.DecodeAddress(
			req.DestAddress, chainParams,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid dest_address: %v", err)
		}
	}

	var (
		targetSwap   *loopdb.LoopOut
		targetHtlc   *swap.Htlc
		targetHash   lntypes.Hash
		preimage     lntypes.Preimage
		heightHint   int32
		storedDest   btcutil.Address
		keyDesc      keychain.KeyDescriptor
		clientKey    *btcec.PublicKey
		keyScanLimit uint32
	)

	if stateless {
		preimage, err = lntypes.MakePreimage(req.Preimage)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid preimage: %v", err)
		}

		serverKey, _, keyErr := parseSweepPubKey(
			"server_pubkey", recovery.ServerPubkey,
		)
		if keyErr != nil {
			return nil, keyErr
		}

		clientKeyBytes, clientPubKey, keyErr := parseSweepPubKey(
			"client_pubkey", recovery.ClientPubkey,
		)
		if keyErr != nil {
			return nil, keyErr
		}
		clientKey = clientPubKey
		keyDesc.PubKey = clientPubKey
		keyScanLimit = recovery.KeyScanLimit
		if keyScanLimit == 0 {
			keyScanLimit = defaultStatelessRecoveryKeyScanLimit
		}

		targetHash = preimage.Hash()
		contract := loopdb.SwapContract{
			Preimage:         preimage,
			CltvExpiry:       recovery.CltvExpiry,
			InitiationHeight: recovery.SwapInitiationHeight,
			ProtocolVersion:  loopdb.ProtocolVersionMuSig2,
			HtlcKeys: loopdb.HtlcKeys{
				SenderScriptKey:        serverKey,
				SenderInternalPubKey:   serverKey,
				ReceiverScriptKey:      clientKeyBytes,
				ReceiverInternalPubKey: clientKeyBytes,
			},
		}

		targetHtlc, err = utils.GetHtlc(
			targetHash, &contract, chainParams,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"construct stateless HTLC: %v", err)
		}

		if !bytes.Equal(targetHtlc.PkScript, htlcPkScript) {
			return nil, status.Errorf(
				codes.InvalidArgument,
				"provided HTLC address %s does not match "+
					"generated HTLC address %s",
				htlcAddr.EncodeAddress(),
				targetHtlc.Address.EncodeAddress(),
			)
		}

		heightHint = recovery.SwapInitiationHeight
	} else {
		// Locate the loop-out swap whose HTLC script matches the
		// requested HTLC address. This supplies all fields needed by
		// the legacy database-backed mode.
		swaps, storeErr := store.FetchLoopOutSwaps(ctx)
		if storeErr != nil {
			return nil, storeErr
		}

		for _, swp := range swaps {
			htlc, htlcErr := utils.GetHtlc(
				swp.Hash, &swp.Contract.SwapContract,
				chainParams,
			)
			if htlcErr != nil {
				return nil, htlcErr
			}

			if bytes.Equal(htlc.PkScript, htlcPkScript) {
				targetSwap = swp
				targetHtlc = htlc
				break
			}
		}

		if targetSwap == nil || targetHtlc == nil {
			return nil, status.Error(codes.NotFound,
				"no matching swap HTLC found")
		}

		targetHash = targetSwap.Hash
		heightHint = targetSwap.Contract.InitiationHeight
		storedDest = targetSwap.Contract.DestAddr
		keyDesc.KeyLocator = targetSwap.Contract.HtlcKeys.
			ClientScriptKeyLocator

		if len(req.Preimage) > 0 {
			preimage, err = lntypes.MakePreimage(req.Preimage)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"invalid preimage: %v", err)
			}
		} else {
			preimage = targetSwap.Contract.Preimage
		}
	}

	// Prefer the stored swap destination for recovery sweeps and only
	// derive a fresh wallet address when neither the request nor DB
	// specifies one.
	if sweepAddr == nil {
		sweepAddr = storedDest
	}
	if sweepAddr == nil {
		sweepAddr, err = wallet.NextAddr(
			ctx, lnwallet.DefaultAccountName,
			walletrpc.AddressType_TAPROOT_PUBKEY,
			false,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"derive sweep address: %v", err)
		}
		infof("sweephtlc: generated new destination address: %v",
			sweepAddr.EncodeAddress())
	}

	sweepPkScript, err := txscript.PayToAddrScript(sweepAddr)
	if err != nil {
		return nil, err
	}

	infof("sweephtlc: start sweep for %v -> %v", req.Outpoint,
		sweepAddr.EncodeAddress())

	infof("sweephtlc: using swap hash %v at height hint %v",
		targetHash, heightHint)

	if heightHint <= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid initiation height %d", heightHint)
	}

	// Wait for a confirmation so we can read the full transaction even if
	// it's not in our wallet.
	infof("sweephtlc: registering conf ntfn for %v hint=%v",
		req.Outpoint, heightHint)
	confChan, errChan, err := notifier.RegisterConfirmationsNtfn(
		ctx, &htlcOutpoint.Hash, htlcPkScript, 1,
		heightHint,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"register conf ntfn: %v", err)
	}

	var (
		htlcTxOut *wire.TxOut
		fundingTx *wire.MsgTx
	)

	infof("sweephtlc: waiting for confirmation of %v", req.Outpoint)
	select {
	case conf := <-confChan:
		if conf == nil {
			return nil, status.Error(codes.Internal,
				"confirmation notification was empty")
		}

		fundingTx = conf.Tx
		infof("sweephtlc: funding confirmed at height %v",
			conf.BlockHeight)

	case ntfnErr := <-errChan:
		infof("sweephtlc: conf ntfn error for %v: %v",
			req.Outpoint, ntfnErr)

		return nil, status.Errorf(codes.Internal,
			"conf ntfn: %v", ntfnErr)

	case <-ctx.Done():
		infof("sweephtlc: context done waiting for %v: %v",
			req.Outpoint, ctx.Err())

		return nil, status.Errorf(codes.DeadlineExceeded,
			"waiting for transaction details")
	}

	if fundingTx == nil {
		return nil, status.Error(codes.Internal,
			"confirmation did not include the funding transaction")
	}
	if fundingTx.TxHash() != htlcOutpoint.Hash {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"confirmed transaction %s does not match outpoint "+
				"transaction %s",
			fundingTx.TxHash(), htlcOutpoint.Hash,
		)
	}

	if int(htlcOutpoint.Index) >= len(fundingTx.TxOut) {
		return nil, status.Errorf(codes.InvalidArgument,
			"vout %d out of range", htlcOutpoint.Index)
	}

	htlcTxOut = fundingTx.TxOut[htlcOutpoint.Index]

	if !bytes.Equal(htlcTxOut.PkScript, htlcPkScript) {
		if stateless {
			observed := sweepOutputAddress(
				htlcTxOut.PkScript, chainParams,
			)

			return nil, status.Errorf(
				codes.InvalidArgument,
				"on-chain HTLC address %s does not match "+
					"generated HTLC address %s",
				observed, targetHtlc.Address.EncodeAddress(),
			)
		}

		return nil, status.Error(codes.InvalidArgument,
			"outpoint script does not match HTLC address")
	}

	infof("sweephtlc: swap hash validated for %v", req.Outpoint)

	if preimage.Hash() != targetHtlc.Hash {
		return nil, status.Error(codes.InvalidArgument,
			"preimage does not match HTLC hash")
	}

	infof("sweephtlc: sweeping to %v with feerate %v sat/vbyte",
		sweepAddr.EncodeAddress(), req.SatPerVbyte)

	// Estimate fee for the success-path spend weight.
	var estimator input.TxWeightEstimator
	err = targetHtlc.AddSuccessToEstimator(&estimator)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"failed to estimate tx input weight: %v", err)
	}
	err = sweep.AddOutputEstimate(&estimator, sweepAddr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"failed to estimate tx output weight: %v", err)
	}

	// Convert the requested fee rate to sat/kw for fee computation.
	feeRate := chainfee.SatPerVByte(req.SatPerVbyte).FeePerKWeight()
	fee := feeRate.FeeForWeightRoundUp(estimator.Weight())

	// Make sure the fee is fine.
	htlcValue := btcutil.Amount(htlcTxOut.Value)
	if htlcValue <= fee {
		return nil, status.Error(codes.InvalidArgument,
			"fee exceeds HTLC value")
	}

	minRelayFeeRate, err := wallet.MinRelayFee(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"min relay fee: %v", err)
	}

	fee, clamped, err := utils.ClampSweepFee(
		fee, htlcValue, utils.MaxFeeToAmountRatio, minRelayFeeRate,
		estimator.Weight(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"fee too low for relay after clamp: %v", err)
	}
	if clamped {
		return nil, status.Errorf(codes.InvalidArgument,
			"fee exceeds %.0f%% of HTLC value; lower sat_per_vbyte",
			utils.MaxFeeToAmountRatio*100,
		)
	}

	// Build the sweep transaction spending the HTLC via the success path.
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *htlcOutpoint,
		Sequence:         targetHtlc.SuccessSequence(),
	})
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: sweepPkScript,
		Value:    int64(htlcValue - fee),
	})

	infof("sweephtlc: signing sweep spending %v", req.Outpoint)

	prevOut := &wire.TxOut{
		Value:    int64(htlcValue),
		PkScript: targetHtlc.PkScript,
	}
	signDesc := lndclient.SignDescriptor{
		WitnessScript: targetHtlc.SuccessScript(),
		Output:        prevOut,
		HashType:      targetHtlc.SigHash(),
		InputIndex:    0,
		KeyDesc:       keyDesc,
	}
	if targetHtlc.Version == swap.HtlcV3 {
		signDesc.SignMethod = input.TaprootScriptSpendSignMethod
	}

	// Sign the HTLC spend. Stateless recovery first asks lnd to resolve
	// the supplied public key from its wallet. A signing error or invalid
	// signature triggers the bounded key-family scan.
	usedKeyScan := false
	scanAndSign := func() ([][]byte, error) {
		recoveredKey, scanErr := findStatelessSweepKey(
			ctx, wallet, clientKey, keyScanLimit,
		)
		if scanErr != nil {
			return nil, scanErr
		}

		signDesc.KeyDesc = recoveredKey
		usedKeyScan = true

		return signer.SignOutputRawKeyLocator(
			ctx, sweepTx,
			[]*lndclient.SignDescriptor{&signDesc},
			[]*wire.TxOut{prevOut},
		)
	}

	rawSigs, err := signer.SignOutputRaw(
		ctx, sweepTx, []*lndclient.SignDescriptor{&signDesc},
		[]*wire.TxOut{prevOut},
	)
	if err != nil {
		if !stateless {
			return nil, err
		}

		infof("sweephtlc: public-key signing failed: %v; "+
			"scanning up to %d family-%d keys", err,
			keyScanLimit, swap.KeyFamily,
		)
		rawSigs, err = scanAndSign()
		if err != nil {
			return nil, err
		}
	}

	applySignature := func(signatures [][]byte) error {
		if len(signatures) != 1 || len(signatures[0]) == 0 {
			return status.Error(codes.Internal,
				"signer returned an invalid signature count")
		}

		witness, witnessErr := targetHtlc.GenSuccessWitness(
			signatures[0], preimage,
		)
		if witnessErr != nil {
			return witnessErr
		}

		sweepTx.TxIn[0].Witness = witness

		return nil
	}
	if err = applySignature(rawSigs); err != nil {
		return nil, err
	}

	if stateless {
		if usedKeyScan {
			infof("sweephtlc: verifying stateless sweep witness " +
				"after key recovery")
		} else {
			infof("sweephtlc: verifying stateless sweep witness")
		}

		verifyErr := verifySweepHtlcWitness(sweepTx, prevOut)
		if verifyErr != nil && !usedKeyScan {
			infof("sweephtlc: public-key signature did not match "+
				"client key; scanning up to %d family-%d keys",
				keyScanLimit, swap.KeyFamily)

			rawSigs, err = scanAndSign()
			if err != nil {
				return nil, err
			}
			if err = applySignature(rawSigs); err != nil {
				return nil, err
			}

			infof("sweephtlc: verifying stateless sweep witness " +
				"after key recovery")
			verifyErr = verifySweepHtlcWitness(sweepTx, prevOut)
		}
		if verifyErr != nil {
			return nil, status.Errorf(codes.Internal,
				"lnd produced an invalid signature for "+
					"client_pubkey: %v", verifyErr)
		}
		infof("sweephtlc: stateless sweep witness verified")

		if usedKeyScan {
			infof("sweephtlc: signed with recovered client " +
				"key locator")
		} else {
			infof("sweephtlc: signed directly with client " +
				"public key")
		}
	}

	infof("sweephtlc: witness assembled, tx size=%d vbytes",
		sweepTx.SerializeSize())

	var rawBuf bytes.Buffer
	err = sweepTx.Serialize(&rawBuf)
	if err != nil {
		return nil, err
	}
	rawTx := rawBuf.Bytes()

	// Optionally publish immediately if requested; otherwise caller can
	// broadcast the signed tx themselves.
	if req.Publish {
		err = wallet.PublishTransaction(
			ctx, sweepTx,
			labels.LoopOutSweepSuccess(targetHash.String()),
		)
		if err != nil {
			errorf("sweephtlc: publish failed for %v: %v",
				req.Outpoint, err)

			return &looprpc.SweepHtlcResponse{
				SweepTx: rawTx,
				FeeSats: uint64(fee),
				Publish: &looprpc.SweepHtlcResponse_Failed{
					Failed: &looprpc.PublishFailed{
						Error: err.Error(),
					},
				},
			}, nil
		}

		infof("sweephtlc: published sweep %v", sweepTx.TxHash())
	}

	resp := &looprpc.SweepHtlcResponse{
		SweepTx: rawTx,
		FeeSats: uint64(fee),
	}
	if req.Publish {
		resp.Publish = &looprpc.SweepHtlcResponse_Published{
			Published: &looprpc.PublishSucceeded{},
		}
	} else {
		resp.Publish = &looprpc.SweepHtlcResponse_NotRequested{
			NotRequested: &looprpc.PublishNotRequested{},
		}
	}

	return resp, nil
}

// findStatelessSweepKey locates the client key in the Loop key family so lnd
// receives the actual key locator when signing.
func findStatelessSweepKey(ctx context.Context, wallet htlcWallet,
	targetKey *btcec.PublicKey,
	keyScanLimit uint32) (keychain.KeyDescriptor, error) {

	for index := range keyScanLimit {
		locator := keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.KeyFamily),
			Index:  index,
		}
		keyDesc, err := wallet.DeriveKey(ctx, &locator)
		if err != nil {
			return keychain.KeyDescriptor{}, status.Errorf(
				codes.Internal,
				"derive client key at family %d index %d: %v",
				swap.KeyFamily, index, err,
			)
		}
		if keyDesc == nil || keyDesc.PubKey == nil {
			return keychain.KeyDescriptor{}, status.Errorf(
				codes.Internal,
				"lnd returned an empty key at family %d "+
					"index %d",
				swap.KeyFamily, index,
			)
		}

		keysScanned := index + 1
		if keysScanned%statelessRecoveryKeyScanLogInterval == 0 {
			infof("sweephtlc: scanned %d of %d family-%d keys",
				keysScanned, keyScanLimit, swap.KeyFamily,
			)
		}

		if keyDesc.PubKey.IsEqual(targetKey) {
			infof("sweephtlc: found client key at family %d "+
				"index %d", swap.KeyFamily, index,
			)

			return *keyDesc, nil
		}
	}

	return keychain.KeyDescriptor{}, status.Errorf(
		codes.FailedPrecondition,
		"client_pubkey does not belong to the connected lnd wallet; "+
			"searched key family %d indices 0-%d",
		swap.KeyFamily, keyScanLimit-1,
	)
}

// parseSweepPubKey parses a canonical compressed public key for stateless
// recovery.
func parseSweepPubKey(name string, serialized []byte) ([33]byte,
	*btcec.PublicKey, error) {

	var keyBytes [33]byte
	if len(serialized) != len(keyBytes) {
		return keyBytes, nil, status.Errorf(
			codes.InvalidArgument, "%s must be 33 bytes", name,
		)
	}

	pubKey, err := btcec.ParsePubKey(serialized)
	if err != nil {
		return keyBytes, nil, status.Errorf(
			codes.InvalidArgument, "invalid %s: %v", name, err,
		)
	}
	if !bytes.Equal(serialized, pubKey.SerializeCompressed()) {
		return keyBytes, nil, status.Errorf(
			codes.InvalidArgument,
			"%s must use canonical compressed encoding", name,
		)
	}

	copy(keyBytes[:], serialized)

	return keyBytes, pubKey, nil
}

// sweepOutputAddress returns a printable address for an observed output. A
// script is returned when the output isn't a standard single-address script.
func sweepOutputAddress(pkScript []byte, chainParams *chaincfg.Params) string {
	_, addresses, _, err := txscript.ExtractPkScriptAddrs(
		pkScript, chainParams,
	)
	if err != nil || len(addresses) != 1 {
		return fmt.Sprintf("script:%x", pkScript)
	}

	return addresses[0].EncodeAddress()
}

// verifySweepHtlcWitness executes the stateless recovery witness against the
// exact on-chain output before the transaction can be returned or published.
func verifySweepHtlcWitness(sweepTx *wire.MsgTx, prevOut *wire.TxOut) error {
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevOutFetcher)
	engine, err := txscript.NewEngine(
		prevOut.PkScript, sweepTx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, prevOut.Value, prevOutFetcher,
	)
	if err != nil {
		return err
	}

	return engine.Execute()
}
