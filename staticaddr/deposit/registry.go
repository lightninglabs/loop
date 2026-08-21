package deposit

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/wire"
)

var (
	// ErrDepositInUse is returned when another client operation is already
	// using one of the requested deposits.
	ErrDepositInUse = errors.New("deposit already in use")
)

// depositUseRegistration identifies a single use of one or more deposits.
// Its pointer identity prevents delayed cleanup from unregistering a newer
// operation.
type depositUseRegistration struct {
	// Give each registration non-zero size so distinct pointer identity is
	// guaranteed.
	_ byte
}

// depositUseRegistry coordinates in-flight client operations that use static
// address deposits. It is intentionally kept in memory so a client restart
// clears incomplete registrations and lets persisted deposit state drive
// recovery.
type depositUseRegistry struct {
	mu sync.Mutex

	registrations map[wire.OutPoint]*depositUseRegistration
}

// register records the deposits as being in use and returns an owner-safe
// cleanup function. Either all deposits are registered or none are.
func (r *depositUseRegistry) register(deposits []*Deposit) (func(), error) {
	if len(deposits) == 0 {
		return nil, errors.New("no deposits selected")
	}

	// Copy the outpoints up front so the cleanup closure does not depend on
	// caller-owned deposit pointers after this method returns.
	outpoints := make([]wire.OutPoint, len(deposits))
	for i, d := range deposits {
		if d == nil {
			return nil, fmt.Errorf("nil deposit at index %d", i)
		}

		outpoints[i] = d.OutPoint
	}

	// Reject duplicate inputs before taking the registry lock. A duplicate
	// would otherwise make ownership of the cleanup entry ambiguous.
	if err := CheckDuplicates(outpoints); err != nil {
		return nil, err
	}

	// Keep the conflict check and registration under the same lock so a
	// request for multiple deposits is registered atomically.
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check all outpoints before changing the map. This ensures a conflict
	// leaves every requested deposit unregistered by this operation.
	for _, outpoint := range outpoints {
		if _, ok := r.registrations[outpoint]; ok {
			return nil, fmt.Errorf("%w: %v", ErrDepositInUse,
				outpoint)
		}
	}

	// Initialize the map lazily because the registry's zero value is ready
	// for use and is embedded directly in the deposit manager.
	if r.registrations == nil {
		r.registrations = make(
			map[wire.OutPoint]*depositUseRegistration,
		)
	}

	// Use one registration token for the whole request. The token lets the
	// cleanup closure prove that it still owns each entry it removes.
	registration := &depositUseRegistration{}
	for _, outpoint := range outpoints {
		r.registrations[outpoint] = registration
	}

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		for _, outpoint := range outpoints {
			// Only remove entries still owned by this registration. This
			// makes repeated or delayed cleanup safe if a newer operation
			// has since registered the same outpoint.
			current, ok := r.registrations[outpoint]
			if ok && current == registration {
				delete(r.registrations, outpoint)
			}
		}
	}, nil
}
