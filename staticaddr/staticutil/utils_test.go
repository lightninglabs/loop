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
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

// makeDeposit creates a deposit with the given value for testing.
func makeDeposit(value btcutil.Amount) *deposit.Deposit {
	return &deposit.Deposit{Value: value}
}

// makeDeposits creates a slice of deposits with the given values.
func makeDeposits(values ...btcutil.Amount) []*deposit.Deposit {
	deps := make([]*deposit.Deposit, len(values))
	for i, v := range values {
		deps[i] = makeDeposit(v)
	}
	return deps
}

// depositSum returns the total value of the given deposits.
func depositSum(deps []*deposit.Deposit) btcutil.Amount {
	var total btcutil.Amount
	for _, d := range deps {
		total += d.Value
	}
	return total
}

func TestSelectDeposits(t *testing.T) {
	t.Parallel()

	dustLimit := lnwallet.DustLimitForSize(input.P2TRSize)

	// Standard fee rate: 1 sat/vbyte = 250 sat/kw.
	lowFeeRate := chainfee.SatPerKVByte(1000).FeePerKWeight()

	// High fee rate: 100 sat/vbyte = 25000 sat/kw.
	highFeeRate := chainfee.SatPerKVByte(100_000).FeePerKWeight()

	anchors := lnrpc.CommitmentType_ANCHORS
	taproot := lnrpc.CommitmentType_SIMPLE_TAPROOT

	tests := []struct {
		name           string
		deposits       []*deposit.Deposit
		amount         int64
		feeRate        chainfee.SatPerKWeight
		commitmentType lnrpc.CommitmentType
		wantErr        string
		wantCount      int
		// validate runs extra assertions on the result.
		validate func(t *testing.T, selected []*deposit.Deposit)
	}{
		{
			name:           "insufficient total funds",
			deposits:       makeDeposits(1_000, 2_000),
			amount:         1_000_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantErr:        "insufficient funds",
		},
		{
			name:           "total equals amount but no room for dust",
			deposits:       makeDeposits(100_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantErr:        "insufficient funds",
		},
		{
			name: "total covers amount and dust but not fees",
			// 1 input high fee = 15400. Need 50k + 15400 +
			// 330 = 65730. Deposit = 51k passes the early
			// check (51k >= 50k + 330) but the loop finds
			// 51k < 65730 and returns an error.
			deposits:       makeDeposits(51_000),
			amount:         50_000,
			feeRate:        highFeeRate,
			commitmentType: anchors,
			wantErr:        "insufficient funds",
		},
		{
			name: "many tiny deposits don't block large selection",
			// Two large deposits easily cover 400k + fees.
			// Many tiny deposits should not cause a false
			// rejection in the early check.
			deposits: append(
				makeDeposits(300_000, 200_000),
				makeDeposits(
					100, 100, 100, 100, 100,
					100, 100, 100, 100, 100,
				)...,
			),
			amount:         400_000,
			feeRate:        highFeeRate,
			commitmentType: anchors,
			wantCount:      2,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				require.Equal(
					t, btcutil.Amount(300_000),
					selected[0].Value,
				)
				require.Equal(
					t, btcutil.Amount(200_000),
					selected[1].Value,
				)
			},
		},
		{
			name:           "single deposit covers amount plus fee and dust",
			deposits:       makeDeposits(500_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantCount:      1,
		},
		{
			name:           "two deposits needed when first is insufficient",
			deposits:       makeDeposits(60_000, 60_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantCount:      2,
		},
		{
			name:           "selects largest deposits first",
			deposits:       makeDeposits(10_000, 200_000, 50_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantCount:      1,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				// Should pick the 200k deposit.
				require.Equal(
					t, btcutil.Amount(200_000),
					selected[0].Value,
				)
			},
		},
		{
			name: "fee-awareness selects extra deposit",
			// With 2 inputs at high fee: need 50k + 21150 +
			// 330 = 71480. Two deposits of 36k = 72k which
			// is just above. But a single 36k deposit = 36k
			// < 50k + 15400 + 330 = 65730, so 1 is not
			// enough. Now make it tighter: amount = 50k,
			// deposits = [35_500, 35_500, 10_000].
			// 1 input: need 50k + 15400 + 330 = 65730. 35.5k
			//   < 65730 -> not enough.
			// 2 inputs: need 50k + 21150 + 330 = 71480.
			//   35.5k + 35.5k = 71k < 71480 -> not enough!
			// 3 inputs: need 50k + 26900 + 330 = 77230.
			//   35.5k + 35.5k + 10k = 81k >= 77230 -> enough.
			deposits:       makeDeposits(35_500, 35_500, 10_000),
			amount:         50_000,
			feeRate:        highFeeRate,
			commitmentType: anchors,
			wantCount:      3,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				total := depositSum(selected)
				fee := estimateFee(
					len(selected), highFeeRate,
					anchors,
				)
				require.GreaterOrEqual(
					t, total,
					btcutil.Amount(50_000)+fee+dustLimit,
				)
			},
		},
		{
			name:           "all deposits selected when all are needed",
			deposits:       makeDeposits(40_000, 40_000, 40_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantCount:      3,
		},
		{
			name:           "zero fee rate means only dust matters",
			deposits:       makeDeposits(100_000, 50_000),
			amount:         99_000,
			feeRate:        0,
			commitmentType: anchors,
			wantCount:      1,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				// With zero fee, 100k covers 99k + 0 + dust.
				require.Equal(
					t, btcutil.Amount(100_000),
					selected[0].Value,
				)
			},
		},
		{
			name:           "high fee rate forces more deposits",
			deposits:       makeDeposits(200_000, 100_000, 50_000),
			amount:         100_000,
			feeRate:        highFeeRate,
			commitmentType: anchors,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				total := depositSum(selected)
				fee := estimateFee(
					len(selected), highFeeRate,
					anchors,
				)
				require.GreaterOrEqual(
					t, total,
					btcutil.Amount(100_000)+fee+dustLimit,
				)
			},
		},
		{
			name:           "taproot commitment type",
			deposits:       makeDeposits(500_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: taproot,
			wantCount:      1,
		},
		{
			name: "many small deposits accumulate",
			deposits: makeDeposits(
				10_000, 10_000, 10_000, 10_000, 10_000,
				10_000, 10_000, 10_000, 10_000, 10_000,
			),
			amount:         50_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				total := depositSum(selected)
				fee := estimateFee(
					len(selected), lowFeeRate,
					anchors,
				)
				require.GreaterOrEqual(
					t, total,
					btcutil.Amount(50_000)+fee+dustLimit,
				)
				// With 10k each and low fees, we need at
				// least 6 (50k + dust + fee).
				require.GreaterOrEqual(
					t, len(selected), 6,
				)
			},
		},
		{
			name: "selection result satisfies fee invariant",
			deposits: makeDeposits(
				80_000, 70_000, 60_000, 50_000,
			),
			amount:         150_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				total := depositSum(selected)
				fee := estimateFee(
					len(selected), lowFeeRate,
					anchors,
				)
				// Core invariant: selected amount covers
				// requested amount + fee + dust.
				require.GreaterOrEqual(
					t, total,
					btcutil.Amount(150_000)+fee+dustLimit,
				)
			},
		},
		{
			name: "deposits sorted descending before selection",
			// Give deposits in ascending order; verify largest
			// are picked first.
			deposits:       makeDeposits(10_000, 20_000, 300_000),
			amount:         100_000,
			feeRate:        lowFeeRate,
			commitmentType: anchors,
			wantCount:      1,
			validate: func(t *testing.T, selected []*deposit.Deposit) {
				require.Equal(
					t, btcutil.Amount(300_000),
					selected[0].Value,
				)
			},
		},
		{
			name: "high fee eats into margin requiring extra deposit",
			// Two deposits of 60k each = 120k total.
			// Amount = 50k. With low fee: 60k > 50k + fee +
			// dust, so 1 deposit suffices.
			// With high fee: 60k < 50k + ~10k fee + dust,
			// so 2 deposits needed.
			deposits:       makeDeposits(60_000, 60_000),
			amount:         50_000,
			feeRate:        highFeeRate,
			commitmentType: anchors,
			wantCount:      2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := SelectDeposits(
				tc.deposits, tc.amount, tc.feeRate,
				tc.commitmentType,
			)

			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, selected)

			if tc.wantCount > 0 {
				require.Len(t, selected, tc.wantCount)
			}

			// Universal invariant: selected deposits must
			// cover amount + fee + dust.
			total := depositSum(selected)
			fee := estimateFee(
				len(selected), tc.feeRate,
				tc.commitmentType,
			)
			require.GreaterOrEqual(
				t, total,
				btcutil.Amount(tc.amount)+fee+dustLimit,
				"selection must cover amount + fee + dust",
			)

			if tc.validate != nil {
				tc.validate(t, selected)
			}
		})
	}
}
