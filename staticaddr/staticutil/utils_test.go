package staticutil

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapserverrpc"
	looptest "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// mustHash converts a hex string to a chainhash.Hash and panics on error.
func mustHash(t *testing.T, s string) chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromStr(s)
	require.NoError(t, err)
	return *h
}

func TestToPrevOuts_Success(t *testing.T) {
	// Prepare two distinct deposits with different outpoints and values.
	d1 := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  mustHash(t, "0000000000000000000000000000000000000000000000000000000000000001"),
			Index: 0,
		},
		Value: btcutil.Amount(12345),
	}

	d2 := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  mustHash(t, "1111111111111111111111111111111111111111111111111111111111111111"),
			Index: 7,
		},
		Value: btcutil.Amount(987654321),
	}

	pkScript := []byte{0x51, 0x21, 0x02, 0x52} // arbitrary bytes

	prevOuts, err := ToPrevOuts([]*deposit.Deposit{d1, d2}, pkScript)
	require.NoError(t, err)

	// We expect two entries.
	require.Len(t, prevOuts, 2)

	// Check the first outpoint mapping.
	txOut1, ok := prevOuts[d1.OutPoint]
	require.True(t, ok, "expected outpoint d1 to be present")
	require.EqualValues(t, int64(d1.Value), txOut1.Value)
	require.Equal(t, pkScript, txOut1.PkScript)

	// Check the second outpoint mapping.
	txOut2, ok := prevOuts[d2.OutPoint]
	require.True(t, ok, "expected outpoint d2 to be present")
	require.EqualValues(t, int64(d2.Value), txOut2.Value)
	require.Equal(t, pkScript, txOut2.PkScript)

	// Ensure the keys in the map are exactly the outpoints we provided.
	for op := range prevOuts {
		require.True(t, op == d1.OutPoint || op == d2.OutPoint)
	}
}

func TestToPrevOuts_DuplicateOutpoint(t *testing.T) {
	// Two deposits that share the exact same outpoint should cause an error.
	shared := wire.OutPoint{
		Hash:  mustHash(t, "2222222222222222222222222222222222222222222222222222222222222222"),
		Index: 2,
	}

	d1 := &deposit.Deposit{OutPoint: shared, Value: btcutil.Amount(100)}
	d2 := &deposit.Deposit{OutPoint: shared, Value: btcutil.Amount(200)}

	_, err := ToPrevOuts([]*deposit.Deposit{d1, d2}, []byte{0x00})
	require.Error(t, err)
}

func TestGetPrevoutInfo_ConversionAndSorting(t *testing.T) {
	// Helper to create a hash from string.
	must := func(s string) chainhash.Hash {
		h, err := chainhash.NewHashFromStr(s)
		require.NoError(t, err)
		return *h
	}

	// Choose txids such that after reversal, ordering is determined by the
	// last byte of the original hex string.
	txidA := must("0000000000000000000000000000000000000000000000000000000000000001")
	txidB := must("0000000000000000000000000000000000000000000000000000000000000002")

	pkScript := []byte{0xaa, 0xbb}

	prevOuts := map[wire.OutPoint]*wire.TxOut{
		{Hash: txidA, Index: 5}: {Value: 11, PkScript: pkScript},
		{Hash: txidA, Index: 2}: {Value: 22, PkScript: pkScript},
		{Hash: txidB, Index: 0}: {Value: 33, PkScript: pkScript},
	}

	infos := GetPrevoutInfo(prevOuts)

	// Expect deterministic ordering:
	// 1) All entries with txidA (..01) before txidB (..02) due to BIP-69
	//    compare on reversed hashes.
	// 2) Within txidA, index 2 before index 5.
	require.Len(t, infos, 3)

	require.Equal(t, &swapserverrpc.PrevoutInfo{
		TxidBytes:   txidA[:],
		OutputIndex: 2,
		Value:       22,
		PkScript:    pkScript,
	}, infos[0])

	require.Equal(t, &swapserverrpc.PrevoutInfo{
		TxidBytes:   txidA[:],
		OutputIndex: 5,
		Value:       11,
		PkScript:    pkScript,
	}, infos[1])

	require.Equal(t, &swapserverrpc.PrevoutInfo{
		TxidBytes:   txidB[:],
		OutputIndex: 0,
		Value:       33,
		PkScript:    pkScript,
	}, infos[2])
}

func TestBip69InputLess_SameHashIndexOrder(t *testing.T) {
	txid := make([]byte, 32)
	txid[31] = 0x7f // Arbitrary value.

	a := &swapserverrpc.PrevoutInfo{TxidBytes: txid, OutputIndex: 1}
	b := &swapserverrpc.PrevoutInfo{TxidBytes: txid, OutputIndex: 3}

	require.True(t, bip69inputLess(a, b))
	require.False(t, bip69inputLess(b, a))
}

func TestBip69InputLess_DifferentHashes(t *testing.T) {
	// txid1 ends with 0x01, txid2 ends with 0x02. After reversing for
	// comparison, txid1 should still come before txid2 in lexicographic
	// order.
	h1, _ := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000001")
	h2, _ := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000000002")

	a := &swapserverrpc.PrevoutInfo{TxidBytes: h1[:], OutputIndex: 9}
	b := &swapserverrpc.PrevoutInfo{TxidBytes: h2[:], OutputIndex: 0}

	require.True(t, bip69inputLess(a, b))
	require.False(t, bip69inputLess(b, a))
}

func TestCreateMusig2Session_Success(t *testing.T) {
	// Set up mock signer from loop/test package.
	lnd := looptest.NewMockLnd()
	signer := lnd.Signer

	// Create dummy key material for address parameters.
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	params := &address.Parameters{
		ClientPubkey: clientKey.PubKey(),
		ServerPubkey: serverKey.PubKey(),
		Expiry:       10,
		PkScript:     []byte{0x51},
		KeyLocator:   keychain.KeyLocator{Family: 1, Index: 2},
	}

	// Build a static address for tweak options.
	staticAddr, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(params.Expiry), params.ClientPubkey, params.ServerPubkey,
	)
	require.NoError(t, err)

	sess, err := CreateMusig2Session(context.Background(), signer, params, staticAddr)
	require.NoError(t, err)
	require.NotNil(t, sess)
}

func TestCreateMusig2Sessions_Multiple(t *testing.T) {
	lnd := looptest.NewMockLnd()
	signer := lnd.Signer

	// Keys/params/static address.
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	params := &address.Parameters{
		ClientPubkey: clientKey.PubKey(),
		ServerPubkey: serverKey.PubKey(),
		Expiry:       12,
		PkScript:     []byte{0xaa},
		KeyLocator:   keychain.KeyLocator{Family: 9, Index: 8},
	}

	staticAddr, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(params.Expiry), params.ClientPubkey, params.ServerPubkey,
	)
	require.NoError(t, err)

	// Prepare N deposits; only the length matters for session count.
	deposits := []*deposit.Deposit{
		{OutPoint: wire.OutPoint{Index: 0}},
		{OutPoint: wire.OutPoint{Index: 1}},
		{OutPoint: wire.OutPoint{Index: 2}},
	}

	sessions, nonces, err := CreateMusig2Sessions(
		context.Background(), signer, deposits, params, staticAddr,
	)
	require.NoError(t, err)
	require.Len(t, sessions, len(deposits))
	require.Len(t, nonces, len(deposits))

	// The mock signer returns a zero-value PublicNonce; assert consistency.
	for i := range sessions {
		require.NotNil(t, sessions[i])
		require.True(t, bytes.Equal(nonces[i], sessions[i].PublicNonce[:]))
	}
}
