package rest

import (
	"fmt"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
)

// parseParcelType converts the proto enum string to tapsdk.ParcelType.
func parseParcelType(s string) tapsdk.ParcelType {
	switch s {
	case "PARCEL_TYPE_PRE_SIGNED":
		return tapsdk.ParcelTypePreSigned
	case "PARCEL_TYPE_PENDING":
		return tapsdk.ParcelTypePending
	case "PARCEL_TYPE_PRE_ANCHORED":
		return tapsdk.ParcelTypePreAnchored
	default:
		return tapsdk.ParcelTypeAddress
	}
}

// unmarshalReceiveEvent converts a JSON receive event to the raw SDK record.
func unmarshalReceiveEvent(
	e *jsonReceiveEvent) (*tapsdk.ReceiveEventRecord, error) {

	if e == nil {
		return nil, fmt.Errorf("nil receive event")
	}

	ts, err := parseInt64(e.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}

	event := &tapsdk.ReceiveEventRecord{
		Timestamp:          ts,
		Outpoint:           e.Outpoint,
		Status:             parseAddressEventStatus(e.Status),
		ConfirmationHeight: e.ConfirmationHeight,
		Error:              e.Error,
	}

	if e.Address != nil {
		addr, err := unmarshalAddr(e.Address)
		if err != nil {
			return nil, fmt.Errorf("receive event "+
				"address: %w", err)
		}

		event.Address = addr
	}

	return event, nil
}

// unmarshalSendEvent converts a JSON send event to the raw SDK record.
func unmarshalSendEvent(
	e *jsonSendEvent) (*tapsdk.SendEventRecord, error) {

	if e == nil {
		return nil, fmt.Errorf("nil send event")
	}

	ts, err := parseInt64(e.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}

	event := &tapsdk.SendEventRecord{
		Timestamp:     ts,
		SendState:     tapsdk.SendState(e.SendState),
		ParcelType:    parseParcelType(e.ParcelType),
		Error:         e.Error,
		TransferLabel: e.TransferLabel,
		NextSendState: tapsdk.SendState(e.NextSendState),
	}

	for _, a := range e.Addresses {
		addr, err := unmarshalAddr(a)
		if err != nil {
			return nil, fmt.Errorf("send event "+
				"address: %w", err)
		}

		event.Addresses = append(event.Addresses, addr)
	}

	if len(e.VirtualPackets) > 0 {
		packets, err := decodeHexSlice(e.VirtualPackets)
		if err != nil {
			return nil, fmt.Errorf("virtual packets: %w", err)
		}

		event.VirtualPackets = packets
	}

	if len(e.PassiveVirtualPackets) > 0 {
		packets, err := decodeHexSlice(e.PassiveVirtualPackets)
		if err != nil {
			return nil, fmt.Errorf("passive virtual "+
				"packets: %w", err)
		}

		event.PassiveVirtualPackets = packets
	}

	if e.AnchorTransaction != nil {
		anchor, err := unmarshalAnchorTransaction(e.AnchorTransaction)
		if err != nil {
			return nil, fmt.Errorf("anchor "+
				"transaction: %w", err)
		}

		event.AnchorTransaction = anchor
	}

	if e.Transfer != nil {
		transfer, err := unmarshalAssetTransfer(e.Transfer)
		if err != nil {
			return nil, fmt.Errorf("transfer: %w", err)
		}

		event.Transfer = transfer
	}

	return event, nil
}

// unmarshalAnchorTransaction converts a JSON AnchorTransaction to the
// SDK type.
func unmarshalAnchorTransaction(
	a *jsonAnchorTransaction) (*tapsdk.AnchorTransaction, error) {

	if a == nil {
		return nil, fmt.Errorf("nil anchor transaction")
	}

	anchorPsbt, err := parseHexBytes(a.AnchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("invalid anchor_psbt: %w", err)
	}

	finalTx, err := parseHexBytes(a.FinalTx)
	if err != nil {
		return nil, fmt.Errorf("invalid final_tx: %w", err)
	}

	chainFees, err := parseInt64(a.ChainFeesSats)
	if err != nil {
		return nil, fmt.Errorf("invalid chain_fees_sats: %w", err)
	}

	tx := &tapsdk.AnchorTransaction{
		AnchorPsbt:        anchorPsbt,
		ChangeOutputIndex: a.ChangeOutputIndex,
		ChainFeesSats:     chainFees,
		TargetFeeRate: feeRateFromSatPerKWeight(
			a.TargetFeeRateSatKw,
		),
		FinalTx: finalTx,
	}

	for _, op := range a.LndLockedUtxos {
		if op == nil {
			continue
		}

		outpoint, err := unmarshalAnchorOutpoint(op)
		if err != nil {
			return nil, fmt.Errorf("locked utxo: %w", err)
		}

		tx.LndLockedUtxos = append(tx.LndLockedUtxos, outpoint)
	}

	return tx, nil
}

// unmarshalAnchorOutpoint converts the binary-txid jsonOutpoint (as
// used by taprpc.OutPoint) into an tapsdk.Outpoint.
func unmarshalAnchorOutpoint(op *jsonOutpoint) (tapsdk.Outpoint, error) {
	var out tapsdk.Outpoint

	txid, err := parseHexBytes(op.Txid)
	if err != nil {
		return out, fmt.Errorf("invalid txid: %w", err)
	}
	if len(txid) != 32 {
		return out, fmt.Errorf("invalid txid length: %d",
			len(txid))
	}

	copy(out.Txid[:], txid)
	out.Index = op.OutputIndex

	return out, nil
}

// unmarshalMintEvent converts a JSON mint event to the SDK type.
func unmarshalMintEvent(
	e *jsonMintEvent) (*tapsdk.MintEvent, error) {

	if e == nil {
		return nil, fmt.Errorf("nil mint event")
	}

	ts, err := parseInt64(e.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}

	event := &tapsdk.MintEvent{
		Timestamp:  ts,
		BatchState: parseBatchState(e.BatchState),
		Error:      e.Error,
	}

	if e.Batch != nil {
		batch, err := unmarshalMintingBatch(e.Batch)
		if err != nil {
			return nil, fmt.Errorf("batch: %w", err)
		}

		event.Batch = batch
	}

	return event, nil
}

// decodeHexSlice decodes a slice of hex strings into a slice of byte
// slices.
func decodeHexSlice(in []string) ([][]byte, error) {
	out := make([][]byte, 0, len(in))
	for _, s := range in {
		b, err := parseHexBytes(s)
		if err != nil {
			return nil, err
		}

		out = append(out, b)
	}

	return out, nil
}
