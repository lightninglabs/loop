package tapsdk

// ReceiveEventRecord is the raw, protocol-shaped incoming asset transfer event
// returned by Client.SubscribeReceiveEvents. Most application code should
// consume the high-level ReceiveEvent emitted by EventListener instead;
// ReceiveEventRecord is the escape hatch for advanced callers that need every
// daemon field.
type ReceiveEventRecord struct {
	// Timestamp is the event creation time as a Unix timestamp in
	// microseconds.
	Timestamp int64

	// Address is the Taproot Asset address that received the asset.
	Address *Address

	// Outpoint is the outpoint of the on-chain transaction used to
	// receive the asset.
	Outpoint string

	// Status is the current status of the receive event.
	Status AddressEventStatus

	// ConfirmationHeight is the block height at which the receive
	// transaction was confirmed. Only set when Status is
	// AddressEventStatusTransactionConfirmed or later.
	ConfirmationHeight uint32

	// Error is an optional error string indicating that processing
	// the event at the current status failed.
	Error string
}

// ParcelType represents the type of an outbound send parcel.
type ParcelType uint8

const (
	// ParcelTypeAddress is a parcel sent to one or more addresses.
	ParcelTypeAddress ParcelType = 0

	// ParcelTypePreSigned is a parcel created from pre-signed
	// virtual packets.
	ParcelTypePreSigned ParcelType = 1

	// ParcelTypePending is a pending parcel that has not yet been
	// broadcast.
	ParcelTypePending ParcelType = 2

	// ParcelTypePreAnchored is a parcel that was pre-anchored.
	ParcelTypePreAnchored ParcelType = 3
)

// AnchorTransaction contains on-chain anchor transaction details for a
// send event.
type AnchorTransaction struct {
	// AnchorPsbt is the anchor transaction PSBT packet.
	AnchorPsbt []byte

	// ChangeOutputIndex is the index of the change output, or -1 if
	// no change was produced.
	ChangeOutputIndex int32

	// ChainFeesSats is the total on-chain fees paid in satoshis.
	ChainFeesSats int64

	// TargetFeeRate is the target fee rate.
	TargetFeeRate FeeRate

	// LndLockedUtxos lists the UTXO lock leases acquired from lnd.
	LndLockedUtxos []Outpoint

	// FinalTx is the signed anchor transaction that was broadcast.
	FinalTx []byte
}

// SendState is the current state of an outbound send event. tapd
// exposes these as stable string names over the API, so the SDK keeps a
// string-backed type here as well.
type SendState string

const (
	// SendStateStartHandleAddrParcel is the initial send state.
	SendStateStartHandleAddrParcel SendState = "SendStateStartHandleAddrParcel" //nolint:lll

	// SendStateVirtualCommitmentSelect performs input coin selection.
	SendStateVirtualCommitmentSelect SendState = "SendStateVirtualCommitmentSelect" //nolint:lll

	// SendStateVirtualSign creates the asset-level witness data.
	SendStateVirtualSign SendState = "SendStateVirtualSign"

	// SendStateAnchorSign signs and finalizes the anchor PSBT.
	SendStateAnchorSign SendState = "SendStateAnchorSign"

	// SendStateVerifyPreBroadcast runs final pre-broadcast checks.
	SendStateVerifyPreBroadcast SendState = "SendStateVerifyPreBroadcast"

	// SendStateStorePreBroadcast persists the signed transaction
	// before broadcast.
	SendStateStorePreBroadcast SendState = "SendStateStorePreBroadcast"

	// SendStateBroadcast broadcasts the anchor transaction.
	SendStateBroadcast SendState = "SendStateBroadcast"

	// SendStateWaitTxConf waits for the anchor transaction to confirm.
	SendStateWaitTxConf SendState = "SendStateWaitTxConf"

	// SendStateStorePostAnchorTxConf stores post-confirmation state.
	SendStateStorePostAnchorTxConf SendState = "SendStateStorePostAnchorTxConf" //nolint:lll

	// SendStateTransferProofs transfers proofs to the receivers.
	SendStateTransferProofs SendState = "SendStateTransferProofs"

	// SendStateComplete means the transfer has completed fully.
	SendStateComplete SendState = "SendStateComplete"
)

// SendEventRecord is the raw, protocol-shaped outgoing asset transfer event
// returned by Client.SubscribeSendEvents. Most application code should consume
// the high-level SendEvent emitted by EventListener instead; SendEventRecord
// is the escape hatch for advanced callers that need PSBTs, virtual packets,
// or other raw fields.
type SendEventRecord struct {
	// Timestamp is the event execution time as a Unix timestamp in
	// microseconds.
	Timestamp int64

	// SendState is the last send state that was executed
	// successfully. If Error is set, this is the state that caused
	// the failure.
	SendState SendState

	// ParcelType is the type of the outbound parcel.
	ParcelType ParcelType

	// Addresses lists the recipient addresses (not including change
	// back to own wallet). Only set for ParcelTypeAddress.
	Addresses []*Address

	// VirtualPackets are the raw virtual packets in the parcel.
	VirtualPackets [][]byte

	// PassiveVirtualPackets are passive virtual packets carried
	// alongside the active ones when other assets shared the input
	// commitment.
	PassiveVirtualPackets [][]byte

	// AnchorTransaction contains on-chain anchor details. Only set
	// after the anchor signing state.
	AnchorTransaction *AnchorTransaction

	// Transfer is the final transfer record. Only set after the
	// commitment is logged.
	Transfer *AssetTransfer

	// Error is an optional error string.
	Error string

	// TransferLabel is the label assigned to this transfer.
	TransferLabel string

	// NextSendState is the next state that will be executed.
	NextSendState SendState
}

