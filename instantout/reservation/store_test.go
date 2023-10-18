package reservation

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestSqlStore tests the basic functionality of the SQLStore.
func TestSqlStore(t *testing.T) {
	ctxb := context.Background()
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	store := NewSQLStore(testDb)

	// Create a reservation and store it.
	reservation := &Reservation{
		ID:           getRandomReservationID(),
		State:        fsm.StateType("init"),
		ClientPubkey: defaultPubkey,
		ServerPubkey: defaultPubkey,
		Value:        100,
		Expiry:       100,
		KeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  1,
		},
	}

	err := store.CreateReservation(ctxb, reservation)
	require.NoError(t, err)

	// Get the reservation and compare it.
	reservation2, err := store.GetReservation(ctxb, reservation.ID)
	require.NoError(t, err)
	require.Equal(t, reservation, reservation2)

	// Update the reservation and compare it.
	reservation.State = fsm.StateType("state2")
	err = store.UpdateReservation(ctxb, reservation)
	require.NoError(t, err)

	reservation2, err = store.GetReservation(ctxb, reservation.ID)
	require.NoError(t, err)
	require.Equal(t, reservation, reservation2)

	// Add an outpoint to the reservation and compare it.
	reservation.Outpoint = &wire.OutPoint{
		Hash:  chainhash.Hash{0x01},
		Index: 0,
	}
	reservation.State = Confirmed

	err = store.UpdateReservation(ctxb, reservation)
	require.NoError(t, err)

	reservation2, err = store.GetReservation(ctxb, reservation.ID)
	require.NoError(t, err)
	require.Equal(t, reservation, reservation2)

	// Add a second reservation.
	reservation3 := &Reservation{
		ID:           getRandomReservationID(),
		State:        fsm.StateType("init"),
		ClientPubkey: defaultPubkey,
		ServerPubkey: defaultPubkey,
		Value:        99,
		Expiry:       100,
		KeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  1,
		},
	}

	err = store.CreateReservation(ctxb, reservation3)
	require.NoError(t, err)

	reservations, err := store.ListReservations(ctxb)
	require.NoError(t, err)
	require.Equal(t, 2, len(reservations))
}

// getRandomReservationID generates a random reservation ID.
func getRandomReservationID() ID {
	var id ID
	rand.Read(id[:]) // nolint: errcheck
	return id
}
