package deposit

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// Sweeper is a higher level type that provides methods to sweep asset deposits.
type Sweeper struct {
	tapdClient *assets.TapdClient
	walletKit  lndclient.WalletKitClient
	signer     lndclient.SignerClient

	addressParams address.ChainParams
}

// NewSweeper creates a new Sweeper instance.
func NewSweeper(tapdClient *assets.TapdClient,
	walletKit lndclient.WalletKitClient, signer lndclient.SignerClient,
	addressParams address.ChainParams) *Sweeper {

	return &Sweeper{
		tapdClient:    tapdClient,
		walletKit:     walletKit,
		signer:        signer,
		addressParams: addressParams,
	}
}

// PublishDepositSweepMuSig2 publishes an interactive deposit sweep using the
// MuSig2 keyspend path.
func (s *Sweeper) PublishDepositSweepMuSig2(ctx context.Context, deposit *Kit,
	funder bool, depositProof *proof.Proof,
	otherInternalKey *btcec.PrivateKey, sweepScriptKey asset.ScriptKey,
	sweepInternalKey *btcec.PublicKey, label string,
	feeRate chainfee.SatPerVByte, lockID wtxmgr.LockID,
	lockDuration time.Duration) (*taprpc.SendAssetResponse, error) {

	// Verify that the proof is valid for the deposit and get the root hash
	// which we will be using as our taproot tweak.
	rootHash, err := deposit.VerifyProof(depositProof)
	if err != nil {
		log.Errorf("failed to verify deposit proof: %v", err)

		return nil, err
	}

	// Create the sweep vpacket which is simply sweeping the asset on the
	// OP_TRUE output to a new output with the provided script and internal
	// keys.
	sweepVpkt, err := assets.CreateOpTrueSweepVpkt(
		ctx, []*proof.Proof{depositProof}, sweepScriptKey,
		sweepInternalKey, nil, &s.addressParams,
	)
	if err != nil {
		return nil, err
	}

	// Gather the list of leased UTXOs that are used for the deposit sweep.
	// This is needed to ensure that the UTXOs are correctly reused if we
	// re-publish the deposit sweep.
	leases, err := s.walletKit.ListLeases(ctx)
	if err != nil {
		return nil, err
	}

	var leasedUtxos []lndclient.LeaseDescriptor
	for _, lease := range leases {
		if lease.LockID == lockID {
			leasedUtxos = append(leasedUtxos, lease)
		}
	}

	// By committing the virtual transaction to the BTC template we created,
	// the underlying lnd node will fund the BTC level transaction with an
	// input to pay for the fees (and it will also add a change output).
	sweepBtcPkt, activeAssets, passiveAssets, commitResp, err :=
		s.tapdClient.PrepareAndCommitVirtualPsbts(
			ctx, sweepVpkt, feeRate, nil, s.addressParams.Params,
			leasedUtxos, &lockID, lockDuration,
		)
	if err != nil {
		return nil, err
	}

	prevOutFetcher := wallet.PsbtPrevOutputFetcher(sweepBtcPkt)
	sigHash, err := getSigHash(sweepBtcPkt.UnsignedTx, 0, prevOutFetcher)
	if err != nil {
		return nil, err
	}

	tweaks := &input.MuSig2Tweaks{
		TaprootTweak: rootHash,
	}

	pubKey := deposit.FunderScriptKey
	otherInternalPubKey := deposit.CoSignerInternalKey
	if !funder {
		pubKey = deposit.CoSignerScriptKey
		otherInternalPubKey = deposit.FunderInternalKey
	}

	internalPubKey, internalKey, err := DeriveSharedDepositKey(
		ctx, s.signer, pubKey,
	)
	if err != nil {
		return nil, err
	}

	finalSig, err := utils.MuSig2Sign(
		input.MuSig2Version100RC2,
		[]*btcec.PrivateKey{
			internalKey, otherInternalKey,
		},
		[]*btcec.PublicKey{
			internalPubKey, otherInternalPubKey,
		},
		tweaks, sigHash,
	)
	if err != nil {
		return nil, err
	}

	// Make sure that the signature is valid for the tx sighash and deposit
	// internal key.
	schnorrSig, err := schnorr.ParseSignature(finalSig)
	if err != nil {
		return nil, err
	}

	// Calculate the final, tweaked MuSig2 output key.
	taprootOutputKey := txscript.ComputeTaprootOutputKey(
		deposit.MuSig2Key.PreTweakedKey, rootHash,
	)

	// Make sure we always return the parity stripped key.
	taprootOutputKey, _ = schnorr.ParsePubKey(schnorr.SerializePubKey(
		taprootOutputKey,
	))

	// Finally, verify that the signature is valid for the sighash and
	// tweaked MuSig2 output key.
	if !schnorrSig.Verify(sigHash[:], taprootOutputKey) {
		return nil, fmt.Errorf("invalid signature")
	}

	// Create the witness and add it to the sweep packet.
	var buf bytes.Buffer
	err = psbt.WriteTxWitness(&buf, wire.TxWitness{finalSig})
	if err != nil {
		return nil, err
	}

	sweepBtcPkt.Inputs[0].FinalScriptWitness = buf.Bytes()

	// Sign and finalize the sweep packet.
	signedBtcPacket, err := s.walletKit.SignPsbt(ctx, sweepBtcPkt)
	if err != nil {
		return nil, err
	}

	finalizedBtcPacket, _, err := s.walletKit.FinalizePsbt(
		ctx, signedBtcPacket, "",
	)
	if err != nil {
		return nil, err
	}

	// Finally publish the sweep and log the transfer.
	skipBroadcast := false
	sendAssetResp, err := s.tapdClient.LogAndPublish(
		ctx, finalizedBtcPacket, activeAssets, passiveAssets,
		commitResp, skipBroadcast, label,
	)

	return sendAssetResp, err
}

