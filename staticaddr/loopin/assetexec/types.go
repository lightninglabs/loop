// Package assetexec journals the client side of server-preimage asset loop-ins.
// It executes the server-owned branch of the shared static loop-in FSM policy
// with mandatory write-ahead persistence before external effects.
package assetexec

import (
	"context"
	"errors"

	depositflow "github.com/lightninglabs/loop/staticaddr/deposit/flow"
	"github.com/lightninglabs/loop/staticaddr/loopin/flow"
	"github.com/lightninglabs/loop/swapkit/bridge"
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

// Terms is the shared public agreement. Client offers reject Admission, which
// is private server recovery data; it is omitted from normal client encoding.
type Terms = bridge.Terms

// Offer contains the server's hash commitment, never its execution preimage.
type Offer = bridge.Offer

// Invoice contains the exact HOLD invoice and durable receiver RFQ binding.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Invoice struct {
	PaymentRequest string `json:"PaymentRequest"`
	PaymentPlan    []byte `json:"PaymentPlan"`
}

// Fallback is independently verified, publishable SDK recovery material.
type Fallback = bridge.Fallback

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
// deposit exclusion. Create atomically binds the reserved admission. Update
// receives the previous revision and commits terminal release atomically with
// DepositsReleased; no external release callback may precede that commit.
type Store interface {
	Create(context.Context, *Record) error
	Get(context.Context, [32]byte) (*Record, error)
	Update(context.Context, *Record, int64) error
	ListActive(context.Context) ([]*Record, error)
}

// Actions connects the controller to the authenticated server, SDK and local
// assetln.Receiver. All effects must be idempotent by hash and immutable input.
// VerifyFinalizeFallback verifies every asset proof,
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
