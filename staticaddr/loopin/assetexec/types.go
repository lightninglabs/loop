// Package assetexec journals the client side of server-preimage asset loop-ins.
// It executes the server-owned branch of the shared static loop-in FSM policy
// with mandatory write-ahead persistence before external effects.
package assetexec

import (
	"context"
	"errors"
	"time"

	depositflow "github.com/lightninglabs/loop/staticaddr/deposit/flow"
	"github.com/lightninglabs/loop/staticaddr/loopin/flow"
)

var (
	ErrNotFound = errors.New("asset loop-in not found")
	ErrConflict = errors.New("asset loop-in changed")
	ErrInvalid  = errors.New("invalid asset loop-in")
	ErrState    = errors.New("invalid asset loop-in state")
	ErrPending  = errors.New("asset loop-in pending")
)

// State reuses the established static loop-in lifecycle names. Additional
// write-ahead boundaries within each state are explicit fields in Record.
type State = flow.State

const (
	InitHtlcTx              = flow.InitHtlcTx
	SignHtlcTx              = flow.SignHtlcTx
	MonitorInvoiceAndHtlcTx = flow.MonitorInvoiceAndHtlcTx
	PaymentReceived         = flow.PaymentReceived
	SweepHtlcTimeout        = flow.SweepHtlcTimeout
	HtlcTimeoutSwept        = flow.HtlcTimeoutSwept
	Succeeded               = flow.Succeeded
	Failed                  = flow.Failed
)

// Terms is the immutable public agreement. GroupKey is canonical compressed.
// Contract binds every asset packet, proof, anchor script, key, amount, and
// recovery fee tier.
// The transaction adapter verifies that complete specification at admission.
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
}

// Offer contains the server's hash commitment, never its execution preimage.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Offer struct {
	Hash  [32]byte `json:"Hash"`
	Terms Terms    `json:"Terms"`
}

// Invoice contains the exact HOLD invoice and durable receiver RFQ binding.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Invoice struct {
	PaymentRequest string `json:"PaymentRequest"`
	PaymentPlan    []byte `json:"PaymentPlan"`
}

// Fallback is the independently verified and fully signed SDK recovery bundle.
// ClaimSignatures may authorize only a preimage-revealing HTLC fallback, never
// an unconditional cooperative spend of the deposits.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Fallback struct {
	Bundle          []byte `json:"Bundle"`
	ClaimSignatures []byte `json:"ClaimSignatures"`
}

// Record contains private client recovery material. Do not expose it in RPCs.
// InvoiceIntent and CooperativeRequest never contain private signing nonces.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Record struct {
	Revision int64 `json:"Revision"`
	State    State `json:"State"`
	// DepositState applies the shared deposit FSM to every selected input.
	// It commits atomically with exposure and confirmed outcome evidence.
	DepositState          depositflow.State `json:"DepositState"`
	Offer                 Offer             `json:"Offer"`
	InvoiceIntent         []byte            `json:"InvoiceIntent"`
	Reserved              bool              `json:"Reserved"`
	DepositsReleased      bool              `json:"DepositsReleased"`
	InvoiceCreating       bool              `json:"InvoiceCreating"`
	Invoice               Invoice           `json:"Invoice"`
	ServerFallback        []byte            `json:"ServerFallback"`
	Fallback              Fallback          `json:"Fallback"`
	AcceptedEvidence      []byte            `json:"AcceptedEvidence"`
	SignaturesExposed     bool              `json:"SignaturesExposed"`
	Preimage              [32]byte          `json:"Preimage"`
	HasPreimage           bool              `json:"HasPreimage"`
	CooperativeRequest    []byte            `json:"CooperativeRequest"`
	CooperativeSignature  []byte            `json:"CooperativeSignature"`
	CancellationRequested bool              `json:"CancellationRequested"`
	RefundEvidence        []byte            `json:"RefundEvidence"`
	ClaimEvidence         []byte            `json:"ClaimEvidence"`
}

// Store persists independent record copies with atomic revision checks and
// deposit exclusion. Update receives the previous revision. Terminal records
// remain recoverable until the existing deposit manager confirms release.
type Store interface {
	Create(context.Context, *Record) error
	Get(context.Context, [32]byte) (*Record, error)
	Update(context.Context, *Record, int64) error
	ListActive(context.Context) ([]*Record, error)
}

// Actions connects the controller to the authenticated server, SDK and local
// assetln.Receiver. All effects must be idempotent by hash and immutable input.
// Reserve/Release must use the existing deposit manager's shared locks and
// enforce hash ownership. VerifyFinalizeFallback verifies every asset proof,
// transaction, output, fee tier and signature, then returns a publishable
// bundle. It must retain private nonces in its signer, never in Record.
// Prepared must call Receiver.Prepared locally, not trust server IN_FLIGHT.
// InvoiceSettled must observe the local invoice as SETTLED. RecoverRefund
// idempotently publishes and monitors the persisted fallback and refund when
// their timelocks permit. It returns evidence only after all deposits are
// currently confirmed refunded with valid spendable asset proofs; an open
// timelock, mempool spend, or stale confirmation is insufficient.
type Actions interface {
	CurrentHeight(context.Context) (uint32, error)
	ValidateOffer(context.Context, Offer) error
	ReserveDeposits(context.Context, Offer) error
	ReleaseDeposits(context.Context, Offer) error
	CreateInvoice(context.Context, Offer, []byte) (Invoice, error)
	LookupInvoice(context.Context, Offer) (Invoice, error)
	VerifyFinalizeFallback(context.Context, Offer, []byte) (Fallback, error)
	Prepared(context.Context, Offer) ([]byte, error)
	PushSignatures(context.Context, Offer, []byte) error
	LookupPreimage(context.Context, Offer) ([32]byte, bool, error)
	SettleInvoice(context.Context, Offer, [32]byte) error
	InvoiceSettled(context.Context, Offer) (bool, error)
	SignCooperative(context.Context, Offer, []byte) ([]byte, error)
	InspectClaim(context.Context, Offer, Fallback) ([]byte, error)
	RecoverRefund(context.Context, Offer, Fallback) ([]byte, error)
	CancelInvoice(context.Context, Offer) error
}
