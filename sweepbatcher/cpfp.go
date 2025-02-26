package sweepbatcher

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ensurePresigned checks that we can sign a 1:1 transaction sweeping the input.
func (b *batch) ensurePresigned(ctx context.Context, newSweep *sweep) error {
	if b.cfg.cpfpHelper == nil {
		return fmt.Errorf("cpfpHelper is not installed")
	}
	if len(b.sweeps) != 0 {
		return fmt.Errorf("ensurePresigned is done when adding to an " +
			"empty batch")
	}

	sweeps := []sweep{
		{
			outpoint: newSweep.outpoint,
			value:    newSweep.value,
			cpfp:     newSweep.cpfp,
		},
	}

	// Cache the destination address.
	destAddr, err := b.getSweepsDestAddr(ctx, sweeps)
	if err != nil {
		return fmt.Errorf("failed to find destination address: %w", err)
	}

	// Set LockTime to 0. It is not critical.
	const currentHeight = 0

	// Check if we can sign with minimum fee rate.
	const feeRate = chainfee.FeePerKwFloor

	tx, _, _, _, err := constructUnsignedTx(
		sweeps, destAddr, currentHeight, feeRate,
	)
	if err != nil {
		return fmt.Errorf("failed to construct unsigned tx "+
			"for feeRate %v: %w", feeRate, err)
	}

	// Try to presign this transaction.
	batchAmt := newSweep.value
	signedTx, err := b.cfg.cpfpHelper.SignTx(ctx, tx, batchAmt, feeRate)
	if err != nil {
		return fmt.Errorf("failed to sign unsigned tx %v "+
			"for feeRate %v: %w", tx.TxHash(), feeRate, err)
	}

	// Check the SignTx worked correctly.
	err = CheckSignedTx(tx, signedTx, batchAmt, feeRate)
	if err != nil {
		return fmt.Errorf("signed tx doesn't correspond the "+
			"unsigned tx: %w", err)
	}

	return nil
}

// presign tries to presign batch sweep transactions composed of this batch and
// the sweep. It signs multiple versions of the transaction to make sure there
// is a transaction to be published if minRelayFee grows.
func (b *batch) presign(ctx context.Context, newSweep *sweep) error {
	if b.cfg.cpfpHelper == nil {
		return fmt.Errorf("cpfpHelper is not installed")
	}
	if len(b.sweeps) == 0 {
		return fmt.Errorf("presigning is done when adding to a " +
			"non-empty batch")
	}

	// Create the list of sweeps of the future batch.
	sweeps := make([]sweep, 0, len(b.sweeps)+1)
	for _, sweep := range b.sweeps {
		sweeps = append(sweeps, sweep)
	}
	existingSweeps := sweeps
	sweeps = append(sweeps, *newSweep)

	// Cache the destination address.
	destAddr, err := b.getSweepsDestAddr(ctx, existingSweeps)
	if err != nil {
		return fmt.Errorf("failed to find destination address: %w", err)
	}

	return presign(ctx, b.cfg.cpfpHelper, sweeps, destAddr)
}

// presigner tries to presign a batch transaction.
type presigner interface {
	// Presign tries to presign a batch transaction. If the method returns
	// nil, it is guaranteed that future calls to SignTx on this set of
	// sweeps return valid signed transactions.
	Presign(ctx context.Context, tx *wire.MsgTx,
		inputAmt btcutil.Amount) error
}

