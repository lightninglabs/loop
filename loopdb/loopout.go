package loopdb

import (
	"bytes"
	"encoding/binary"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// LoopOutContract contains the data that is serialized to persistent storage
// for pending swaps.
type LoopOutContract struct {
	// SwapContract contains basic information pertaining to this swap.
	// Each swap type has a base contract, then swap specific information
	// on top of it.
	SwapContract

	// DestAddr is the destination address of the loop out swap.
	DestAddr btcutil.Address

	// SwapInvoice is the invoice that is to be paid by the client to
	// initiate the loop out swap.
	SwapInvoice string

	// MaxSwapRoutingFee is the maximum off-chain fee in msat that may be
	// paid for the swap payment to the server.
	MaxSwapRoutingFee btcutil.Amount

	// SweepConfTarget specifies the targeted confirmation target for the
	// client sweep tx.
	SweepConfTarget int32

	// TargetChannel is the channel to loop out. If zero, any channel may
	// be used.
	UnchargeChannel *uint64
}

// LoopOut is a combination of the contract and the updates.
type LoopOut struct {
	Loop

	// Contract is the active contract for this swap. It describes the
	// precise details of the swap including the final fee, CLTV value,
	// etc.
	Contract *LoopOutContract
}

// LastUpdateTime returns the last update time of this swap.
func (s *LoopOut) LastUpdateTime() time.Time {
	lastUpdate := s.LastUpdate()
	if lastUpdate == nil {
		return s.Contract.InitiationTime
	}

	return lastUpdate.Time
}

func deserializeLoopOutContract(value []byte) (*LoopOutContract, error) {
	r := bytes.NewReader(value)

	contract, err := deserializeContract(r)
	if err != nil {
		return nil, err
	}

	swap := LoopOutContract{
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

func serializeLoopOutContract(swap *LoopOutContract) (
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
