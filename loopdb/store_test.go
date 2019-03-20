package loopdb

import (
	"crypto/sha256"
	"io/ioutil"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	senderKey = [33]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2,
	}

	receiverKey = [33]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 3,
	}

	testPreimage = lntypes.Preimage([32]byte{
		1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4,
		1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4,
	})

	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)
)

// TestLoopOutStore tests all the basic functionality of the current bbolt
// swap store.
func TestLoopOutStore(t *testing.T) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDirName)

	store, err := NewBoltSwapStore(tempDirName)
	if err != nil {
		t.Fatal(err)
	}

	// First, verify that an empty database has no active swaps.
	swaps, err := store.FetchLoopOutSwaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(swaps) != 0 {
		t.Fatal("expected empty store")
	}

	destAddr := test.GetDestAddr(t, 0)
	hash := sha256.Sum256(testPreimage[:])
	initiationTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)

	// Next, we'll make a new pending swap that we'll insert into the
	// database shortly.
	pendingSwap := LoopOutContract{
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

	// checkSwap is a test helper function that'll assert the state of a
	// swap.
	checkSwap := func(expectedState SwapState) {
		t.Helper()

		swaps, err := store.FetchLoopOutSwaps()
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

	// If we create a new swap, then it should show up as being initialized
	// right after.
	if err := store.CreateLoopOut(hash, &pendingSwap); err != nil {
		t.Fatal(err)
	}
	checkSwap(StateInitiated)

	// Trying to make the same swap again should result in an error.
	if err := store.CreateLoopOut(hash, &pendingSwap); err == nil {
		t.Fatal("expected error on storing duplicate")
	}
	checkSwap(StateInitiated)

	// Next, we'll update to the next state of the pre-image being
	// revealed. The state should be reflected here again.
	err = store.UpdateLoopOut(
		hash, testTime, StatePreimageRevealed,
	)
	if err != nil {
		t.Fatal(err)
	}
	checkSwap(StatePreimageRevealed)

	// Next, we'll update to the final state to ensure that the state is
	// properly updated.
	err = store.UpdateLoopOut(
		hash, testTime, StateFailInsufficientValue,
	)
	if err != nil {
		t.Fatal(err)
	}
	checkSwap(StateFailInsufficientValue)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// If we re-open the same store, then the state of the current swap
	// should be the same.
	store, err = NewBoltSwapStore(tempDirName)
	if err != nil {
		t.Fatal(err)
	}
	checkSwap(StateFailInsufficientValue)
}

// TestLoopInStore tests all the basic functionality of the current bbolt
// swap store.
func TestLoopInStore(t *testing.T) {
	tempDirName, err := ioutil.TempDir("", "clientstore")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDirName)

	store, err := NewBoltSwapStore(tempDirName)
	if err != nil {
		t.Fatal(err)
	}

	// First, verify that an empty database has no active swaps.
	swaps, err := store.FetchLoopInSwaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(swaps) != 0 {
		t.Fatal("expected empty store")
	}

	hash := sha256.Sum256(testPreimage[:])
	initiationTime := time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC)

	// Next, we'll make a new pending swap that we'll insert into the
	// database shortly.
	pendingSwap := LoopInContract{
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
		HtlcConfTarget: 2,
	}

	// checkSwap is a test helper function that'll assert the state of a
	// swap.
	checkSwap := func(expectedState SwapState) {
		t.Helper()

		swaps, err := store.FetchLoopInSwaps()
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

	// If we create a new swap, then it should show up as being initialized
	// right after.
	if err := store.CreateLoopIn(hash, &pendingSwap); err != nil {
		t.Fatal(err)
	}
	checkSwap(StateInitiated)

	// Trying to make the same swap again should result in an error.
	if err := store.CreateLoopIn(hash, &pendingSwap); err == nil {
		t.Fatal("expected error on storing duplicate")
	}
	checkSwap(StateInitiated)

	// Next, we'll update to the next state of the pre-image being
	// revealed. The state should be reflected here again.
	err = store.UpdateLoopIn(
		hash, testTime, StatePreimageRevealed,
	)
	if err != nil {
		t.Fatal(err)
	}
	checkSwap(StatePreimageRevealed)

	// Next, we'll update to the final state to ensure that the state is
	// properly updated.
	err = store.UpdateLoopIn(
		hash, testTime, StateFailInsufficientValue,
	)
	if err != nil {
		t.Fatal(err)
	}
	checkSwap(StateFailInsufficientValue)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// If we re-open the same store, then the state of the current swap
	// should be the same.
	store, err = NewBoltSwapStore(tempDirName)
	if err != nil {
		t.Fatal(err)
	}
	checkSwap(StateFailInsufficientValue)
}
