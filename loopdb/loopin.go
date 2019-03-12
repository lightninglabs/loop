package loopdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
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

	if err := binary.Write(&b, byteOrder, swap.InitiationTime.UnixNano()); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.Preimage); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.AmountRequested); err != nil {
		return nil, err
	}

	n, err := b.Write(swap.SenderKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("sender key has invalid length")
	}

	n, err = b.Write(swap.ReceiverKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("receiver key has invalid length")
	}

	if err := binary.Write(&b, byteOrder, swap.CltvExpiry); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.MaxMinerFee); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.MaxSwapFee); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.InitiationHeight); err != nil {
		return nil, err
	}

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

	contract := LoopInContract{}
	var err error
	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	contract.InitiationTime = time.Unix(0, unixNano)

	if err := binary.Read(r, byteOrder, &contract.Preimage); err != nil {
		return nil, err
	}

	binary.Read(r, byteOrder, &contract.AmountRequested)

	n, err := r.Read(contract.SenderKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("sender key has invalid length")
	}

	n, err = r.Read(contract.ReceiverKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("receiver key has invalid length")
	}

	if err := binary.Read(r, byteOrder, &contract.CltvExpiry); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &contract.MaxMinerFee); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &contract.MaxSwapFee); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &contract.InitiationHeight); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &contract.HtlcConfTarget); err != nil {
		return nil, err
	}

	var loopInChannel uint64
	if err := binary.Read(r, byteOrder, &loopInChannel); err != nil {
		return nil, err
	}
	if loopInChannel != 0 {
		contract.LoopInChannel = &loopInChannel
	}

	return &contract, nil
}
