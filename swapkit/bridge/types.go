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
type Terms struct {
	Version          uint32
	RequestID        [32]byte
	RequestDigest    [32]byte
	Owner            [32]byte
	GroupKey         [33]byte
	DepositOutpoints []string
	DepositAmount    uint64
	PayoutAmount     uint64
	AssetFee         uint64
	ChangeAmount     uint64
	MaxDebit         uint64
	RefundHeight     uint32
	MinPayoutExpiry  uint32
	RecoveryDelta    uint32
	ExecutionDelta   uint32
	OfferExpiry      time.Time
	Contract         []byte
	Admission        []byte `json:",omitempty"`
}

// Offer commits to the server payment hash and exact native asset terms.
type Offer struct {
	Hash  [32]byte
	Terms Terms
}

// Registration is loaded from the authenticated owner's static address row.
// The runtime must bind every deposit policy to these exact keys and CSV.
type Registration struct {
	Owner     [32]byte
	ClientKey [33]byte
	ServerKey [33]byte
	CSV       uint32
	KeyFamily int32
	KeyIndex  uint32
}

// Deposit preserves the proof and contract needed after process restart.
type Deposit struct {
	Outpoint           string
	GroupKey           [33]byte
	Amount             uint64
	Proof              []byte
	Contract           []byte
	ConfirmationHeight uint32
	RefundHeight       uint32
	ValueSat           uint64
}

// Admission binds client-supplied proofs to server-authenticated registration
// and server policy before the execution journal reserves any deposit.
type Admission struct {
	Registration     Registration
	RequestID        [32]byte
	GroupKey         [33]byte
	Deposits         []Deposit
	Contract         []byte
	RefundHeight     uint32
	MinConfirmations uint32
}

// AdmissionResult contains the native total verified from all input proofs.
type AdmissionResult struct {
	DepositAmount uint64
}

// Ready identifies the authenticated local node pair and network.
type Ready struct {
	Network   string
	LndPubkey [33]byte
}

// Preparation binds the invoice and transaction recovery request.
type Preparation struct {
	Invoice         string
	PaymentPlan     []byte
	RecoveryRequest []byte
}

// Bundle and Claim use the SDK runtime's versioned serialized transaction data.
type Bundle struct{ Data []byte }
type Claim struct{ Data []byte }

// Resolution requires confirmed spends and valid spendable asset proofs.
type Resolution struct {
	Claimed  bool
	Refunded bool
	Evidence []byte
}

// Operation carries the fixed inputs to an offer-scoped runtime operation.
// Each method consumes only its documented fields and must reject mutation of
// previously persisted signing, transaction or proof intent for this hash.
type Operation struct {
	Offer       Offer
	Preparation Preparation
	Bundle      Bundle
	Claim       Claim
	Signatures  []byte
	Preimage    [32]byte
	Request     []byte
	Fallback    Fallback
	Evidence    []byte
}

// Fallback is the client's verified publishable recovery material.
type Fallback struct {
	Bundle          []byte
	ClaimSignatures []byte
}

// PreimageResult distinguishes a verified chain disclosure from no disclosure.
type PreimageResult struct {
	Preimage [32]byte
	Found    bool
}

// CreateDepositRequest binds idempotent deposit funding to wallet key locators.
type CreateDepositRequest struct {
	RequestID [32]byte
	GroupKey  [33]byte
	Amount    uint64
	ClientKey [33]byte
	ServerKey [33]byte
	CSV       uint32
	KeyFamily int32
	KeyIndex  uint32
}

// InventoryRequest selects confirmed deposits of one native asset group.
type InventoryRequest struct{ GroupKey [33]byte }

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
type RefundDepositRequest struct{ Outpoint string }

// PublicRPCMaxBytes bounds complete external protobuf requests and responses.
// The local bridge has a separate, larger JSON envelope budget; it does not
// imply that public peers accept arbitrarily long asset proof histories.
const PublicRPCMaxBytes = 16 << 20