// presign tries to presign batch sweep transactions of the sweeps. It signs
// multiple versions of the transaction to make sure there is a transaction to
// be published if minRelayFee grows.
func presign(ctx context.Context, presigner presigner, sweeps []sweep,
	destAddr btcutil.Address) error {

	if presigner == nil {
		return fmt.Errorf("presigner is not installed")
	}

	if len(sweeps) == 0 {
		return fmt.Errorf("there are no sweeps")
	}

	// Keep track of the total amount this batch is sweeping back.
	batchAmt := btcutil.Amount(0)
	for _, sweep := range sweeps {
		batchAmt += sweep.value
	}

	// Go from the floor (1.01 sat/vbyte) to 2k sat/vbyte with step of 1.5x.
	const (
		start = chainfee.FeePerKwFloor
		stop  = chainfee.AbsoluteFeePerKwFloor * 2_000
	)

	// Set LockTime to 0. It is not critical.
	const currentHeight = 0

	for feeRate := start; feeRate <= stop; feeRate = (feeRate * 3) / 2 {
		// Construct an unsigned transaction for this fee rate.
		tx, _, feeForWeight, fee, err := constructUnsignedTx(
			sweeps, destAddr, currentHeight, feeRate,
		)
		if err != nil {
			return fmt.Errorf("failed to construct unsigned tx "+
				"for feeRate %v: %w", feeRate, err)
		}

		// Try to presign this transaction.
		err = presigner.Presign(ctx, tx, batchAmt)
		if err != nil {
			return fmt.Errorf("failed to presign unsigned tx %v "+
				"for feeRate %v: %w", tx.TxHash(), feeRate, err)
		}

		// If fee was clamped, stop here, because fee rate won't grow.
		if fee < feeForWeight {
			break
		}
	}

	return nil
}

// cpfpLabelPrefix is a prefix added to the label of the batch to form a label
// for CPFP transaction.
const cpfpLabelPrefix = "cpfp-for-"