// PublishDepositTimeoutSweep publishes a deposit timeout sweep using the
// timeout script spend path.
func (s *Sweeper) PublishDepositTimeoutSweep(ctx context.Context, deposit *Kit,
	depositProof *proof.Proof, sweepScriptKey asset.ScriptKey,
	sweepInternalKey *btcec.PublicKey, label string,
	feeRate chainfee.SatPerVByte, lockID wtxmgr.LockID,
	lockDuration time.Duration) (*taprpc.SendAssetResponse, error) {

	// Create the sweep vpacket which is simply sweeping the asset on the
	// OP_TRUE output to a new output with the provided script and internal
	// keys.
	sweepVpkt, err := assets.CreateOpTrueSweepVpkt(
		ctx, []*proof.Proof{depositProof}, sweepScriptKey,
		sweepInternalKey, nil, &s.addressParams,
	)
	if err != nil {
		log.Errorf("Unable to create timeout sweep vpkt: %v", err)

		return nil, err
	}

	// Gather the list of leased UTXOs that are used for the deposit sweep.
	// This is needed to ensure that the UTXOs are correctly reused if we
	// re-publish the deposit sweep.
	leases, err := s.walletKit.ListLeases(ctx)
	if err != nil {
		log.Errorf("Unable to list leases: %v", err)

		return nil, err
	}

	var leasedUtxos []lndclient.LeaseDescriptor
	for _, lease := range leases {
		if lease.LockID == lockID {
			leasedUtxos = append(leasedUtxos, lease)
		}
	}

	// By committing the virtual transaction to the BTC template we created,
	// the underlying lnd node will fund the BTC level transaction with an
	// input to pay for the fees (and it will also add a change output).
	timeoutSweepBtcPkt, activeAssets, passiveAssets, commitResp, err :=
		s.tapdClient.PrepareAndCommitVirtualPsbts(
			ctx, sweepVpkt, feeRate, nil,
			s.addressParams.Params, leasedUtxos,
			&lockID, lockDuration,
		)
	if err != nil {
		log.Errorf("Unable to prepare and commit virtual psbt: %v",
			err)
	}

	// Create the witness for the timeout sweep.
	witness, err := deposit.CreateTimeoutWitness(
		ctx, s.signer, depositProof, timeoutSweepBtcPkt,
	)
	if err != nil {
		log.Errorf("Unable to create timeout witness: %v", err)

		return nil, err
	}

	// Now add the witness to the sweep packet.
	var buf bytes.Buffer
	err = psbt.WriteTxWitness(&buf, witness)
	if err != nil {
		log.Errorf("Unable to write witness to buffer: %v", err)

		return nil, err
	}

	timeoutSweepBtcPkt.Inputs[0].SighashType = txscript.SigHashDefault
	timeoutSweepBtcPkt.Inputs[0].FinalScriptWitness = buf.Bytes()

	// Sign and finalize the sweep packet.
	signedBtcPacket, err := s.walletKit.SignPsbt(ctx, timeoutSweepBtcPkt)
	if err != nil {
		log.Errorf("Unable to sign timeout sweep packet: %v", err)

		return nil, err
	}

	finalizedBtcPacket, _, err := s.walletKit.FinalizePsbt(
		ctx, signedBtcPacket, "",
	)
	if err != nil {
		log.Errorf("Unable to finalize timeout sweep packet: %v", err)

		return nil, err
	}

	anchorTxHash := depositProof.AnchorTx.TxHash()
	depositOutIdx := depositProof.InclusionProof.OutputIndex

	// Register the deposit transfer. This essentially materializes an asset
	// "out of thin air" to ensure that LogAndPublish succeeds and the asset
	// balance will be updated correctly.
	depositScriptKey := depositProof.Asset.ScriptKey.PubKey
	_, err = s.tapdClient.RegisterTransfer(
		ctx, &taprpc.RegisterTransferRequest{
			AssetId:   deposit.AssetID[:],
			GroupKey:  nil,
			ScriptKey: depositScriptKey.SerializeCompressed(),
			Outpoint: &taprpc.OutPoint{
				Txid:        anchorTxHash[:],
				OutputIndex: depositOutIdx,
			},
		},
	)
	if err != nil {
		if !strings.Contains(err.Error(), "proof already exists") {
			log.Errorf("Unable to register deposit transfer: %v",
				err)

			return nil, err
		}
	}

	// Publish the timeout sweep and log the transfer.
	sendAssetResp, err := s.tapdClient.LogAndPublish(
		ctx, finalizedBtcPacket, activeAssets, passiveAssets,
		commitResp, false, label,
	)
	if err != nil {
		log.Errorf("Failed to publish timeout sweep: %v", err)

		return nil, err
	}

	return sendAssetResp, nil
}

