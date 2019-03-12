package loopdb

import (
	"bytes"
	"encoding/binary"
	"time"
)

// LoopInContract contains the data that is serialized to persistent storage for
// pending loop in swaps.
type LoopInContract struct {
	SwapContract

	// SweepConfTarget specifies the targeted confirmation target for the
	// client sweep tx.
	HtlcConfTarget int32

	// LoopInChannel is the channel to charge. If zero, any channel may
	// be used.
	LoopInChannel *uint64
}

// LoopIn is a combination of the contract and the updates.
type LoopIn struct {
	Loop

	Contract *LoopInContract
}

// LastUpdateTime returns the last update time of this swap.
func (s *LoopIn) LastUpdateTime() time.Time {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return s.Contract.InitiationTime
	}

	return lastUpdate.Time
}

// serializeLoopInContract serialize the loop in contract into a byte slice.
func serializeLoopInContract(swap *LoopInContract) (
	[]byte, error) {

	var b bytes.Buffer

	serializeContract(&swap.SwapContract, &b)

	if err := binary.Write(&b, byteOrder, swap.HtlcConfTarget); err != nil {
		return nil, err
	}

	var chargeChannel uint64
	if swap.LoopInChannel != nil {
		chargeChannel = *swap.LoopInChannel
	}
	if err := binary.Write(&b, byteOrder, chargeChannel); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// deserializeLoopInContract deserializes the loop in contract from a byte slice.
func deserializeLoopInContract(value []byte) (*LoopInContract, error) {
	r := bytes.NewReader(value)

	contract, err := deserializeContract(r)
	if err != nil {
		return nil, err
	}

	swap := LoopInContract{
		SwapContract: *contract,
	}

	if err := binary.Read(r, byteOrder, &swap.HtlcConfTarget); err != nil {
		return nil, err
	}

	var loopInChannel uint64
	if err := binary.Read(r, byteOrder, &loopInChannel); err != nil {
		return nil, err
	}
	if loopInChannel != 0 {
		swap.LoopInChannel = &loopInChannel
	}

	return &swap, nil
}