// publishWithCPFP creates sweep transaction using a custom transaction signer
// and publishes it. It may use CPFP if the custom signer returned a pre-signed
// transaction with insufficient fee. It returns fee of the first transaction,
// not including CPFP's fee, an error (if signing and/or publishing failed) and
// a boolean flag indicating signing success. This mode is incompatible with
// an external address, because it may use CPFP and is designed for batches.
func (b *batch) publishWithCPFP(ctx context.Context) (btcutil.Amount, error,
	bool) {

	// Sanity check, there should be at least 1 sweep in this batch.
	if len(b.sweeps) == 0 {
		return 0, fmt.Errorf("no sweeps in batch"), false
	}

	// Make sure that no external address is used.
	for _, sweep := range b.sweeps {
		if sweep.isExternalAddr {
			return 0, fmt.Errorf("external address was used with " +
				"a custom transaction signer"), false
		}
	}

	// Cache current height and desired feerate of the batch.
	currentHeight := b.currentHeight
	feeRate := b.rbfCache.FeeRate

	// Append this sweep to an array of sweeps. This is needed to keep the
	// order of sweeps stored, as iterating the sweeps map does not
	// guarantee same order.
	sweeps := make([]sweep, 0, len(b.sweeps))
	for _, sweep := range b.sweeps {
		sweeps = append(sweeps, sweep)
	}

	// Cache the destination address.
	address, err := b.getSweepsDestAddr(ctx, sweeps)
	if err != nil {
		return 0, fmt.Errorf("failed to find destination address: %w",
			err), false
	}

	// Construct unsigned batch transaction.
	tx, weight, _, fee, err := constructUnsignedTx(
		sweeps, address, currentHeight, feeRate,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to construct tx: %w", err),
			false
	}

	// Adjust feeRate, because it may have been clamped.
	feeRate = chainfee.NewSatPerKWeight(fee, weight)

	// Calculate total input amount.
	batchAmt := btcutil.Amount(0)
	for _, sweep := range sweeps {
		batchAmt += sweep.value
	}

	// Determine the current minimum relay fee based on our chain backend.
	minRelayFee, err := b.wallet.MinRelayFee(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get minRelayFee: %w", err),
			false
	}

	// Get a signed transaction. It may be either new transaction or a
	// pre-signed one.
	signedTx, err := b.cfg.cpfpHelper.SignTx(ctx, tx, batchAmt, minRelayFee)
	if err != nil {
		return 0, fmt.Errorf("failed to sign tx: %w", err),
			false
	}

	// Run sanity checks to make sure cpfpHelper.SignTx complied with all
	// the invariants.
	err = CheckSignedTx(tx, signedTx, batchAmt, minRelayFee)
	if err != nil {
		return 0, fmt.Errorf("signed tx doesn't correspond the "+
			"unsigned tx: %w", err), false
	}
	tx = signedTx
	txHash := tx.TxHash()

	// Make sure tx weight matches the expected value.
	realWeight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(tx)),
	)
	if realWeight != weight {
		b.log().Warnf("actual weight of tx %v is %v, estimated as %d",
			txHash, realWeight, weight)
	}

	// Find actual fee rate of the signed transaction. It may differ from
	// the desired fee rate, because SignTx may return a presigned tx.
	output := btcutil.Amount(tx.TxOut[0].Value)
	fee = batchAmt - output
	signedFeeRate := chainfee.NewSatPerKWeight(fee, realWeight)

	b.log().Infof("attempting to publish custom signed tx=%v, "+
		"desiredFeerate=%v, signedFeeRate=%v, weight=%v, fee=%v, "+
		"sweeps=%d, destAddr=%s", txHash, feeRate, signedFeeRate,
		weight, fee, len(tx.TxIn), address)
	b.debugLogTx("serialized batch", tx)

	// Publish the transaction. If it fails, we don't return immediately,
	// because we may still need a CPFP and it can be done against a
	// previously published transaction.
	publishErr1 := b.wallet.PublishTransaction(
		ctx, tx, b.cfg.txLabeler(b.id),
	)
	if publishErr1 == nil {
		// Store the batch transaction's txid and pkScript, to use in
		// CPFP and for monitoring purposes.
		b.batchTxid = &txHash
		b.batchPkScript = tx.TxOut[0].PkScript

		if err := b.persist(ctx); err != nil {
			return 0, fmt.Errorf("failed to persist: %w", err), true
		}
	} else {
		b.log().Infof("failed to publish custom signed tx=%v, "+
			"desiredFeerate=%v, signedFeeRate=%v, weight=%v, "+
			"fee=%v, sweeps=%d, destAddr=%s", txHash, feeRate,
			signedFeeRate, weight, fee, len(tx.TxIn), address)
	}

	// Load previously published tx if it exists.
	var parentTx *wire.MsgTx
	if b.batchTxid != nil {
		parentTx, err = b.cfg.cpfpHelper.LoadTx(ctx, *b.batchTxid)
		if err != nil {
			return 0, fmt.Errorf("failed to load batch tx %v: %w",
				*b.batchTxid, err), true
		}
	} else {
		b.log().Warnf("need a CPFP, but there is no published tx known")
	}

	// Print this log here, to keep isCPFPNeeded a pure function.
	if parentTx != nil && len(parentTx.TxIn) < len(tx.TxIn) {
		b.log().Infof("Skip publishing CPFP, because batch tx in mempool"+
			"has %d inputs, while the batch has now %d inputs",
			len(parentTx.TxIn), len(tx.TxIn))
	}

	// Determine if CPFP is needed and its feerate.
	needsCPFP, err := isCPFPNeeded(
		parentTx, batchAmt, len(tx.TxIn), feeRate, signedFeeRate,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to determine if CPFP is "+
			"needed: %w", err), false
	}

	// If CPFP is not needed, we are done now.
	if !needsCPFP {
		b.log().Infof("CPFP is not needed")

		return fee, publishErr1, true
	}

	b.log().Infof("CPFP is needed, parent is %v", parentTx.TxHash())

	// Create and sign CPFP.
	parentWeight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(parentTx)),
	)
	parentOutput := btcutil.Amount(parentTx.TxOut[0].Value)
	parentFee := batchAmt - parentOutput
	childTx, childFeeRate, err := makeUnsignedCPFP(
		*b.batchTxid, parentOutput, parentWeight, parentFee,
		minRelayFee, feeRate, address, currentHeight,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to make CPFP tx: %w", err),
			true
	}

	childTx, err = b.signChildTx(ctx, childTx)
	if err != nil {
		return 0, fmt.Errorf("failed to sign CPFP tx: %w", err),
			true
	}

	childTxHash := childTx.TxHash()
	parentFeeRate := chainfee.NewSatPerKWeight(parentFee, parentWeight)
	b.log().Infof("attempting to publish child tx %v to CPFP parent tx %v,"+
		" effectiveFeeRate=%v, parentFeeRate=%v, childFeeRate=%v",
		childTxHash, *b.batchTxid, feeRate, parentFeeRate,
		childFeeRate)
	b.debugLogTx("serialized child tx", childTx)

	// Publish child transaction.
	publishErr2 := b.wallet.PublishTransaction(
		ctx, childTx, cpfpLabelPrefix+b.cfg.txLabeler(b.id),
	)
	if publishErr2 != nil {
		b.log().Infof("failed to publish child tx %v to CPFP parent "+
			"tx %v, effectiveFeeRate=%v, parentFeeRate=%v, "+
			"childFeeRate=%v", childTxHash, *b.batchTxid, feeRate,
			parentFeeRate, childFeeRate)

		return fee, publishErr2, true
	}

	return fee, publishErr1, true
}

