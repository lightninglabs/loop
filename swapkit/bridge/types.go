// Package bridge defines the versioned process boundary between Loop's
// daemons and the SDK transaction runtime. Only primitive protocol data
// crosses this boundary; private keys and signing nonces never do.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Version identifies the serialized process protocol.
const Version uint32 = 1

// Method names are an explicit capability whitelist for the local runtime.
type Method string

const (
	CheckReady             Method = "CheckReady"
	Height                 Method = "Height"
	VerifyAdmission        Method = "VerifyAdmission"
	ValidateDeposits       Method = "ValidateDeposits"
	ValidateInvoice        Method = "ValidateInvoice"
	BuildFallback          Method = "BuildFallback"
	VerifyClaim            Method = "VerifyClaim"
	ValidateExecution      Method = "ValidateExecution"
	RevealPreimage         Method = "RevealPreimage"
	RecoverClaim           Method = "RecoverClaim"
	InspectRefund          Method = "InspectRefund"
	StoreRefundProof       Method = "StoreRefundProof"
	Inventory              Method = "Inventory"
	CreateDeposit          Method = "CreateDeposit"
	RefundDeposit          Method = "RefundDeposit"
	PrepareRecovery        Method = "PrepareRecovery"
	ValidateOffer          Method = "ValidateOffer"
	VerifyFinalizeFallback Method = "VerifyFinalizeFallback"
	LookupPreimage         Method = "LookupPreimage"
	SignCooperative        Method = "SignCooperative"
	InspectClaim           Method = "InspectClaim"
	RecoverRefund          Method = "RecoverRefund"
)

var (
	ErrInvalid = errors.New("invalid asset bridge request")
	ErrPending = errors.New("asset bridge operation pending")
	ErrRemote  = errors.New("asset bridge operation failed")
)

// Terms matches the durable coordinator's public terms. Admission is private
// server recovery data containing authenticated registration and input proofs.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Terms struct {
	Version          uint32    `json:"Version"`
	RequestID        [32]byte  `json:"RequestID"`
	RequestDigest    [32]byte  `json:"RequestDigest"`
	Owner            [32]byte  `json:"Owner"`
	GroupKey         [33]byte  `json:"GroupKey"`
	DepositOutpoints []string  `json:"DepositOutpoints"`
	DepositAmount    uint64    `json:"DepositAmount"`
	PayoutAmount     uint64    `json:"PayoutAmount"`
	AssetFee         uint64    `json:"AssetFee"`
	ChangeAmount     uint64    `json:"ChangeAmount"`
	MaxDebit         uint64    `json:"MaxDebit"`
	RefundHeight     uint32    `json:"RefundHeight"`
	MinPayoutExpiry  uint32    `json:"MinPayoutExpiry"`
	RecoveryDelta    uint32    `json:"RecoveryDelta"`
	ExecutionDelta   uint32    `json:"ExecutionDelta"`
	OfferExpiry      time.Time `json:"OfferExpiry"`
	Contract         []byte    `json:"Contract"`
	Admission        []byte    `json:",omitempty"`
}

// Offer commits to the server payment hash and exact native asset terms.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Offer struct {
	Hash  [32]byte `json:"Hash"`
	Terms Terms    `json:"Terms"`
}

// Registration is loaded from the authenticated owner's static address row.
// The runtime must bind every deposit policy to these exact keys and CSV.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Registration struct {
	Owner     [32]byte `json:"Owner"`
	ClientKey [33]byte `json:"ClientKey"`
	ServerKey [33]byte `json:"ServerKey"`
	CSV       uint32   `json:"CSV"`
	KeyFamily int32    `json:"KeyFamily"`
	KeyIndex  uint32   `json:"KeyIndex"`
}

// Deposit preserves the proof and contract needed after process restart.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Deposit struct {
	Outpoint           string   `json:"Outpoint"`
	GroupKey           [33]byte `json:"GroupKey"`
	Amount             uint64   `json:"Amount"`
	Proof              []byte   `json:"Proof"`
	Contract           []byte   `json:"Contract"`
	ConfirmationHeight uint32   `json:"ConfirmationHeight"`
	RefundHeight       uint32   `json:"RefundHeight"`
	ValueSat           uint64   `json:"ValueSat"`
}

