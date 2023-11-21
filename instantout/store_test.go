package instantout

import (
	"crypto/rand"
	"testing"

	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/stretchr/testify/require"
)

func TestConvertingReservations(t *testing.T) {
	var resId1, resId2 reservation.ID

	// fill the ids with random values.
	if _, err := rand.Read(resId1[:]); err != nil {
		t.Fatal(err)
	}

	if _, err := rand.Read(resId2[:]); err != nil {
		t.Fatal(err)
	}

	reservations := []*reservation.Reservation{
		{ID: resId1}, {ID: resId2},
	}

	byteSlice := reservationIdsToByteSlice(reservations)
	require.Len(t, byteSlice, 64)

	reservationIds, err := byteSliceToReservationIds(byteSlice)
	require.NoError(t, err)

	require.Len(t, reservationIds, 2)
	require.Equal(t, resId1, reservationIds[0])
	require.Equal(t, resId2, reservationIds[1])
}
