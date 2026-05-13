package deposit

import (
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ID is a unique identifier for a deposit.
type ID [IdLength]byte

// DefaultRecoveryScanLimit is the highest child index scanned per static
// address key family when manually recovering a deposit.
const DefaultRecoveryScanLimit = 20

// RecoveryRequest describes one static-address output that should be verified
// on-chain and restored locally.
type RecoveryRequest struct {
	TxID       chainhash.Hash
	VOut       uint32
	HeightHint int32
	PkScript   []byte
}

// RecoveryResult describes the restored deposit and matched static address.
type RecoveryResult struct {
	OutPoint           wire.OutPoint
	Value              btcutil.Amount
	ConfirmationHeight int64
	AddressParams      *address.Parameters
	StaticAddress      string
	RecoveredAddress   bool
	RecoveredDeposit   bool
	DepositID          ID
}

// FromByteSlice creates a deposit id from a byte slice.
func (r *ID) FromByteSlice(b []byte) error {
	if len(b) != IdLength {
		return fmt.Errorf("deposit id must be 32 bytes, got %d, %x",
			len(b), b)
	}

	copy(r[:], b)

	return nil
}

// Deposit bundles an utxo at a static address together with manager-relevant
// data.
//
// Lock order: if both Manager.mu and a Deposit lock are needed, acquire
// Manager.mu before Deposit.Lock. Never acquire Manager.mu while holding a
// Deposit lock.
type Deposit struct {
	sync.Mutex

	// Outpoint of the deposit.
	wire.OutPoint

	// ID is the unique identifier of the deposit.
	ID ID

	// state is the current state of the deposit.
	state fsm.StateType

	// Value is the amount of the deposit.
	Value btcutil.Amount

	// ConfirmationHeight is the absolute height at which the deposit was
	// first confirmed. A value of zero means the deposit is still
	// unconfirmed.
	ConfirmationHeight int64

	// TimeOutSweepPkScript is the pk script that is used to sweep the
	// deposit to after it is expired.
	TimeOutSweepPkScript []byte

	// ExpirySweepTxid is the transaction id of the expiry sweep.
	ExpirySweepTxid chainhash.Hash

	// SwapHash is an optional reference to a static address loop-in swap
	// that used this deposit.
	SwapHash *lntypes.Hash

	// FinalizedWithdrawalTx is the coop-signed withdrawal transaction. It
	// is republished on new block arrivals and on client restarts.
	FinalizedWithdrawalTx *wire.MsgTx

	// AddressParams are the static address parameters that produced this
	// deposit's pkScript. Spending code must use these per-deposit
	// parameters rather than assuming all deposits belong to one address.
	AddressParams *address.Parameters

	// AddressID is the database ID of the static address that produced this
	// deposit's pkScript.
	AddressID int32
}

// IsInFinalState returns true if the deposit is final.
func (d *Deposit) IsInFinalState() bool {
	d.Lock()
	defer d.Unlock()

	// Replaced is inactive from the deposit FSM's point of view. The manager may
	// still revive the same record if lnd reports the exact outpoint again after
	// a transient wallet-view miss.
	return d.state == Expired || d.state == Withdrawn ||
		d.state == LoopedIn || d.state == HtlcTimeoutSwept ||
		d.state == ChannelPublished || d.state == Replaced
}

func (d *Deposit) IsExpired(currentHeight, expiry uint32) bool {
	d.Lock()
	defer d.Unlock()

	if d.ConfirmationHeight <= 0 {
		return false
	}

	return currentHeight >= uint32(d.ConfirmationHeight)+expiry
}

func (d *Deposit) GetState() fsm.StateType {
	d.Lock()
	defer d.Unlock()

	return d.state
}

func (d *Deposit) SetState(state fsm.StateType) {
	d.Lock()
	defer d.Unlock()

	d.state = state
}

func (d *Deposit) SetStateNoLock(state fsm.StateType) {
	d.state = state
}

func (d *Deposit) IsInState(state fsm.StateType) bool {
	d.Lock()
	defer d.Unlock()

	return d.state == state
}

func (d *Deposit) IsInStateNoLock(state fsm.StateType) bool {
	return d.state == state
}

// GetStaticAddressScript reconstructs the static address script for this
// deposit's matched address parameters.
func (d *Deposit) GetStaticAddressScript() (*script.StaticAddress, error) {
	if d.AddressParams == nil {
		return nil, fmt.Errorf("missing static address parameters")
	}

	return script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(d.AddressParams.Expiry),
		d.AddressParams.ClientPubkey, d.AddressParams.ServerPubkey,
	)
}

// GetRandomDepositID generates a random deposit ID.
func GetRandomDepositID() (ID, error) {
	var id ID
	_, err := rand.Read(id[:])
	return id, err
}