// Admission binds client-supplied proofs to server-authenticated registration
// and server policy before the execution journal reserves any deposit.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Admission struct {
	Registration     Registration `json:"Registration"`
	RequestID        [32]byte     `json:"RequestID"`
	GroupKey         [33]byte     `json:"GroupKey"`
	Deposits         []Deposit    `json:"Deposits"`
	Contract         []byte       `json:"Contract"`
	RefundHeight     uint32       `json:"RefundHeight"`
	MinConfirmations uint32       `json:"MinConfirmations"`
}

// AdmissionResult contains the native total verified from all input proofs.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type AdmissionResult struct {
	DepositAmount uint64 `json:"DepositAmount"`
}

// Ready identifies the authenticated local node pair and network.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Ready struct {
	Network   string   `json:"Network"`
	LndPubkey [33]byte `json:"LndPubkey"`
}

// Preparation binds the invoice and transaction recovery request.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Preparation struct {
	Invoice         string `json:"Invoice"`
	PaymentPlan     []byte `json:"PaymentPlan"`
	RecoveryRequest []byte `json:"RecoveryRequest"`
}

// Bundle and Claim use the SDK runtime's versioned serialized transaction data.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Bundle struct {
	Data []byte `json:"Data"`
}

//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Claim struct {
	Data []byte `json:"Data"`
}

// Resolution requires confirmed spends and valid spendable asset proofs.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Resolution struct {
	Claimed  bool   `json:"Claimed"`
	Refunded bool   `json:"Refunded"`
	Evidence []byte `json:"Evidence"`
}

// Operation carries the fixed inputs to an offer-scoped runtime operation.
// Each method consumes only its documented fields and must reject mutation of
// previously persisted signing, transaction or proof intent for this hash.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Operation struct {
	Offer       Offer       `json:"Offer"`
	Preparation Preparation `json:"Preparation"`
	Bundle      Bundle      `json:"Bundle"`
	Claim       Claim       `json:"Claim"`
	Signatures  []byte      `json:"Signatures"`
	Preimage    [32]byte    `json:"Preimage"`
	Request     []byte      `json:"Request"`
	Fallback    Fallback    `json:"Fallback"`
	Evidence    []byte      `json:"Evidence"`
}

// Fallback is the client's verified publishable recovery material.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Fallback struct {
	Bundle          []byte `json:"Bundle"`
	ClaimSignatures []byte `json:"ClaimSignatures"`
}

// PreimageResult distinguishes a verified chain disclosure from no disclosure.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type PreimageResult struct {
	Preimage [32]byte `json:"Preimage"`
	Found    bool     `json:"Found"`
}

// CreateDepositRequest binds idempotent deposit funding to wallet key locators.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type CreateDepositRequest struct {
	RequestID [32]byte `json:"RequestID"`
	GroupKey  [33]byte `json:"GroupKey"`
	Amount    uint64   `json:"Amount"`
	ClientKey [33]byte `json:"ClientKey"`
	ServerKey [33]byte `json:"ServerKey"`
	CSV       uint32   `json:"CSV"`
	KeyFamily int32    `json:"KeyFamily"`
	KeyIndex  uint32   `json:"KeyIndex"`
}

// InventoryRequest selects confirmed deposits of one native asset group.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type InventoryRequest struct {
	GroupKey [33]byte `json:"GroupKey"`
}

// Backend implements authenticated local transaction operations. Calls must
// honor context cancellation and persist intent before irreversible effects.
type Backend interface {
	Call(context.Context, Method, json.RawMessage) (any, error)
}

// BackendFunc adapts a method dispatcher to Backend.
type BackendFunc func(context.Context, Method, json.RawMessage) (any, error)

// Call invokes the configured dispatcher.
func (f BackendFunc) Call(ctx context.Context, method Method,
	payload json.RawMessage) (any, error) {

	return f(ctx, method, payload)
}

// Decode rejects unknown fields and trailing data at the process boundary.
func Decode(raw []byte, target any) error {
	return decode(raw, target)
}

// RefundDepositRequest selects a wallet-owned mature static asset deposit.
// The runtime must reject deposits reserved by an active signing operation.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type RefundDepositRequest struct {
	Outpoint string `json:"Outpoint"`
}

// PublicRPCMaxBytes bounds complete external protobuf requests and responses.
// The local bridge has a separate, larger JSON envelope budget; it does not
// imply that public peers accept arbitrarily long asset proof histories.
const PublicRPCMaxBytes = 16 << 20
