// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.25.0

package sqlc

import (
	"database/sql"
	"time"
)

type HtlcKey struct {
	SwapHash               []byte
	SenderScriptPubkey     []byte
	ReceiverScriptPubkey   []byte
	SenderInternalPubkey   []byte
	ReceiverInternalPubkey []byte
	ClientKeyFamily        int32
	ClientKeyIndex         int32
}

type InstantoutSwap struct {
	SwapHash                  []byte
	Preimage                  []byte
	SweepAddress              string
	OutgoingChanSet           string
	HtlcFeeRate               int64
	ReservationIds            []byte
	SwapInvoice               string
	FinalizedHtlcTx           []byte
	SweepTxHash               []byte
	FinalizedSweeplessSweepTx []byte
	SweepConfirmationHeight   sql.NullInt32
}

type InstantoutUpdate struct {
	ID              int32
	SwapHash        []byte
	UpdateState     string
	UpdateTimestamp time.Time
}

type LiquidityParam struct {
	ID     int32
	Params []byte
}

type LoopinSwap struct {
	SwapHash       []byte
	HtlcConfTarget int32
	LastHop        []byte
	ExternalHtlc   bool
}

type LoopoutSwap struct {
	SwapHash            []byte
	DestAddress         string
	SwapInvoice         string
	MaxSwapRoutingFee   int64
	SweepConfTarget     int32
	HtlcConfirmations   int32
	OutgoingChanSet     string
	PrepayInvoice       string
	MaxPrepayRoutingFee int64
	PublicationDeadline time.Time
	SingleSweep         bool
}

type Reservation struct {
	ID                 int32
	ReservationID      []byte
	ClientPubkey       []byte
	ServerPubkey       []byte
	Expiry             int32
	Value              int64
	ClientKeyFamily    int32
	ClientKeyIndex     int32
	InitiationHeight   int32
	TxHash             []byte
	OutIndex           sql.NullInt32
	ConfirmationHeight sql.NullInt32
}

type ReservationUpdate struct {
	ID              int32
	ReservationID   []byte
	UpdateState     string
	UpdateTimestamp time.Time
}

type StaticAddress struct {
	ID              int32
	ClientPubkey    []byte
	ServerPubkey    []byte
	Expiry          int32
	ClientKeyFamily int32
	ClientKeyIndex  int32
	Pkscript        []byte
	ProtocolVersion int32
}

type Swap struct {
	ID               int32
	SwapHash         []byte
	Preimage         []byte
	InitiationTime   time.Time
	AmountRequested  int64
	CltvExpiry       int32
	MaxMinerFee      int64
	MaxSwapFee       int64
	InitiationHeight int32
	ProtocolVersion  int32
	Label            string
}

type SwapUpdate struct {
	ID              int32
	SwapHash        []byte
	UpdateTimestamp time.Time
	UpdateState     int32
	HtlcTxhash      string
	ServerCost      int64
	OnchainCost     int64
	OffchainCost    int64
}

type Sweep struct {
	ID            int32
	SwapHash      []byte
	BatchID       int32
	OutpointTxid  []byte
	OutpointIndex int32
	Amt           int64
	Completed     bool
}

type SweepBatch struct {
	ID                 int32
	Confirmed          bool
	BatchTxID          sql.NullString
	BatchPkScript      []byte
	LastRbfHeight      sql.NullInt32
	LastRbfSatPerKw    sql.NullInt32
	MaxTimeoutDistance int32
}
