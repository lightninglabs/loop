package reservation

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math"
	"math/big"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

// ProbeOutcome describes reachability, not a promise of payment success.
type ProbeOutcome uint8

const (
	ProbeNotRun ProbeOutcome = iota
	ProbeSucceeded
	ProbeFailed
	ProbeTimedOut
	ProbeUnsupported
)

// ProbeResults retains the main probe and fee estimates shown before approval.
type ProbeResults struct {
	// Main reports whether the estimated swap payment reached its destination.
	Main ProbeOutcome

	// PrepayFeeMsat scales MainFeeMsat by the BTC payment amounts, rounded up.
	// It ignores fixed hop fees and is unavailable unless Main succeeded.
	PrepayFeeMsat uint64

	// MainFeeMsat is the main probe's routing estimate, in millisatoshis.
	MainFeeMsat uint64

	// CheckedAt records when the main probe completed.
	CheckedAt time.Time
}

// QuoteHash identifies the exact quote the user approved.
func QuoteHash(q *swapserverrpc.AssetReservationQuote) ([32]byte, error) {
	if q == nil {
		return [32]byte{}, errors.New("missing reservation quote")
	}
	data, err := (proto.MarshalOptions{
		Deterministic: true,
	}).Marshal(q)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// TermsFromRPC checks a server offer without imposing client pricing policy.
func TermsFromRPC(t *swapserverrpc.AssetReservationTerms) (Terms, error) {
	if t == nil || len(t.AssetId) != 32 {
		return Terms{}, errors.New("invalid reservation terms")
	}
	terms := Terms{
		AssetID:               [32]byte(t.AssetId),
		Amount:                t.Amount,
		Fee:                   t.Fee,
		CSVDelay:              t.CsvDelay,
		RequiredConfirmations: t.RequiredConfirmations,
		ExecutionDelta:        t.ExecutionDelta,
		MinUsableBlocks:       t.MinUsableBlocks,
	}
	return terms, terms.Validate()
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
	if terms.AssetID != r.AssetID || terms.Amount != r.Amount ||
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

func (r *Reservation) checkPaymentLimits() error {
	if r.Quote == nil || r.MaxRouteFeeMsat > math.MaxInt64 {
		return errors.New("invalid reservation payment limits")
	}
	return r.checkProbe(r.SkipProbe)
}

func (r *Reservation) checkProbe(skipProbe bool) error {
	if !skipProbe && (r.Probes.CheckedAt.IsZero() ||
		r.Probes.Main != ProbeSucceeded) {

		return errors.New("reservation requires a successful main probe " +
			"or explicit approval to skip probing")
	}
	return nil
}

// ParseOutpoint accepts exactly the canonical txid:vout representation.
func ParseOutpoint(value string) (*wire.OutPoint, error) {
	outpoint, err := wire.NewOutPointFromString(value)
	if err != nil || outpoint.String() != value {
		return nil, errors.New("invalid reservation outpoint")
	}
	return outpoint, nil
}

func (r *Reservation) validatePurchase() error {
	if r.Quote != nil {
		if err := r.ValidateQuote(r.Quote); err != nil {
			return err
		}
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

func preservePurchase(next, saved *Reservation) error {
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

// estimatePrepayRoutingFee scales the main fee by the BTC payment amounts.
// It rounds up to a millisatoshi and deliberately ignores fixed hop fees.
func estimatePrepayRoutingFee(mainFee, prepayAmount, mainAmount uint64) (
	uint64, error) {

	if prepayAmount == 0 || mainAmount == 0 {
		return 0, errors.New("missing amount for prepay fee estimate")
	}
	// Multiply before dividing, using a wide integer so large valid quotes
	// cannot overflow or lose precision through floating-point arithmetic.
	fee := new(big.Int).SetUint64(mainFee)
	fee.Mul(fee, new(big.Int).SetUint64(prepayAmount))
	fee.Add(fee, new(big.Int).SetUint64(mainAmount-1))
	fee.Quo(fee, new(big.Int).SetUint64(mainAmount))
	if !fee.IsInt64() {
		return 0, errors.New("prepay routing fee estimate is out of range")
	}
	return fee.Uint64(), nil
}
