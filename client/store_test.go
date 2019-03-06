package client

import (
	"crypto/sha256"
	"io/ioutil"
	"reflect"
	"testing"
	"time"

	"github.com/lightninglabs/nautilus/test"
)

func TestStore(t *testing.T) {

	tempDirName, err := ioutil.TempDir("", "clientstore")
	if err != nil {
		t.Fatal(err)
	}

	store, err := newBoltSwapClientStore(tempDirName)
	if err != nil {
		t.Fatal(err)
	}

	swaps, err := store.getUnchargeSwaps()
	if err != nil {
		t.Fatal(err)
	}

	if len(swaps) != 0 {
		t.Fatal("expected empty store")
	}

	destAddr := test.GetDestAddr(t, 0)

	senderKey := [33]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2}

	receiverKey := [33]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 3}

	hash := sha256.Sum256(testPreimage[:])

	initiationTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)

	pendingSwap := UnchargeContract{
		SwapContract: SwapContract{
			AmountRequested:     100,
			Preimage:            testPreimage,
			CltvExpiry:          144,
			SenderKey:           senderKey,
			PrepayInvoice:       "prepayinvoice",
			ReceiverKey:         receiverKey,
			MaxMinerFee:         10,
			MaxSwapFee:          20,
			MaxPrepayRoutingFee: 40,
			InitiationHeight:    99,

			// Convert to/from unix to remove timezone, so that it
			// doesn't interfere with DeepEqual.
			InitiationTime: time.Unix(0, initiationTime.UnixNano()),
		},
		DestAddr:          destAddr,
		SwapInvoice:       "swapinvoice",
		MaxSwapRoutingFee: 30,
		SweepConfTarget:   2,
	}

	checkSwap := func(expectedState SwapState) {
		t.Helper()

		swaps, err := store.getUnchargeSwaps()
		if err != nil {
			t.Fatal(err)
		}

		if len(swaps) != 1 {
			t.Fatal("expected pending swap in store")
		}

		swap := swaps[0].Contract
		if !reflect.DeepEqual(swap, &pendingSwap) {
			t.Fatal("invalid pending swap data")
		}

		if swaps[0].State() != expectedState {
			t.Fatalf("expected state %v, but got %v",
				expectedState, swaps[0].State(),
			)
		}
	}

	err = store.createUncharge(hash, &pendingSwap)
	if err != nil {
		t.Fatal(err)
	}

	checkSwap(StateInitiated)

	err = store.createUncharge(hash, &pendingSwap)
	if err == nil {
		t.Fatal("expected error on storing duplicate")
	}

	checkSwap(StateInitiated)

	if err := store.updateUncharge(hash, testTime, StatePreimageRevealed); err != nil {
		t.Fatal(err)
	}

	checkSwap(StatePreimageRevealed)

	if err := store.updateUncharge(hash, testTime, StateFailInsufficientValue); err != nil {
		t.Fatal(err)
	}

	checkSwap(StateFailInsufficientValue)

	err = store.close()
	if err != nil {
		t.Fatal(err)
	}

	// Reopen store
	store, err = newBoltSwapClientStore(tempDirName)
	if err != nil {
		t.Fatal(err)
	}

	checkSwap(StateFailInsufficientValue)
}
