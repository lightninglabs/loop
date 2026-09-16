package reservation

import (
	"crypto/rand"
	"errors"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ID identifies a purchase before and after its funding outpoint exists.
type ID [32]byte

// NewID returns a random reservation ID, also used to deduplicate requests.
func NewID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}

// Reservation holds the client's agreed terms and progress. The FSM owns the
// live record; status readers use copies loaded from the store.
type Reservation struct {
	// Terms holds the requested asset and amount, then the quoted fee and
	// lifetime once a quote is validated.
	Terms

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
	// An explicit probe retry clears it.
	SkipProbe bool

	// Probes holds the main probe outcome and both routing fee estimates.
	Probes ProbeResults

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

	if r.Fee == 0 {
		requested := Terms{
			AssetID: r.AssetID,
			Amount:  r.Amount,
		}
		if r.Terms != requested || r.AssetID == ([32]byte{}) ||
			r.Amount == 0 || r.Amount > math.MaxInt64 || r.Quote != nil {

			return errors.New("invalid unquoted reservation")
		}
	} else if err := r.Terms.Validate(); err != nil {
		return err
	}
	return r.validatePurchase()
}
