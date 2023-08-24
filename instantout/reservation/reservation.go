package reservation

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	reservation_script "github.com/lightninglabs/loop/instantout/reservation/script"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// ID is a unique identifier for a reservation.
type ID [IdLength]byte

// FromByteSlice creates a reservation id from a byte slice.
func (r *ID) FromByteSlice(b []byte) error {
	if len(b) != IdLength {
		return fmt.Errorf("reservation id must be 32 bytes, got %d, %x",
			len(b), b)
	}

	copy(r[:], b)

	return nil
}

// Reservation holds all the necessary information for the 2-of-2 multisig
// reservation utxo.
type Reservation struct {
	// ID is the unique identifier of the reservation.
	ID ID

	// State is the current state of the reservation.
	State fsm.StateType

	// ClientPubkey is the client's pubkey.
	ClientPubkey *btcec.PublicKey

	// ServerPubkey is the server's pubkey.
	ServerPubkey *btcec.PublicKey

	// Value is the amount of the reservation.
	Value btcutil.Amount

	// Expiry is the absolute block height at which the reservation expires.
	Expiry uint32

	// KeyLocator is the key locator of the client's key.
	KeyLocator keychain.KeyLocator

	// Outpoint is the outpoint of the reservation.
	Outpoint *wire.OutPoint

	// InitiationHeight is the height at which the reservation was
	// initiated.
	InitiationHeight int32

	// ConfirmationHeight is the height at which the reservation was
	// confirmed.
	ConfirmationHeight uint32
}

func NewReservation(id ID, serverPubkey, clientPubkey *btcec.PublicKey,
	value btcutil.Amount, expiry, heightHint uint32,
	keyLocator keychain.KeyLocator) (*Reservation,
	error) {

	if id == [32]byte{} {
		return nil, errors.New("id is empty")
	}

	if clientPubkey == nil {
		return nil, errors.New("client pubkey is nil")
	}

	if serverPubkey == nil {
		return nil, errors.New("server pubkey is nil")
	}

	if expiry == 0 {
		return nil, errors.New("expiry is 0")
	}

	if value == 0 {
		return nil, errors.New("value is 0")
	}

	if keyLocator.Family == 0 {
		return nil, errors.New("key locator family is 0")
	}
	return &Reservation{
		ID:               id,
		Value:            value,
		ClientPubkey:     clientPubkey,
		ServerPubkey:     serverPubkey,
		KeyLocator:       keyLocator,
		Expiry:           expiry,
		InitiationHeight: int32(heightHint),
	}, nil
}

// GetPkScript returns the pk script of the reservation.
func (r *Reservation) GetPkScript() ([]byte, error) {
	// Now that we have all the required data, we can create the pk script.
	pkScript, err := reservation_script.ReservationScript(
		r.Expiry, r.ServerPubkey, r.ClientPubkey,
	)
	if err != nil {
		return nil, err
	}

	return pkScript, nil
}

// Output returns the reservation output.
func (r *Reservation) Output() (*wire.TxOut, error) {
	pkscript, err := r.GetPkScript()
	if err != nil {
		return nil, err
	}

	return wire.NewTxOut(int64(r.Value), pkscript), nil
}

func (r *Reservation) findReservationOutput(tx *wire.MsgTx) (*wire.OutPoint,
	error) {

	pkScript, err := r.GetPkScript()
	if err != nil {
		return nil, err
	}

	for i, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) {
			return &wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: uint32(i),
			}, nil
		}
	}

	return nil, errors.New("reservation output not found")
}

// SignDescriptor returns the sign descriptor for the reservation.
func (r *Reservation) SignDescriptor() (lndclient.SignDescriptor, error) {
	txScript, err := reservation_script.TaprootExpiryScript(
		r.Expiry, r.ServerPubkey,
	)
	if err != nil {
		return lndclient.SignDescriptor{}, err
	}

	output, err := r.Output()
	if err != nil {
		return lndclient.SignDescriptor{}, err
	}

	return lndclient.SignDescriptor{
		WitnessScript: txScript.Script,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: r.ServerPubkey,
		},
		Output:     output,
		HashType:   txscript.SigHashDefault,
		InputIndex: 0,
		SignMethod: input.TaprootScriptSpendSignMethod,
	}, nil
}

// Musig2CreateSession creates a musig2 session for the reservation.
func (r *Reservation) Musig2CreateSession(ctx context.Context,
	signer lndclient.SignerClient) (*input.MuSig2SessionInfo, error) {

	signers := [][]byte{
		r.ClientPubkey.SerializeCompressed(),
		r.ServerPubkey.SerializeCompressed(),
	}

	expiryLeaf, err := reservation_script.TaprootExpiryScript(
		r.Expiry, r.ServerPubkey,
	)
	if err != nil {
		return nil, err
	}

	rootHash := expiryLeaf.TapHash()

	musig2SessionInfo, err := signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2,
		&r.KeyLocator, signers,
		lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
	)
	if err != nil {
		return nil, err
	}

	return musig2SessionInfo, nil
}
