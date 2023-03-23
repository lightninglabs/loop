package loopdb

import (
	"bytes"
	"encoding/binary"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// HtlcKeys is a holder of all keys used when constructing the swap HTLC. Since
// it's used for both loop in and loop out swaps it may hold partial information
// about the sender or receiver depending on the swap type.
type HtlcKeys struct {
	// SenderScriptKey is the sender's public key that is used in the HTLC,
	// specifically when constructing the script spend scripts.
	SenderScriptKey [33]byte

	// SenderInternalPubKey is the sender's internal pubkey that is used in
	// taproot HTLCs as part of the aggregate internal key.
	SenderInternalPubKey [33]byte

	// ReceiverScriptKey is the receiver's public key that is used in the
	// HTLC, specifically when constructing the script spend scripts.
	ReceiverScriptKey [33]byte

	// ReceiverInternalPubKey is the sender's internal pubkey that is used
	// in taproot HTLCs as part of the aggregate internal key.
	ReceiverInternalPubKey [33]byte

	// ClientScriptKeyLocator is the client's key locator for the key used
	// in the HTLC script spend scripts.
	ClientScriptKeyLocator keychain.KeyLocator
}

// SwapContract contains the base data that is serialized to persistent storage
// for pending swaps.
type SwapContract struct {
	// Preimage is the preimage for the swap.
	Preimage lntypes.Preimage

	// AmountRequested is the total amount of the swap.
	AmountRequested btcutil.Amount

	// HtlcKeys holds all keys used in the swap HTLC construction.
	HtlcKeys HtlcKeys

	// CltvExpiry is the total absolute CLTV expiry of the swap.
	CltvExpiry int32

	// MaxSwapFee is the maximum we are willing to pay the server for the
	// swap.
	MaxSwapFee btcutil.Amount

	// MaxMinerFee is the maximum in on-chain fees that we are willing to
	// spend.
	MaxMinerFee btcutil.Amount

	// InitiationHeight is the block height at which the swap was
	// initiated.
	InitiationHeight int32

	// InitiationTime is the time at which the swap was initiated.
	InitiationTime time.Time

	// Label contains an optional label for the swap.
	Label string

	// ProtocolVersion stores the protocol version when the swap was
	// created.
	ProtocolVersion ProtocolVersion
}

// Loop contains fields shared between LoopIn and LoopOut.
type Loop struct {
	Hash   lntypes.Hash
	Events []*LoopEvent
}

// LoopEvent contains the dynamic data of a swap.
type LoopEvent struct {
	SwapStateData

	// Time is the time that this swap had its state changed.
	Time time.Time
}

// State returns the most recent state of this swap.
func (s *Loop) State() SwapStateData {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return SwapStateData{
			State: StateInitiated,
		}
	}

	return lastUpdate.SwapStateData
}

// LastUpdate returns the most recent update of this swap.
func (s *Loop) LastUpdate() *LoopEvent {
	eventCount := len(s.Events)

	if eventCount == 0 {
		return nil
	}

	lastEvent := s.Events[eventCount-1]
	return lastEvent
}

// serializeLoopEvent serializes a state update of a swap. This is used for both
// in and out swaps.
func serializeLoopEvent(time time.Time, state SwapStateData) (
	[]byte, error) {

	var b bytes.Buffer

	if err := binary.Write(&b, byteOrder, time.UnixNano()); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, state.State); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, state.Cost.Server); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, state.Cost.Onchain); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, state.Cost.Offchain); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// deserializeLoopEvent deserializes a state update of a swap. This is used for
// both in and out swaps.
func deserializeLoopEvent(value []byte) (*LoopEvent, error) {
	update := &LoopEvent{}

	r := bytes.NewReader(value)

	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	update.Time = time.Unix(0, unixNano)

	if err := binary.Read(r, byteOrder, &update.State); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &update.Cost.Server); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &update.Cost.Onchain); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &update.Cost.Offchain); err != nil {
		return nil, err
	}

	return update, nil
}
