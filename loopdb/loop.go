package loopdb

import (
	"bytes"
	"encoding/binary"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapContract contains the base data that is serialized to persistent storage
// for pending swaps.
type SwapContract struct {
	// Preimage is the preimage for the swap.
	Preimage lntypes.Preimage

	// AmountRequested is the total amount of the swap.
	AmountRequested btcutil.Amount

	// SenderKey is the key of the sender that will be used in the on-chain
	// HTLC.
	SenderKey [33]byte

	// ReceiverKey is the of the receiver that will be used in the on-chain
	// HTLC.
	ReceiverKey [33]byte

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
}

// Loop contains fields shared between LoopIn and LoopOut
type Loop struct {
	Hash   lntypes.Hash
	Events []*LoopEvent
}

// LoopEvent contains the dynamic data of a swap.
type LoopEvent struct {
	// State is the new state for this swap as a result of this event.
	State SwapState

	// Time is the time that this swap had its state changed.
	Time time.Time
}

// State returns the most recent state of this swap.
func (s *Loop) State() SwapState {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return StateInitiated
	}

	return lastUpdate.State
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
func serializeLoopEvent(time time.Time, state SwapState) (
	[]byte, error) {

	var b bytes.Buffer

	if err := binary.Write(&b, byteOrder, time.UnixNano()); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, state); err != nil {
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

	return update, nil
}