// SendEvent is the high-level, AssetRef-keyed view of an outgoing send event
// emitted by EventListener. Use this for application code that thinks in
// terms of user-facing assets; for raw protocol fields (PSBTs, virtual
// packets, ...) consume Client.SubscribeSendEvents directly to receive
// SendEventRecord.
type SendEvent struct {
	// Timestamp is the event execution time as a Unix timestamp in
	// microseconds.
	Timestamp int64

	// SendState is the last send state that was executed successfully.
	SendState SendState

	// NextSendState is the next state that will be executed.
	NextSendState SendState

	// TransferLabel is the label assigned to this transfer.
	TransferLabel string

	// AssetRefs are the logical asset refs involved in the send event.
	AssetRefs []AssetRef

	// Transfer is the final transfer when the event contains one.
	Transfer *Transfer

	// Error is an optional error string.
	Error string
}

// ReceiveEvent is the high-level, AssetRef-keyed view of an incoming receive
// event emitted by EventListener. Use this for application code that thinks
// in terms of user-facing assets; for raw protocol fields consume
// Client.SubscribeReceiveEvents directly to receive ReceiveEventRecord.
type ReceiveEvent struct {
	// Timestamp is the event creation time as a Unix timestamp in
	// microseconds.
	Timestamp int64

	// AssetRef is the SDK identifier from the receiving address.
	AssetRef AssetRef

	// Amount is the address amount when the address embeds one.
	Amount uint64

	// Status is the current status of the receive event.
	Status AddressEventStatus

	// Outpoint is the outpoint of the on-chain receive transaction.
	Outpoint string

	// ConfirmationHeight is the block height at which the receive
	// transaction was confirmed.
	ConfirmationHeight uint32

	// Error is an optional error string.
	Error string
}

// MintEvent represents a minting batch event.
type MintEvent struct {
	// Timestamp is the event execution time as a Unix timestamp in
	// microseconds.
	Timestamp int64

	// BatchState is the last batch state that was executed. If Error
	// is set, this is the state that caused the failure.
	BatchState BatchState

	// Batch contains the minting batch details.
	Batch *MintingBatch

	// Error is an optional error string.
	Error string
}

// SubscribeReceiveEventsRequest contains parameters for subscribing to
// incoming transfer events.
type SubscribeReceiveEventsRequest struct {
	// FilterAddr restricts events to a specific Taproot Asset
	// address. Leave empty to receive all events.
	FilterAddr string

	// StartTimestamp is the start time as a Unix timestamp in
	// microseconds. If zero, the daemon streams from the current
	// time.
	StartTimestamp int64
}

// SubscribeSendEventsRequest contains parameters for subscribing to
// outgoing transfer events.
type SubscribeSendEventsRequest struct {
	// FilterScriptKey restricts events to a specific recipient
	// script key. Leave empty to receive all send events.
	FilterScriptKey []byte

	// FilterLabel restricts events to a specific transfer label.
	// Leave empty to not filter by label.
	FilterLabel string

	// StartTimestamp is the start time as a Unix timestamp in
	// microseconds. If zero, the daemon streams from the current
	// time.
	StartTimestamp int64
}

// SubscribeMintEventsRequest contains parameters for subscribing to
// mint batch events.
type SubscribeMintEventsRequest struct {
	// ShortResponse omits the full asset list from batch events
	// to reduce data volume for large batches.
	ShortResponse bool
}

// NewSendEvent projects a raw SendEventRecord into the high-level SendEvent.
//
// AssetRefs is built from the final transfer's inputs and outputs when the
// transfer is available. Before tapd logs the transfer, recipient addresses
// provide the best available refs. The final Transfer, when set on the record,
// is rebuilt with AssetRef-keyed inputs and outputs.
//
// Returns nil when record is nil.
func NewSendEvent(record *SendEventRecord) *SendEvent {
	if record == nil {
		return nil
	}

	event := &SendEvent{
		Timestamp:     record.Timestamp,
		SendState:     record.SendState,
		NextSendState: record.NextSendState,
		TransferLabel: record.TransferLabel,
		Error:         record.Error,
	}

	if record.Transfer != nil {
		event.Transfer = NewTransfer(record.Transfer)
		for _, input := range event.Transfer.Inputs {
			event.AssetRefs = appendUniqueAssetRef(
				event.AssetRefs, input.AssetRef,
			)
		}
		for _, output := range event.Transfer.Outputs {
			event.AssetRefs = appendUniqueAssetRef(
				event.AssetRefs, output.AssetRef,
			)
		}

		if len(event.AssetRefs) > 0 {
			return event
		}
	}

	for _, addr := range record.Addresses {
		if addr == nil || addr.AssetRef.IsZero() {
			continue
		}

		event.AssetRefs = appendUniqueAssetRef(
			event.AssetRefs, addr.AssetRef,
		)
	}

	return event
}

// NewReceiveEvent projects a raw ReceiveEventRecord into the high-level
// ReceiveEvent. Returns nil when record is nil.
func NewReceiveEvent(record *ReceiveEventRecord) *ReceiveEvent {
	if record == nil {
		return nil
	}

	event := &ReceiveEvent{
		Timestamp:          record.Timestamp,
		Status:             record.Status,
		Outpoint:           record.Outpoint,
		ConfirmationHeight: record.ConfirmationHeight,
		Error:              record.Error,
	}

	if record.Address != nil {
		event.AssetRef = record.Address.AssetRef
		event.Amount = record.Address.Amount
	}

	return event
}

func appendUniqueAssetRef(refs []AssetRef, ref AssetRef) []AssetRef {
	for _, existing := range refs {
		if existing.Equivalent(ref) {
			return refs
		}
	}

	return append(refs, ref)
}