// getSweepsDestAddr returns the destination address used by a group of sweeps.
// The method must be used in CPFP mode only.
func (b *batch) getSweepsDestAddr(ctx context.Context,
	sweeps []sweep) (btcutil.Address, error) {

	if b.cfg.cpfpHelper == nil {
		return nil, fmt.Errorf("getSweepsDestAddr used without CPFP")
	}

	inputs := make([]wire.OutPoint, len(sweeps))
	for i, s := range sweeps {
		if !s.cpfp {
			return nil, fmt.Errorf("getSweepsDestAddr used on a " +
				"non-CPFP input")
		}

		inputs[i] = s.outpoint
	}

	// Load pkScript from the CPFP helper.
	pkScriptBytes, err := b.cfg.cpfpHelper.DestPkScript(ctx, inputs)
	if err != nil {
		return nil, fmt.Errorf("cpfpHelper.DestPkScript failed for "+
			"inputs %v: %w", inputs, err)
	}

	// Convert pkScript to btcutil.Address.
	pkScript, err := txscript.ParsePkScript(pkScriptBytes)
	if err != nil {
		return nil, fmt.Errorf("txscript.ParsePkScript failed for "+
			"pkScript %x returned for inputs %v: %w", pkScriptBytes,
			inputs, err)
	}

	address, err := pkScript.Address(b.cfg.chainParams)
	if err != nil {
		return nil, fmt.Errorf("pkScript.Address failed for "+
			"pkScript %x returned for inputs %v: %w", pkScriptBytes,
			inputs, err)
	}

	return address, nil
}

