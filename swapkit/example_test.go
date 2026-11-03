package swapkit_test

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapkit"
)

func ExampleNewSwapKit() {
	_, client := btcec.PrivKeyFromBytes([]byte{1})
	_, server := btcec.PrivKeyFromBytes([]byte{2})
	clientKey := swapkit.PubKey(client.SerializeCompressed())
	serverKey := swapkit.PubKey(server.SerializeCompressed())
	preimage := [32]byte{42}
	// In Loop Out the server sends assets and owns the refund path.
	kit, err := swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:            swapkit.SwapHTLCCLTV,
		SenderInternalKey: serverKey, ReceiverInternalKey: clientKey,
		SenderScriptKey: serverKey, ReceiverScriptKey: clientKey,
		PaymentHash: sha256.Sum256(preimage[:]), Expiry: 1_000_000,
	})
	if err != nil {
		panic(err)
	}
	spend, err := kit.TimeoutSpend(swapkit.Hash{1})
	if err != nil {
		panic(err)
	}
	fmt.Println(spend.Sequence, spend.LockTime)
	// Output: 4294967293 1000000
}

func ExampleNewDepositKit() {
	_, client := btcec.PrivKeyFromBytes([]byte{1})
	_, server := btcec.PrivKeyFromBytes([]byte{2})
	kit, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: swapkit.PubKey(client.SerializeCompressed()),
		ServerKey: swapkit.PubKey(server.SerializeCompressed()),
		CSVExpiry: 144,
	})
	if err != nil {
		panic(err)
	}
	spend, err := kit.TimeoutSpend(swapkit.Hash{1})
	if err != nil {
		panic(err)
	}
	fmt.Println(spend.Sequence, spend.LockTime)
	// Output: 144 0
}

func ExampleNewReservationKit() {
	_, client := btcec.PrivKeyFromBytes([]byte{1})
	_, server := btcec.PrivKeyFromBytes([]byte{2})
	kit, err := swapkit.NewReservationKit(swapkit.ReservationKitParams{
		ClientKey:    swapkit.PubKey(client.SerializeCompressed()),
		ServerKey:    swapkit.PubKey(server.SerializeCompressed()),
		RefundHeight: 1_000_000,
	})
	if err != nil {
		panic(err)
	}
	spend, err := kit.TimeoutSpend(swapkit.Hash{1})
	if err != nil {
		panic(err)
	}
	fmt.Println(spend.Sequence, spend.LockTime)
	// Output: 4294967293 1000000
}
