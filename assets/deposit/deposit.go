package deposit

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/taproot-assets/proof"
)

// State is the enum used for deposit states.
type State uint8

const (
	// StateInitiated indicates that the deposit has been initiated by the
	// client.
	StateInitiated State = 0

	// StatePending indicates that the deposit is pending confirmation on
	// the blockchain.
	StatePending State = 1

	// StateConfirmed indicates that the deposit has been confirmed on the
	// blockchain.
	StateConfirmed State = 2

	// StateExpired indicates that the deposit has expired.
	StateExpired State = 3

	// StateTimeoutSweepPublished indicates that the timeout sweep has been
	// published.
	StateTimeoutSweepPublished State = 4

	// StateWithdrawn indicates that the deposit has been withdrawn.
	StateWithdrawn State = 5

	// StateCooperativeSweepPublished indicates that the cooperative sweep
	// withdrawing the deposit has been published.
	StateCooperativeSweepPublished State = 6

	// StateKeyRevealed indicates that the client has revealed a valid key
	// for the deposit which is now ready to be swept.
	StateKeyRevealed State = 7

	// StateSpent indicates that the deposit has been spent.
	StateSpent State = 8

	// StateSwept indicates that the deposit has been swept, either by a
	// timeout sweep or a cooperative (ie withdrawal) sweep.
	StateSwept State = 9
)

// String coverts a deposit state to human readable string.
func (s State) String() string {
	switch s {
	case StateInitiated:
		return "Initiated"

	case StatePending:
		return "Pending"

	case StateConfirmed:
		return "Confirmed"

	case StateExpired:
		return "Expired"

	case StateTimeoutSweepPublished:
		return "TimeoutSweepPublished"

	case StateWithdrawn:
		return "Withdrawn"

	case StateCooperativeSweepPublished:
		return "CooperativeSweepPublished"

	case StateKeyRevealed:
		return "KeyRevealed"

	case StateSpent:
		return "Spent"

	case StateSwept:
		return "Swept"

	default:
		return "Unknown"
	}
}

// IsFinal returns true if the deposit state is final, meaning that no further
// actions can be taken on the deposit.
func (s State) IsFinal() bool {
	return s == StateSpent || s == StateSwept
}

// DepositInfo holds publicly available information about an asset deposit.
// It is used to communicate deposit details to clients of the deposit Manager.
type DepositInfo struct {
	// ID is the unique identifier for this deposit which will also be used
	// to store the deposit in both the server and client databases.
	ID string

	// Verison is the protocol version of the deposit.
	Version AssetDepositProtocolVersion

	// CreatedAt is the time when the deposit was created (on the client).
	CreatedAt time.Time

	// Amount is the amount of asset to be deposited.
	Amount uint64

	// Addr is the TAP deposit address where the asset will be sent.
	Addr string

	// State is the deposit state.
	State State

	// ConfirmationHeight is the block height at which the deposit was
	// confirmed.
	ConfirmationHeight uint32

	// Outpoint is the anchor outpoint of the deposit. It is only set if the
	// deposit has been confirmed.
	Outpoint *wire.OutPoint

	// Expiry is the block height at which the deposit will expire. It is
	// only set if the deposit has been confirmed.
	Expiry uint32

	// SweepAddr is the address we'll use to sweep the deposit back after
	// timeout or if cooperatively withdrawing.
	SweepAddr string
}

// Copy creates a copy of the DepositInfo struct.
func (d *DepositInfo) Copy() *DepositInfo {
	info := &DepositInfo{
		ID:                 d.ID,
		Version:            d.Version,
		CreatedAt:          d.CreatedAt,
		Amount:             d.Amount,
		Addr:               d.Addr,
		State:              d.State,
		ConfirmationHeight: d.ConfirmationHeight,
		Expiry:             d.Expiry,
		SweepAddr:          d.SweepAddr,
	}

	if d.Outpoint != nil {
		info.Outpoint = &wire.OutPoint{
			Hash:  d.Outpoint.Hash,
			Index: d.Outpoint.Index,
		}
	}

	return info
}

// Deposit is the struct that holds all the information about an asset deposit.
type Deposit struct {
	*Kit

	*DepositInfo

	// PkScript is the pkscript of the deposit anchor output.
	PkScript []byte

	// Proof is the proof of the deposit transfer.
	Proof *proof.Proof

	// AnchorRootHash is the root hash of the deposit anchor output.
	AnchorRootHash []byte
}

// label returns a string label that we can use for marking a transfer funding
// the deposit. This is useful if we need to filter deposits.
func (d *Deposit) label() string {
	return fmt.Sprintf("deposit %v", d.ID)
}

// lockID converts a deposit ID to a lock ID. The lock ID is used to lock inputs
// used for the deposit sweep transaction. Note that we assume that the deposit
// ID is a hex-encoded string of the same length as the lock ID.
func (d *Deposit) lockID() (wtxmgr.LockID, error) {
	var lockID wtxmgr.LockID
	depositIDBytes, err := hex.DecodeString(d.ID)
	if err != nil {
		return wtxmgr.LockID{}, err
	}

	if len(depositIDBytes) != len(lockID) {
		return wtxmgr.LockID{}, fmt.Errorf("invalid deposit ID "+
			"length: %d", len(depositIDBytes))
	}

	copy(lockID[:], depositIDBytes)

	return lockID, nil
}