// CheckSignedTx makes sure that signedTx matches the unsignedTx. It checks
// according to criteria specified in the description of CpfpHelper.SignTx.
func CheckSignedTx(unsignedTx, signedTx *wire.MsgTx, inputAmt btcutil.Amount,
	minRelayFee chainfee.SatPerKWeight) error {

	// Make sure the set of inputs is the same.
	unsignedMap := make(map[wire.OutPoint]uint32, len(unsignedTx.TxIn))
	for _, txIn := range unsignedTx.TxIn {
		unsignedMap[txIn.PreviousOutPoint] = txIn.Sequence
	}
	for _, txIn := range signedTx.TxIn {
		seq, has := unsignedMap[txIn.PreviousOutPoint]
		if !has {
			return fmt.Errorf("input %s is new in signed tx",
				txIn.PreviousOutPoint)
		}
		if seq != txIn.Sequence {
			return fmt.Errorf("sequence mismatch in input %s: "+
				"%d in unsigned, %d in signed",
				txIn.PreviousOutPoint, seq, txIn.Sequence)
		}
		delete(unsignedMap, txIn.PreviousOutPoint)
	}
	for outpoint := range unsignedMap {
		return fmt.Errorf("input %s is missing in signed tx", outpoint)
	}

	// Compare outputs.
	if len(unsignedTx.TxOut) != 1 {
		return fmt.Errorf("unsigned tx has %d outputs, want 1",
			len(unsignedTx.TxOut))
	}
	if len(signedTx.TxOut) != 1 {
		return fmt.Errorf("the signed tx has %d outputs, want 1",
			len(signedTx.TxOut))
	}
	unsignedOut := unsignedTx.TxOut[0]
	signedOut := signedTx.TxOut[0]
	if !bytes.Equal(unsignedOut.PkScript, signedOut.PkScript) {
		return fmt.Errorf("mismatch of output pkScript: %v, %v",
			unsignedOut.PkScript, signedOut.PkScript)
	}

	// Find the feerate of signedTx.
	fee := inputAmt - btcutil.Amount(signedOut.Value)
	weight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(signedTx)),
	)
	feeRate := chainfee.NewSatPerKWeight(fee, weight)
	if feeRate < minRelayFee {
		return fmt.Errorf("feerate (%v) of signed tx is lower than "+
			"minRelayFee (%v)", feeRate, minRelayFee)
	}

	// Check LockTime.
	if signedTx.LockTime > unsignedTx.LockTime {
		return fmt.Errorf("locktime (%d) of signed tx is higher than "+
			"locktime of unsigned tx (%d)", signedTx.LockTime,
			unsignedTx.LockTime)
	}

	// Check Version.
	if signedTx.Version != unsignedTx.Version {
		return fmt.Errorf("version (%d) of signed tx is not equal to "+
			"version of unsigned tx (%d)", signedTx.Version,
			unsignedTx.Version)
	}

	return nil
}

// feeRateThresholdPPM is the ratio of accepted underpayment of fee for which
// no CPFP is used to adjust the effective fee rate. If the underpayment is
// higher, then CPFP is enabled. It is measured in PPM, current level is 2%.
const feeRateThresholdPPM = 2_0000

// isCPFPNeeded returns if CPFP is needed to make the effective fee rate close
// to the desired feeRate. The threshold is feeRateThresholdPPM.
func isCPFPNeeded(parentTx *wire.MsgTx, inputAmt btcutil.Amount, numSweeps int,
	desiredFeeRate, signedFeeRate chainfee.SatPerKWeight) (bool, error) {

	// First, if feerate of the signed tx matches exactly the desired
	// feerate, this means, that we didn't use a presigned transaction,
	// which means that all the input are likely to be online, so we don't
	// use CPFP.
	if desiredFeeRate == signedFeeRate {
		return false, nil
	}

	// If no transaction was ever published, we can't do CPFP anyway. A
	// warning is produced by the calling function in this case.
	if parentTx == nil {
		return false, nil
	}

	// Sanity checks.
	if len(parentTx.TxOut) != 1 {
		return false, fmt.Errorf("batch tx must have one output, "+
			"but it has %d", len(parentTx.TxOut))
	}

	// Make sure that the parent transaction is signed.
	for _, txIn := range parentTx.TxIn {
		if len(txIn.Witness) == 0 {
			return false, fmt.Errorf("the tx must be signed")
		}
	}

	// If previously published tx has fewer inputs than the current state
	// of the batch, skip CPFP, since it would bump an outdated state.
	if len(parentTx.TxIn) < numSweeps {
		return false, nil
	}

	// Previously published transaction must not have more inputs than the
	// current batch state, because inputs are only added.
	if len(parentTx.TxIn) > numSweeps {
		return false, fmt.Errorf("parent tx has more inputs (%d) than "+
			"exist in the batch currently (%d)", len(parentTx.TxIn),
			numSweeps)
	}

	// Calculate fee rate of the transaction.
	weight := lntypes.WeightUnit(
		blockchain.GetTransactionWeight(btcutil.NewTx(parentTx)),
	)
	fee := inputAmt - btcutil.Amount(parentTx.TxOut[0].Value)
	if fee < 0 {
		return false, fmt.Errorf("the tx has negative fee %v", fee)
	}
	parentFeeRate := chainfee.NewSatPerKWeight(fee, weight)

	// Check of the observed_feerate < desired_feerate - threshold.
	threshold := desiredFeeRate * feeRateThresholdPPM / 1_000_000
	cpfpNeeded := parentFeeRate < desiredFeeRate-threshold

	return cpfpNeeded, nil
}

