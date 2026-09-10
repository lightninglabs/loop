package reservation

import (
	"crypto/rand"
	"errors"
	"math"
	"time"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/keychain"
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
}

// Validate checks the record before storage. State transitions belong to the
// FSM; the store only requires a named state.
func (r *Reservation) Validate() error {
	if r == nil || r.ID == (ID{}) || r.State == fsm.EmptyState ||
		r.ClientKey.PubKey == nil || r.ClientKey.Family > math.MaxInt32 {

		return errors.New("invalid asset reservation")
	}

	return r.Terms.Validate()
}
