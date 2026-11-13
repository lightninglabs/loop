package reservation

import (
	"bytes"
	"crypto/rand"
	"errors"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

// ID identifies a purchase before and after its funding outpoint exists.
type ID [32]byte

// NewID returns a random reservation ID, also used to deduplicate requests.
func NewID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}

// States name the action resumed after a restart.
const (
	RequestQuote       fsm.StateType = "RequestQuote"
	QuoteFailed        fsm.StateType = "QuoteFailed"
	QuoteRejected      fsm.StateType = "QuoteRejected"
	ProbeRoutes        fsm.StateType = "ProbeRoutes"
	AwaitApproval      fsm.StateType = "AwaitApproval"
	PayPrepay          fsm.StateType = "PayPrepay"
	WaitForDelivery    fsm.StateType = "WaitForDelivery"
	VerifyReservation  fsm.StateType = "VerifyReservation"
	Ready              fsm.StateType = "Ready"
	CancelPrepay       fsm.StateType = "CancelPrepay"
	Canceled           fsm.StateType = "Canceled"
	Expired            fsm.StateType = "Expired"
	NeedAdminAttention fsm.StateType = "NeedAdminAttention"
)

// Reservation holds the client's agreed terms and progress. The FSM owns the
// live record; status readers use copies loaded from the store.
type Reservation struct {
	// Terms holds the requested asset and amount, then the quoted fee and
	// lifetime once a quote is validated.
	Terms

	// GroupKey preserves the original selector even after choosing an issuance.
	GroupKey [33]byte

	// ID identifies this purchase across retries and restarts.
	ID ID

	// ClientKey identifies the local signing key and how to derive it again.
	ClientKey keychain.KeyDescriptor

	// State records the client's progress, independently of server status.
	State fsm.StateType

	// CreatedAt records when the purchase was first stored.
	CreatedAt time.Time

	// UpdatedAt records when the stored purchase last changed.
	UpdatedAt time.Time

	// Quote holds the validated offer; nil means no quote has been saved.
	Quote *swapserverrpc.AssetReservationQuote

	// SkipProbe skips automatic probing before approval. Approval saves the
	// caller's choice with PayPrepay, where it permits payment without a
	// successful main probe. This flag alone never authorizes payment.
	// Background recovery preserves this choice.
	SkipProbe bool

	// Probes holds the main probe outcome and both routing fee estimates.
	Probes ProbeResults

	// The single probe request is saved before sending and never replaced.
	ProbeNodeKey  *btcec.PublicKey
	ProbeRequest  *routerrpc.SendPaymentRequest
	ProbeResult   *lnrpc.Payment
	ProbeDeadline time.Time

	// MaxRouteFeeMsat caps the prepay routing fee in millisatoshis. Save it
	// with the transition to PayPrepay; that state records consent to pay.
	MaxRouteFeeMsat uint64

	// Payment identity and request are saved before the first send. A node
	// change must not turn an unknown payment into permission to pay again.
	PaymentHash    lntypes.Hash
	PayingNodeKey  *btcec.PublicKey
	PaymentRequest *routerrpc.SendPaymentRequest
	PaymentResult  *lnrpc.Payment

	FundingOutpoint    *wire.OutPoint
	ConfirmationHeight uint32
	PrepayCredit       uint64
	ReservationProof   []byte
}

// Validate checks the record before storage. State transitions belong to the
// FSM; the store only requires a named state.
func (r *Reservation) Validate() error {
	if r == nil || r.ID == (ID{}) || r.State == fsm.EmptyState ||
		r.ClientKey.PubKey == nil || r.ClientKey.Family > math.MaxInt32 {

		return errors.New("invalid asset reservation")
	}

	if _, err := ParseAssetSelector(r.selector().Bytes()); err != nil {
		return err
	}
	if r.Fee == 0 {
		if r.GroupKey != ([33]byte{}) && r.AssetID != ([32]byte{}) {
			return errors.New("unquoted group reservation has an issuance")
		}
		requested := Terms{
			AssetID: r.AssetID,
			Amount:  r.Amount,
		}
		if r.Terms != requested ||
			r.Amount == 0 || r.Amount > math.MaxInt64 || r.Quote != nil {

			return errors.New("invalid unquoted reservation")
		}
	} else if err := r.Terms.Validate(); err != nil {
		return err
	}
	return r.validatePurchase()
}

