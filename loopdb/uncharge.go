package loopdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
)

// UnchargeContract contains the data that is serialized to persistent storage
// for pending swaps.
type UnchargeContract struct {
	SwapContract

	DestAddr btcutil.Address

	SwapInvoice string

	// MaxSwapRoutingFee is the maximum off-chain fee in msat that may be
	// paid for the swap payment to the server.
	MaxSwapRoutingFee btcutil.Amount

	// SweepConfTarget specifies the targeted confirmation target for the
	// client sweep tx.
	SweepConfTarget int32

	// UnchargeChannel is the channel to uncharge. If zero, any channel may
	// be used.
	UnchargeChannel *uint64
}

// PersistentUnchargeEvent contains the dynamic data of a swap.
type PersistentUnchargeEvent struct {
	// State is the new state for this swap as a result of this event.
	State SwapState

	// Time is the time that this swap had its state changed.
	Time time.Time
}

// PersistentUncharge is a combination of the contract and the updates.
type PersistentUncharge struct {
	// Hash is the hash that uniquely identifies this swap.
	Hash lntypes.Hash

	// Contract is the active contract for this swap. It describes the
	// precise details of the swap including the final fee, CLTV value,
	// etc.
	Contract *UnchargeContract

	// Events are each of the state transitions that this swap underwent.
	Events []*PersistentUnchargeEvent
}

// State returns the most recent state of this swap.
func (s *PersistentUncharge) State() SwapState {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return StateInitiated
	}

	return lastUpdate.State
}

// LastUpdate returns the most recent update of this swap.
func (s *PersistentUncharge) LastUpdate() *PersistentUnchargeEvent {
	eventCount := len(s.Events)

	if eventCount == 0 {
		return nil
	}

	lastEvent := s.Events[eventCount-1]
	return lastEvent
}

// LastUpdateTime returns the last update time of this swap.
func (s *PersistentUncharge) LastUpdateTime() time.Time {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return s.Contract.InitiationTime
	}

	return lastUpdate.Time
}

func deserializeUnchargeContract(value []byte) (*UnchargeContract, error) {
	r := bytes.NewReader(value)

	contract, err := deserializeContract(r)
	if err != nil {
		return nil, err
	}

	swap := UnchargeContract{
		SwapContract: *contract,
	}

	addr, err := wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}
	swap.DestAddr, err = btcutil.DecodeAddress(addr, nil)
	if err != nil {
		return nil, err
	}

	swap.SwapInvoice, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.SweepConfTarget); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.MaxSwapRoutingFee); err != nil {
		return nil, err
	}

	var unchargeChannel uint64
	if err := binary.Read(r, byteOrder, &unchargeChannel); err != nil {
		return nil, err
	}
	if unchargeChannel != 0 {
		swap.UnchargeChannel = &unchargeChannel
	}

	return &swap, nil
}

func serializeUnchargeContract(swap *UnchargeContract) (
	[]byte, error) {

	var b bytes.Buffer

	serializeContract(&swap.SwapContract, &b)

	addr := swap.DestAddr.String()
	if err := wire.WriteVarString(&b, 0, addr); err != nil {
		return nil, err
	}

	if err := wire.WriteVarString(&b, 0, swap.SwapInvoice); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.SweepConfTarget); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.MaxSwapRoutingFee); err != nil {
		return nil, err
	}

	var unchargeChannel uint64
	if swap.UnchargeChannel != nil {
		unchargeChannel = *swap.UnchargeChannel
	}
	if err := binary.Write(&b, byteOrder, unchargeChannel); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func deserializeContract(r io.Reader) (*SwapContract, error) {
	swap := SwapContract{}
	var err error
	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	swap.InitiationTime = time.Unix(0, unixNano)

	if err := binary.Read(r, byteOrder, &swap.Preimage); err != nil {
		return nil, err
	}

	binary.Read(r, byteOrder, &swap.AmountRequested)

	swap.PrepayInvoice, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	n, err := r.Read(swap.SenderKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("sender key has invalid length")
	}

	n, err = r.Read(swap.ReceiverKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("receiver key has invalid length")
	}

	if err := binary.Read(r, byteOrder, &swap.CltvExpiry); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &swap.MaxMinerFee); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.MaxSwapFee); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.MaxPrepayRoutingFee); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &swap.InitiationHeight); err != nil {
		return nil, err
	}

	return &swap, nil
}

func serializeContract(swap *SwapContract, b *bytes.Buffer) error {
	if err := binary.Write(b, byteOrder, swap.InitiationTime.UnixNano()); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.Preimage); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.AmountRequested); err != nil {
		return err
	}

	if err := wire.WriteVarString(b, 0, swap.PrepayInvoice); err != nil {
		return err
	}

	n, err := b.Write(swap.SenderKey[:])
	if err != nil {
		return err
	}
	if n != keyLength {
		return fmt.Errorf("sender key has invalid length")
	}

	n, err = b.Write(swap.ReceiverKey[:])
	if err != nil {
		return err
	}
	if n != keyLength {
		return fmt.Errorf("receiver key has invalid length")
	}

	if err := binary.Write(b, byteOrder, swap.CltvExpiry); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.MaxMinerFee); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.MaxSwapFee); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.MaxPrepayRoutingFee); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.InitiationHeight); err != nil {
		return err
	}

	return nil
}

func serializeUnchargeUpdate(time time.Time, state SwapState) (
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

func deserializeUnchargeUpdate(value []byte) (*PersistentUnchargeEvent, error) {
	update := &PersistentUnchargeEvent{}

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
