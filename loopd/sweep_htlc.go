package loopd

import (
	"bytes"
	"context"

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

	// Destination address: honor a provided override or derive a fresh
	// wallet address from the default account.
	var sweepAddr btcutil.Address
	if req.DestAddress != "" {
		sweepAddr, err = btcutil.DecodeAddress(
			req.DestAddress, chainParams,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid dest_address: %v", err)
		}
	} else {
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

	// Locate the loop-out swap whose HTLC script matches the outpoint so
	// we can obtain keys and the stored preimage.
	swaps, err := store.FetchLoopOutSwaps(ctx)
	if err != nil {
		return nil, err
	}

	var (
		targetSwap *loopdb.LoopOut
		targetHtlc *swap.Htlc
	)

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

	infof("sweephtlc: matched swap %v at height hint %v",
		targetSwap.Hash, targetSwap.Contract.InitiationHeight)

	if targetSwap.Contract.InitiationHeight <= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid initiation height %d",
			targetSwap.Contract.InitiationHeight)
	}

	// Wait for a confirmation so we can read the full transaction even if
	// it's not in our wallet.
	infof("sweephtlc: registering conf ntfn for %v hint=%v",
		req.Outpoint, targetSwap.Contract.InitiationHeight)
	confChan, errChan, err := notifier.RegisterConfirmationsNtfn(
		ctx, &htlcOutpoint.Hash, htlcPkScript, 1,
		targetSwap.Contract.InitiationHeight,
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

	if int(htlcOutpoint.Index) >= len(fundingTx.TxOut) {
		return nil, status.Errorf(codes.InvalidArgument,
			"vout %d out of range", htlcOutpoint.Index)
	}

	htlcTxOut = fundingTx.TxOut[htlcOutpoint.Index]

	if !bytes.Equal(htlcTxOut.PkScript, htlcPkScript) {
		return nil, status.Error(codes.InvalidArgument,
			"outpoint script does not match HTLC address")
	}

	infof("sweephtlc: swap hash validated for %v", req.Outpoint)

	// Pick a preimage: prefer the caller-provided override, otherwise use
	// the swap's stored preimage.
	var preimage lntypes.Preimage
	if len(req.Preimage) > 0 {
		preimage, err = lntypes.MakePreimage(req.Preimage)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid preimage: %v", err)
		}
	} else {
		preimage = targetSwap.Contract.Preimage
	}

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
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: targetSwap.Contract.HtlcKeys.
				ClientScriptKeyLocator,
		},
	}
	if targetHtlc.Version == swap.HtlcV3 {
		signDesc.SignMethod = input.TaprootScriptSpendSignMethod
	}

	// Sign the HTLC spend.
	rawSigs, err := signer.SignOutputRaw(
		ctx, sweepTx, []*lndclient.SignDescriptor{&signDesc},
		[]*wire.TxOut{prevOut},
	)
	if err != nil {
		return nil, err
	}
	sig := rawSigs[0]

	infof("sweephtlc: witness assembled, tx size=%d vbytes",
		sweepTx.SerializeSize())

	// Assemble the success witness using the signature and preimage.
	witness, err := targetHtlc.GenSuccessWitness(sig, preimage)
	if err != nil {
		return nil, err
	}
	sweepTx.TxIn[0].Witness = witness

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
			labels.LoopOutSweepSuccess(targetSwap.Hash.String()),
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
