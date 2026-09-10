package reservation

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrPaymentNotFound must come from an authoritative lookup on the
	// saved paying node. A timeout or a different node is not absence.
	ErrPaymentNotFound    = errors.New("prepay payment not found")
	ErrInvalidReservation = errors.New("invalid funded reservation")
)

// PaymentPlan contains the exact LND request and node identity saved before
// sending a probe or prepay. Its hash must match the corresponding invoice.
type PaymentPlan struct {
	Hash    lntypes.Hash
	NodeKey *btcec.PublicKey
	Request *routerrpc.SendPaymentRequest
}

// Payments checks quotes and pays approved prepays through bounded LND calls.
type Payments interface {
	// ValidateQuote checks that the invoices and receiving RFQ agree with
	// the quote, including amounts, destinations, and validity periods.
	// It sends no payment and must run again before paying after recovery.
	ValidateQuote(context.Context, *swapserverrpc.AssetReservationQuote) error

	// PrepareProbe returns a request that must be saved before SendProbe.
	PrepareProbe(context.Context, *Reservation) (*PaymentPlan, error)
	SendProbe(context.Context, *Reservation, func(*lnrpc.Payment) error) error

	// TrackProbe streams the saved payment, including in-flight routes.
	// Both methods deliver updates synchronously for persistence by the FSM.
	TrackProbe(context.Context, *Reservation, func(*lnrpc.Payment) error) error

	// Prepare rechecks the approved quote, available balance, payment
	// lifetime, and routing limits without sending a payment. It returns
	// the exact request and paying node, which the caller must save before
	// calling Pay so that a restart cannot lose track of the payment.
	Prepare(context.Context, *Reservation) (*PaymentPlan, error)

	// Lookup retrieves the payment's current status by hash from the saved
	// paying node. The payment may still be in flight. ErrPaymentNotFound
	// means that node confirmed its absence; a timeout or node mismatch
	// must return a different error. Lookup never sends a payment.
	Lookup(context.Context, *btcec.PublicKey, lntypes.Hash) (*lnrpc.Payment, error)

	// Pay sends the saved request through the saved paying node. A nil
	// error does not confirm settlement; an error does not prove that
	// nothing was sent. The caller must use Lookup to resolve the outcome
	// and must not pay again after a terminal failure.
	Pay(context.Context, *btcec.PublicKey, *routerrpc.SendPaymentRequest) error
}

// FundingStatus comes from a synchronized local LND-backed chain view, not
// server status. ConfirmationHeight is the original funding height. Spent
// means LND has reported a spend, not that a synchronous UTXO check completed.
type FundingStatus struct {
	Height             uint32
	ConfirmationHeight uint32
	Spent              bool
}

// ReservationVerifier uses tapd and the shared deposit kit. Verify checks full
// proof history, asset, amount, both keys, the local key locator, scripts,
// and exact output. Like Instant Out, spend tracking uses LND notifications;
// readiness does not require a synchronous unspent assertion.
type ReservationVerifier interface {
	// DeriveKey allocates the next local key for jointly controlling the
	// reservation with the server. The caller must save its descriptor
	// before requesting a quote and reuse it when retrying that purchase.
	DeriveKey(context.Context) (*keychain.KeyDescriptor, error)

	// Verify checks the supplied asset proof against the reservation's
	// terms, keys, and funding output, and checks its anchor against the
	// local chain. It returns the observed funding status; the caller must
	// still apply the required confirmation depth and lifetime limits.
	Verify(context.Context, *Reservation, []byte) (FundingStatus, error)

	// Inspect rechecks the saved proof and observes funding confirmations
	// and spends through the local chain backend. It may wait for a new
	// block or spend until the context ends. The confirmation height stays
	// anchored to the funding transaction so that recovery cannot extend
	// the reservation's lifetime.
	Inspect(context.Context, *Reservation) (FundingStatus, error)
}

// Config provides the client's purchase services. Node adapters and daemon
// wiring remain separate from this FSM.
type Config struct {
	Store       Store
	Server      swapserverrpc.AssetReservationServiceClient
	Payments    Payments
	Wallet      ReservationVerifier
	Clock       clock.Clock
	NotifyAdmin func(ID, error)
}

// Validate rejects a client that cannot recover its purchases.
func (c *Config) Validate() error {
	if c == nil || c.Store == nil || c.Server == nil || c.Payments == nil ||
		c.Wallet == nil || c.Clock == nil || c.NotifyAdmin == nil {

		return errors.New("incomplete reservation client configuration")
	}
	return nil
}
