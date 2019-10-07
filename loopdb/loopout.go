package loopdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
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

	// PrepayInvoice is the invoice that the client should pay to the
	// server that will be returned if the swap is complete.
	PrepayInvoice string

	// MaxPrepayRoutingFee is the maximum off-chain fee in msat that may be
	// paid for the prepayment to the server.
	MaxPrepayRoutingFee btcutil.Amount
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

func deserializeLoopOutContract(value []byte, chainParams *chaincfg.Params) (
	*LoopOutContract, error) {

	r := bytes.NewReader(value)

	contract := LoopOutContract{}
	var err error
	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	contract.InitiationTime = time.Unix(0, unixNano)

	if err := binary.Read(r, byteOrder, &contract.Preimage); err != nil {
		return nil, err
	}

	err = binary.Read(r, byteOrder, &contract.AmountRequested)
	if err != nil {
		return nil, err
	}

	contract.PrepayInvoice, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

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

	if err := binary.Read(r, byteOrder, &contract.MaxPrepayRoutingFee); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &contract.InitiationHeight); err != nil {
		return nil, err
	}

	addr, err := wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}
	contract.DestAddr, err = btcutil.DecodeAddress(addr, chainParams)
	if err != nil {
		return nil, err
	}

	contract.SwapInvoice, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &contract.SweepConfTarget); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &contract.MaxSwapRoutingFee); err != nil {
		return nil, err
	}

	var unchargeChannel uint64
	if err := binary.Read(r, byteOrder, &unchargeChannel); err != nil {
		return nil, err
	}
	if unchargeChannel != 0 {
		contract.UnchargeChannel = &unchargeChannel
	}

	return &contract, nil
}

func serializeLoopOutContract(swap *LoopOutContract) (
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

	if err := wire.WriteVarString(&b, 0, swap.PrepayInvoice); err != nil {
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

	if err := binary.Write(&b, byteOrder, swap.MaxPrepayRoutingFee); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.InitiationHeight); err != nil {
		return nil, err
	}

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