// ValidateQuote checks the offer's binding and amounts. The payment adapter
// must also decode both invoices and validate network, hashes, destinations,
// hints, exact values, final CLTV, expiry, and the receiving RFQ.
func (r *Reservation) ValidateQuote(
	q *swapserverrpc.AssetReservationQuote) error {

	if q == nil || !bytes.Equal(q.ReservationId, r.ID[:]) ||
		!bytes.Equal(q.ClientKey, r.ClientKey.PubKey.SerializeCompressed()) {

		return errors.New("quote does not match reservation request")
	}
	terms, err := TermsFromRPC(q.Terms)
	if err != nil {
		return err
	}
	if r.GroupKey == ([33]byte{}) {
		if q.Group != nil || terms.AssetID != r.AssetID {
			return errors.New("quote changes exact issuance")
		}
	} else if !bytes.Equal(q.GetGroup().GetGroupKey(), r.GroupKey[:]) ||
		len(q.Group.PaymentAssetId) != 32 || [32]byte(q.Group.PaymentAssetId) == ([32]byte{}) {

		return errors.New("quote changes requested group or lacks payment issuance")
	}
	if terms.Amount != r.Amount ||
		(r.Fee != 0 && terms != r.Terms) {

		return errors.New("quote changes reservation terms")
	}
	for _, key := range [][]byte{q.ServerKey, q.ReceivingNodeKey, q.EdgeKey} {
		if len(key) != btcec.PubKeyBytesLenCompressed {
			return errors.New("invalid quote public key")
		}
		if _, err := btcec.ParsePubKey(key); err != nil {
			return errors.New("invalid quote public key")
		}
	}
	if bytes.Equal(q.ServerKey, q.ClientKey) || q.PrepayInvoice == "" ||
		q.ProbeInvoice == "" || q.PrepayInvoice == q.ProbeInvoice ||
		q.PrepayAmountMsat == 0 || q.PrepayAmountMsat > math.MaxInt64 ||
		q.EstimatedMainAmountMsat == 0 ||
		q.EstimatedMainAmountMsat > math.MaxInt64 || q.ExpiresAt <= 0 {

		return errors.New("invalid reservation quote")
	}

	return nil
}

// CheckApproval validates a caller's consent against the immutable quote.
// Accepting its hash accepts the quoted asset fee and BTC prepay amount.
// Routing fees and permission to buy without a successful probe are separate.
func (r *Reservation) CheckApproval(
	a *looprpc.ApproveAssetReservationRequest) error {

	if a == nil || r.Quote == nil || len(a.QuoteHash) != 32 {
		return errors.New("missing reservation approval")
	}
	hash, err := QuoteHash(r.Quote)
	if err != nil {
		return err
	}
	if !bytes.Equal(hash[:], a.QuoteHash) {
		return errors.New("approval does not match reservation quote")
	}
	if a.MaxRouteFeeMsat > math.MaxInt64 {
		return errors.New("reservation routing fee limit is out of range")
	}
	return r.checkProbe(a.SkipProbe)
}

// checkPrepay permits preparation only after the approval transition was saved.
func (r *Reservation) checkPrepay() error {
	if r.State != PayPrepay {
		return errors.New("reservation is not approved for prepay")
	}
	return r.checkPaymentLimits()
}

// checkPaymentLimits requires a saved quote, a routing cap that fits storage,
// and either a successful probe or explicit permission to skip it.
func (r *Reservation) checkPaymentLimits() error {
	if r.Quote == nil || r.MaxRouteFeeMsat > math.MaxInt64 {
		return errors.New("invalid reservation payment limits")
	}
	return r.checkProbe(r.SkipProbe)
}

// checkProbe requires a recorded successful probe unless skipping was allowed.
// Probe success describes past reachability, not guaranteed future liquidity.
func (r *Reservation) checkProbe(skipProbe bool) error {
	if !skipProbe && (r.Probes.CheckedAt.IsZero() ||
		r.Probes.Main != ProbeSucceeded) {

		return errors.New("reservation requires a successful main probe " +
			"or explicit approval to skip probing")
	}
	return nil
}

