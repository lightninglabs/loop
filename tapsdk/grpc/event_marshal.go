package grpc

import (
	"fmt"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
)

// unmarshalReceiveEvent converts an RPC ReceiveEvent to the raw SDK record.
func unmarshalReceiveEvent(
	rpcEvent *taprpc.ReceiveEvent) (*tapsdk.ReceiveEventRecord, error) {

	if rpcEvent == nil {
		return nil, fmt.Errorf("nil receive event")
	}

	event := &tapsdk.ReceiveEventRecord{
		Timestamp:          rpcEvent.Timestamp,
		Outpoint:           rpcEvent.Outpoint,
		Status:             tapsdk.AddressEventStatus(rpcEvent.Status),
		ConfirmationHeight: rpcEvent.ConfirmationHeight,
		Error:              rpcEvent.Error,
	}

	if rpcEvent.Address != nil {
		addr, err := unmarshalAddr(rpcEvent.Address)
		if err != nil {
			return nil, fmt.Errorf("receive event "+
				"address: %w", err)
		}
		event.Address = addr
	}

	return event, nil
}

// unmarshalSendEvent converts an RPC SendEvent to the raw SDK record.
func unmarshalSendEvent(
	rpcEvent *taprpc.SendEvent) (*tapsdk.SendEventRecord, error) {

	if rpcEvent == nil {
		return nil, fmt.Errorf("nil send event")
	}

	event := &tapsdk.SendEventRecord{
		Timestamp:     rpcEvent.Timestamp,
		SendState:     tapsdk.SendState(rpcEvent.SendState),
		ParcelType:    tapsdk.ParcelType(rpcEvent.ParcelType),
		Error:         rpcEvent.Error,
		TransferLabel: rpcEvent.TransferLabel,
		NextSendState: tapsdk.SendState(rpcEvent.NextSendState),
	}

	// Unmarshal recipient addresses.
	for _, rpcAddr := range rpcEvent.Addresses {
		addr, err := unmarshalAddr(rpcAddr)
		if err != nil {
			return nil, fmt.Errorf("send event "+
				"address: %w", err)
		}
		event.Addresses = append(event.Addresses, addr)
	}

	// Copy raw virtual packet bytes.
	if len(rpcEvent.VirtualPackets) > 0 {
		event.VirtualPackets = make(
			[][]byte, len(rpcEvent.VirtualPackets),
		)
		copy(event.VirtualPackets, rpcEvent.VirtualPackets)
	}

	if len(rpcEvent.PassiveVirtualPackets) > 0 {
		event.PassiveVirtualPackets = make(
			[][]byte, len(rpcEvent.PassiveVirtualPackets),
		)
		copy(
			event.PassiveVirtualPackets,
			rpcEvent.PassiveVirtualPackets,
		)
	}

	// Unmarshal anchor transaction if present.
	if rpcEvent.AnchorTransaction != nil {
		anchor, err := unmarshalAnchorTransaction(
			rpcEvent.AnchorTransaction,
		)
		if err != nil {
			return nil, fmt.Errorf("send event "+
				"anchor tx: %w", err)
		}
		event.AnchorTransaction = anchor
	}

	// Unmarshal transfer if present.
	if rpcEvent.Transfer != nil {
		transfer, err := unmarshalAssetTransfer(rpcEvent.Transfer)
		if err != nil {
			return nil, fmt.Errorf("send event "+
				"transfer: %w", err)
		}
		event.Transfer = transfer
	}

	return event, nil
}

// unmarshalAnchorTransaction converts an RPC AnchorTransaction to the
// SDK type.
func unmarshalAnchorTransaction(
	rpcTx *taprpc.AnchorTransaction) (*tapsdk.AnchorTransaction,
	error) {

	if rpcTx == nil {
		return nil, fmt.Errorf("nil anchor transaction")
	}

	tx := &tapsdk.AnchorTransaction{
		AnchorPsbt:        rpcTx.AnchorPsbt,
		ChangeOutputIndex: rpcTx.ChangeOutputIndex,
		ChainFeesSats:     rpcTx.ChainFeesSats,
		TargetFeeRate: feeRateFromSatPerKWeight(
			rpcTx.TargetFeeRateSatKw,
		),
		FinalTx: rpcTx.FinalTx,
	}

	for _, rpcOp := range rpcTx.LndLockedUtxos {
		if rpcOp == nil {
			continue
		}

		op, err := unmarshalOutPoint(rpcOp)
		if err != nil {
			return nil, fmt.Errorf("locked utxo: %w", err)
		}
		tx.LndLockedUtxos = append(tx.LndLockedUtxos, op)
	}

	return tx, nil
}

// unmarshalOutPoint converts an RPC OutPoint to an SDK Outpoint.
func unmarshalOutPoint(
	rpcOp *taprpc.OutPoint) (tapsdk.Outpoint, error) {

	if rpcOp == nil {
		return tapsdk.Outpoint{}, fmt.Errorf("nil outpoint")
	}

	var op tapsdk.Outpoint
	if len(rpcOp.Txid) != 32 {
		return op, fmt.Errorf("invalid txid length: %d",
			len(rpcOp.Txid))
	}
	copy(op.Txid[:], rpcOp.Txid)
	op.Index = rpcOp.OutputIndex

	return op, nil
}

// unmarshalMintEvent converts an RPC MintEvent to the SDK type.
func unmarshalMintEvent(
	rpcEvent *mintrpc.MintEvent) (*tapsdk.MintEvent, error) {

	if rpcEvent == nil {
		return nil, fmt.Errorf("nil mint event")
	}

	event := &tapsdk.MintEvent{
		Timestamp:  rpcEvent.Timestamp,
		BatchState: tapsdk.BatchState(rpcEvent.BatchState),
		Error:      rpcEvent.Error,
	}

	if rpcEvent.Batch != nil {
		batch, err := unmarshalMintingBatch(rpcEvent.Batch)
		if err != nil {
			return nil, fmt.Errorf("mint event "+
				"batch: %w", err)
		}
		event.Batch = batch
	}

	return event, nil
}