// getSigHash calculates the signature hash for the given transaction.
func getSigHash(tx *wire.MsgTx, idx int,
	prevOutFetcher txscript.PrevOutputFetcher) ([32]byte, error) {

	var sigHash [32]byte

	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	taprootSigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, tx, idx, prevOutFetcher,
	)
	if err != nil {
		return sigHash, err
	}

	copy(sigHash[:], taprootSigHash)

	return sigHash, nil
}

// GetHTLC creates a new zero-fee HTLC packet to be able to partially sign it
// and send it to the server for further processing.
//
// TODO(bhandras): add support for spending multiple deposits into the HTLC.
func (s *Sweeper) GetHTLC(ctx context.Context, deposit *Kit,
	depositProof *proof.Proof, amount uint64, hash lntypes.Hash,
	csvExpiry uint32) (*htlc.SwapKit, *psbt.Packet, []*tappsbt.VPacket,
	[]*tappsbt.VPacket, *assetwalletrpc.CommitVirtualPsbtsResponse, error) {

	// Genearate the HTLC address that will be used to sweep the deposit to
	// in case the client is uncooperative.
	rpcHtlcAddr, swapKit, err := deposit.NewHtlcAddr(
		ctx, s.tapdClient, amount, hash, csvExpiry,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("unable to create "+
			"htlc addr: %v", err)
	}

	htlcAddr, err := address.DecodeAddress(
		rpcHtlcAddr.Encoded, &s.addressParams,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	htlcScriptKey := asset.NewScriptKey(&htlcAddr.ScriptKey)

	// Now we can create the sweep vpacket that'd sweep the deposited
	// assets to the HTLC output.
	depositSpendVpkt, err := assets.CreateOpTrueSweepVpkt(
		ctx, []*proof.Proof{depositProof}, htlcScriptKey,
		&htlcAddr.InternalKey, htlcAddr.TapscriptSibling,
		&s.addressParams,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("unable to create "+
			"deposit spend vpacket: %v", err)
	}

	// By committing the virtual transaction to the BTC template we
	// created, our lnd node will fund the BTC level transaction with an
	// input to pay for the fees. We'll further add a change output to the
	// transaction that will be generated using the above key descriptor.
	feeRate := chainfee.SatPerVByte(0)

	// Use an empty lock ID, as we don't need to lock any UTXOs for this
	// operation.
	lockID := wtxmgr.LockID{}

	htlcBtcPkt, activeAssets, passiveAssets, commitResp, err :=
		s.tapdClient.PrepareAndCommitVirtualPsbts(
			ctx, depositSpendVpkt, feeRate, nil,
			s.addressParams.Params, nil, &lockID,
			time.Duration(0),
		)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("deposit spend "+
			"HTLC prepare and commit failed: %v", err)
	}

	htlcBtcPkt.UnsignedTx.Version = 3

	return swapKit, htlcBtcPkt, activeAssets, passiveAssets, commitResp, nil
}