// validatePurchase checks that saved quote, probe, payment, and delivery facts
// are internally consistent. It does not query LND or verify the on-chain proof;
// recovery must reconcile those external facts before continuing the purchase.
func (r *Reservation) validatePurchase() error {
	if r.Quote != nil {
		if err := r.ValidateQuote(r.Quote); err != nil {
			return err
		}
	}
	if r.ProbeRequest != nil {
		if r.Quote == nil || r.ProbeNodeKey == nil ||
			r.ProbeDeadline.IsZero() ||
			r.ProbeRequest.PaymentRequest != r.Quote.ProbeInvoice ||
			r.ProbeRequest.MaxParts != 1 ||
			r.ProbeRequest.TimeoutSeconds <= 0 {

			return errors.New("invalid saved probe request")
		}
	} else if r.ProbeNodeKey != nil || !r.ProbeDeadline.IsZero() ||
		r.ProbeResult != nil {

		return errors.New("incomplete saved probe")
	}
	if r.ProbeResult != nil &&
		(r.ProbeResult.PaymentHash != ProbeHash(r.ID).String() ||
			r.ProbeResult.Status != lnrpc.Payment_FAILED) {

		return errors.New("invalid terminal probe result")
	}
	if r.Probes.Main > ProbeUnsupported ||
		r.Probes.PrepayFeeMsat > math.MaxInt64 ||
		r.Probes.MainFeeMsat > math.MaxInt64 {

		return errors.New("invalid reservation probe results")
	}
	if r.MaxRouteFeeMsat > math.MaxInt64 {
		return errors.New("reservation routing fee limit is out of range")
	}
	if r.State == PayPrepay || r.PaymentRequest != nil {
		if err := r.checkPaymentLimits(); err != nil {
			return err
		}
	}
	if r.PaymentRequest != nil {
		if r.awaitingApproval() || r.PaymentHash == (lntypes.Hash{}) ||
			r.PayingNodeKey == nil ||
			r.PaymentRequest.PaymentRequest != r.Quote.PrepayInvoice ||
			r.PaymentRequest.FeeLimitSat != 0 ||
			r.PaymentRequest.FeeLimitMsat < 0 ||
			uint64(r.PaymentRequest.FeeLimitMsat) > r.MaxRouteFeeMsat {

			return errors.New("invalid saved prepay payment")
		}
	} else if r.PaymentHash != (lntypes.Hash{}) || r.PayingNodeKey != nil {
		return errors.New("incomplete saved prepay payment")
	}
	if r.PaymentResult != nil {
		if r.PaymentRequest == nil ||
			r.PaymentResult.PaymentHash != r.PaymentHash.String() {

			return errors.New("payment result does not match prepay")
		}
		if r.PaymentResult.Status == lnrpc.Payment_SUCCEEDED {
			if r.PaymentResult.ValueMsat <= 0 ||
				uint64(r.PaymentResult.ValueMsat) !=
					r.Quote.PrepayAmountMsat ||
				r.PaymentResult.FeeMsat < 0 ||
				r.PaymentResult.FeeMsat > r.PaymentRequest.FeeLimitMsat {

				return errors.New("prepay receipt exceeds approved amounts")
			}
			preimage, err := lntypes.MakePreimageFromStr(
				r.PaymentResult.PaymentPreimage,
			)
			if err != nil || preimage.Hash() != r.PaymentHash {
				return errors.New("invalid prepay settlement preimage")
			}
		}
	}
	if r.ConfirmationHeight > math.MaxInt32 || r.PrepayCredit > r.Fee ||
		(r.PrepayCredit != 0 && r.PrepayCredit != r.Fee) {

		return errors.New("invalid reservation delivery facts")
	}
	return nil
}

// awaitingApproval identifies the states in which purchase choices may change.
// Once a purchase leaves these states, its choices stay fixed through payment,
// cancellation, delivery, and recovery.
func (r *Reservation) awaitingApproval() bool {
	return r.State == RequestQuote || r.State == ProbeRoutes ||
		r.State == AwaitApproval
}

// preservePurchase rejects updates that replace already-fixed purchase facts.
// Quotes, payment identities, terminal results, and recorded delivery facts stay
// unchanged; approval choices also become fixed once approval is left behind.
// New facts may be filled in as the purchase progresses.
func preservePurchase(next, saved *Reservation) error {
	if saved.ProbeRequest != nil &&
		(!proto.Equal(next.ProbeRequest, saved.ProbeRequest) ||
			next.ProbeNodeKey == nil ||
			!next.ProbeNodeKey.IsEqual(saved.ProbeNodeKey) ||
			!next.ProbeDeadline.Equal(saved.ProbeDeadline)) {

		return ErrRequestConflict
	}
	if saved.ProbeResult != nil &&
		!proto.Equal(next.ProbeResult, saved.ProbeResult) {

		return ErrRequestConflict
	}
	if (saved.Fee != 0 && next.Terms != saved.Terms) ||
		(saved.Quote != nil && !proto.Equal(next.Quote, saved.Quote)) ||
		(!saved.awaitingApproval() && (next.awaitingApproval() ||
			next.MaxRouteFeeMsat != saved.MaxRouteFeeMsat ||
			next.Probes != saved.Probes ||
			next.SkipProbe != saved.SkipProbe)) ||
		(saved.PaymentRequest != nil &&
			(!proto.Equal(next.PaymentRequest, saved.PaymentRequest) ||
				next.PaymentHash != saved.PaymentHash ||
				next.PayingNodeKey == nil ||
				!next.PayingNodeKey.IsEqual(saved.PayingNodeKey))) ||
		(saved.FundingOutpoint != nil && (next.FundingOutpoint == nil ||
			*next.FundingOutpoint != *saved.FundingOutpoint)) ||
		(saved.ConfirmationHeight != 0 &&
			next.ConfirmationHeight != saved.ConfirmationHeight) ||
		(saved.PrepayCredit != 0 && next.PrepayCredit != saved.PrepayCredit) ||
		(len(saved.ReservationProof) != 0 &&
			!bytes.Equal(
				next.ReservationProof, saved.ReservationProof,
			)) {

		return ErrRequestConflict
	}
	if saved.PaymentResult != nil &&
		(saved.PaymentResult.Status == lnrpc.Payment_SUCCEEDED ||
			saved.PaymentResult.Status == lnrpc.Payment_FAILED) &&
		!proto.Equal(next.PaymentResult, saved.PaymentResult) {

		return ErrRequestConflict
	}
	return nil
}
