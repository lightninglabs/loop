package loopdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
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

	// HtlcConfirmations is the number of confirmations we require the on
	// chain htlc to have before proceeding with the swap.
	HtlcConfirmations uint32

	// OutgoingChanSet is the set of short ids of channels that may be used.
	// If empty, any channel may be used.
	OutgoingChanSet ChannelSet

	// PrepayInvoice is the invoice that the client should pay to the
	// server that will be returned if the swap is complete.
	PrepayInvoice string

	// MaxPrepayRoutingFee is the maximum off-chain fee in msat that may be
	// paid for the prepayment to the server.
	MaxPrepayRoutingFee btcutil.Amount

	// SwapPublicationDeadline is a timestamp that the server commits to
	// have the on-chain swap published by. It is set by the client to
	// allow the server to delay the publication in exchange for possibly
	// lower fees.
	SwapPublicationDeadline time.Time
}

// ChannelSet stores a set of channels.
type ChannelSet []uint64

// String returns the human-readable representation of a channel set.
func (c ChannelSet) String() string {
	channelStrings := make([]string, len(c))
	for i, chanID := range c {
		channelStrings[i] = strconv.FormatUint(chanID, 10)
	}
	return strings.Join(channelStrings, ",")
}

// NewChannelSet instantiates a new channel set and verifies that there are no
// duplicates present.
func NewChannelSet(set []uint64) (ChannelSet, error) {
	// Check channel set for duplicates.
	chanSet := make(map[uint64]struct{})
	for _, chanID := range set {
		if _, exists := chanSet[chanID]; exists {
			return nil, fmt.Errorf("duplicate chan in set: id=%v",
				chanID)
		}
		chanSet[chanID] = struct{}{}
	}

	return ChannelSet(set), nil
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

	n, err := r.Read(contract.HtlcKeys.SenderScriptKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("sender key has invalid length")
	}

	n, err = r.Read(contract.HtlcKeys.ReceiverScriptKey[:])
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
		contract.OutgoingChanSet = ChannelSet{unchargeChannel}
	}

	var deadlineNano int64
	err = binary.Read(r, byteOrder, &deadlineNano)
	if err != nil {
		return nil, err
	}
	contract.SwapPublicationDeadline = time.Unix(0, deadlineNano)

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

	n, err := b.Write(swap.HtlcKeys.SenderScriptKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("sender key has invalid length")
	}

	n, err = b.Write(swap.HtlcKeys.ReceiverScriptKey[:])
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

	// Always write no outgoing channel. This field is replaced by an
	// outgoing channel set.
	unchargeChannel := uint64(0)
	if err := binary.Write(&b, byteOrder, unchargeChannel); err != nil {
		return nil, err
	}

	err = binary.Write(&b, byteOrder, swap.SwapPublicationDeadline.UnixNano())
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}
