package loopin

import (
	"fmt"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/zpay32"
	"sync"

	"github.com/lightninglabs/loop/fsm"
)

// ID is a unique identifier for a deposit.
type ID [IdLength]byte

// FromByteSlice creates a deposit id from a byte slice.
func (r *ID) FromByteSlice(b []byte) error {
	if len(b) != IdLength {
		return fmt.Errorf("deposit id must be 32 bytes, got %d, %x",
			len(b), b)
	}

	copy(r[:], b)

	return nil
}

// StaticAddressLoopIn ...
type StaticAddressLoopIn struct {
	// ID is the unique identifier of the deposit.
	ID ID

	// ProtocolVersion is the protocol version of the static address.
	StaticAddressProtocolVersion version.AddressProtocolVersion

	SwapInvoice string

	SwapPayRequest *zpay32.Invoice

	// SwapHash is the hash of the swap.
	SwapHash lntypes.Hash

	// SwapPreimage is the preimage that is used for the swap.
	SwapPreimage lntypes.Preimage

	LastHop []byte

	// State is the current state of the swap.
	state fsm.StateType

	// InitiationHeight is the height at which the swap was initiated.
	InitiationHeight uint32

	// L402ID is the main identifier of the L402 that was used to perform
	// a swap. If the legacy server interface without aperture/L402 is used,
	// this will be nil.
	L402ID l402.TokenID

	AddressParams *address.Parameters

	Address *script.StaticAddress

	DepositOutpoints []string

	Deposits []*deposit.Deposit

	// Value is the amount that is swapped.
	Value btcutil.Amount

	// ///////////////// HTLC Fields //////////////////////
	// ClientPubkey is the pubkey of the client that is used for the swap.
	ClientPubkey *btcec.PublicKey

	// ServerPubkey is the pubkey of the server that is used for the swap.
	ServerPubkey *btcec.PublicKey

	// HtlcExpiry is the expiry of the swap.
	HtlcExpiry int32

	// HtlcKeyLocator is the locator of the server's htlc key.
	HtlcKeyLocator keychain.KeyLocator

	// HtlcFeeRate is the fee rate that is used for the htlc transaction.
	HtlcFeeRate chainfee.SatPerKWeight

	// ///////////////// Sweepless Tx Fields //////////////////////
	// SweeplessFeeRate is the fee rate that is used for the htlc
	// transaction.
	SweeplessFeeRate chainfee.SatPerKWeight

	// SweeplessSweepAddress is the address that is used to cooperatively
	// sweep the deposits as part of the loop in swap.
	SweeplessSweepAddress btcutil.Address

	// SweepTxHash is the hash of the sweep transaction.
	SweepTxHash *chainhash.Hash

	// SweepConfirmationHeight is the height at which the sweep
	// transaction was confirmed.
	SweepConfirmationHeight uint32

	sync.Mutex
}

func (l *StaticAddressLoopIn) getHtlc(chainParams *chaincfg.Params) (*swap.Htlc,
	error) {

	return swap.NewHtlcV2(
		l.HtlcExpiry, pubkeyTo33ByteSlice(l.ClientPubkey),
		pubkeyTo33ByteSlice(l.ServerPubkey), l.SwapHash, chainParams,
	)
}

func (l *StaticAddressLoopIn) GetState() fsm.StateType {
	l.Lock()
	defer l.Unlock()

	return l.state
}

func (l *StaticAddressLoopIn) SetState(state fsm.StateType) {
	l.Lock()
	defer l.Unlock()

	l.state = state
}

func (l *StaticAddressLoopIn) IsInState(state fsm.StateType) bool {
	l.Lock()
	defer l.Unlock()

	return l.state == state
}

// pubkeyTo33ByteSlice converts a pubkey to a 33 byte slice.
func pubkeyTo33ByteSlice(pubkey *btcec.PublicKey) [33]byte {
	var pubkeyBytes [33]byte
	copy(pubkeyBytes[:], pubkey.SerializeCompressed())

	return pubkeyBytes
}