// maxChildFeeSharePPM specifies max share (in ppm) of total funds that can be
// burn in the child transaction in CPFP. Currently it is set to 20%.
const maxChildFeeSharePPM = 20_0000

// makeUnsignedCPFP constructs unsigned child tx for CPFP to achieve desired
// effective fee rate. It also returns fee rate of the constructed child tx.
// The transaction spends the UTXO to the same address. Supports P2WKH, P2TR.
func makeUnsignedCPFP(parentTxid chainhash.Hash, parentOutput btcutil.Amount,
	parentWeight lntypes.WeightUnit, parentFee btcutil.Amount, minRelayFee,
	effectiveFeeRate chainfee.SatPerKWeight, address btcutil.Address,
	currentHeight int32) (*wire.MsgTx, chainfee.SatPerKWeight, error) {

	// Estimate the weight of the child tx.
	var estimator input.TxWeightEstimator
	switch address.(type) {
	case *btcutil.AddressWitnessPubKeyHash:
		estimator.AddP2WKHInput()
		estimator.AddP2WKHOutput()

	case *btcutil.AddressTaproot:
		estimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
		estimator.AddP2TROutput()

	default:
		return nil, 0, fmt.Errorf("unknown address type %T", address)
	}
	childWeight := estimator.Weight()

	// Estimate the fee of the child tx.
	totalWeight := parentWeight + childWeight
	totalFee := effectiveFeeRate.FeeForWeight(totalWeight)
	childFee := totalFee - parentFee
	childFeeRate := chainfee.NewSatPerKWeight(childFee, childWeight)
	if childFeeRate < minRelayFee {
		childFeeRate = minRelayFee
		childFee = childFeeRate.FeeForWeight(childWeight)
	}
	if childFeeRate < effectiveFeeRate {
		return nil, 0, fmt.Errorf("got child fee rate %v lower than "+
			"effective fee rate %v", childFeeRate, effectiveFeeRate)
	}
	if childFee > parentOutput*maxChildFeeSharePPM/1_000_000 {
		return nil, 0, fmt.Errorf("child fee %v is higher than %d%% "+
			"of total funds %v", childFee,
			maxChildFeeSharePPM*100/1_000_000, parentOutput)
	}

	// Construct child tx.
	childTx := &wire.MsgTx{
		Version:  2,
		LockTime: uint32(currentHeight),
	}
	childTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parentTxid,
			Index: 0,
		},
	})
	pkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return nil, 0, fmt.Errorf("txscript.PayToAddrScript "+
			"failed: %w", err)
	}
	childTx.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    int64(parentOutput - childFee),
	})

	return childTx, childFeeRate, nil
}

// signChildTx signs child CPFP transaction using LND client.
func (b *batch) signChildTx(ctx context.Context,
	unsignedTx *wire.MsgTx) (*wire.MsgTx, error) {

	// Create PSBT packet object.
	packet, err := psbt.NewFromUnsignedTx(unsignedTx)
	if err != nil {
		return nil, fmt.Errorf("failed to create PSBT: %w", err)
	}

	packet, err = b.wallet.SignPsbt(ctx, packet)
	if err != nil {
		return nil, fmt.Errorf("signing PSBT failed: %w", err)
	}

	return psbt.Extract(packet)
}
