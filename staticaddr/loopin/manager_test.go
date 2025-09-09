package loopin

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type testCase struct {
	name        string
	deposits    []*deposit.Deposit
	targetValue btcutil.Amount
	csvExpiry   uint32
	blockHeight uint32
	expected    []*deposit.Deposit
	expectedErr string
}

// TestSelectDeposits tests the selectDeposits function, which selects
// deposits that can cover a target value while respecting the dust limit.
func TestSelectDeposits(t *testing.T) {
	d1, d2, d3, d4 := &deposit.Deposit{
		Value:              1_000_000,
		ConfirmationHeight: 5_000,
		AddressParams:      &address.Parameters{Expiry: 10_000},
	}, &deposit.Deposit{
		Value:              2_000_000,
		ConfirmationHeight: 5_001,
		AddressParams:      &address.Parameters{Expiry: 10_000},
	}, &deposit.Deposit{
		Value:              3_000_000,
		ConfirmationHeight: 5_002,
		AddressParams:      &address.Parameters{Expiry: 10_000},
	}, &deposit.Deposit{
		Value:              3_000_000,
		ConfirmationHeight: 5_003,
		AddressParams:      &address.Parameters{Expiry: 10_000},
	}
	d1.Hash = chainhash.Hash{1}
	d1.Index = 0
	d2.Hash = chainhash.Hash{2}
	d2.Index = 0
	d3.Hash = chainhash.Hash{3}
	d3.Index = 0
	d4.Hash = chainhash.Hash{4}
	d4.Index = 0

	testCases := []testCase{
		{
			name:        "single deposit exact target",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_000,
			expected:    []*deposit.Deposit{d1},
			expectedErr: "",
		},
		{
			name:        "prefer larger deposit when both cover",
			deposits:    []*deposit.Deposit{d1, d2},
			targetValue: 1_000_000,
			expected:    []*deposit.Deposit{d2},
			expectedErr: "",
		},
		{
			name:        "prefer largest among three when one is enough",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: 1_000_000,
			expected:    []*deposit.Deposit{d3},
			expectedErr: "",
		},
		{
			name:        "single deposit insufficient by 1",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_001,
			expected:    []*deposit.Deposit{},
			expectedErr: "not enough deposits to cover",
		},
		{
			name:        "target leaves exact dust limit change",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_000 - dustLimit,
			expected:    []*deposit.Deposit{d1},
			expectedErr: "",
		},
		{
			name:        "target leaves dust change (just over)",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_000 - dustLimit + 1,
			expected:    []*deposit.Deposit{},
			expectedErr: "not enough deposits to cover",
		},
		{
			name:        "all deposits exactly match target",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value,
			expected:    []*deposit.Deposit{d1, d2, d3},
			expectedErr: "",
		},
		{
			name:        "sum minus dust limit is allowed (change == dust)",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value - dustLimit,
			expected:    []*deposit.Deposit{d1, d2, d3},
			expectedErr: "",
		},
		{
			name:        "sum minus dust limit plus 1 is not allowed (dust change)",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value - dustLimit + 1,
			expected:    []*deposit.Deposit{},
			expectedErr: "not enough deposits to cover",
		},
		{
			name:        "tie by value, prefer earlier expiry",
			deposits:    []*deposit.Deposit{d3, d4},
			targetValue: d4.Value - dustLimit, // d3/d4 have the
			// same value but different expiration.
			expected:    []*deposit.Deposit{d3},
			expectedErr: "",
		},
		{
			name: "prefilter filters deposits close to expiry",
			deposits: func() []*deposit.Deposit {
				// dClose expires before
				// htlcExpiry+DepositHtlcDelta and must be
				// filtered out. dOK expires exactly at the
				// threshold and must be eligible.
				dClose := &deposit.Deposit{
					Value:              3_000_000,
					ConfirmationHeight: 3000,
					AddressParams: &address.Parameters{
						Expiry: 10,
					},
				}
				dClose.Hash = chainhash.Hash{5}
				dClose.Index = 0
				dOK := &deposit.Deposit{
					Value:              2_000_000,
					ConfirmationHeight: 3050,
					AddressParams: &address.Parameters{
						Expiry: 10_000,
					},
				}
				dOK.Hash = chainhash.Hash{6}
				dOK.Index = 0
				return []*deposit.Deposit{dClose, dOK}
			}(),
			targetValue: 1_000_000,
			csvExpiry:   1000,
			blockHeight: 3000,
			expected: func() []*deposit.Deposit {
				// Only dOK should be considered.
				// dClose is filtered.
				dOK := &deposit.Deposit{
					Value:              2_000_000,
					ConfirmationHeight: 3050,
					AddressParams: &address.Parameters{
						Expiry: 10_000,
					},
				}
				dOK.Hash = chainhash.Hash{6}
				dOK.Index = 0
				return []*deposit.Deposit{dOK}
			}(),
			expectedErr: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			selectedDeposits, err := SelectDeposits(
				tc.targetValue, tc.deposits, tc.blockHeight,
			)
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.expectedErr)
			}
			require.ElementsMatch(t, tc.expected, selectedDeposits)
		})
	}
}

// mockAddressManager implements AddressManager for tests.
type mockAddressManager struct {
	OurPkScript []byte
}

func (m *mockAddressManager) NewAddress(_ context.Context) (*address.Parameters,
	error) {

	return nil, nil
}

func (m *mockAddressManager) IsOurPkScript(pkScript []byte) bool {
	return bytes.Equal(pkScript, m.OurPkScript)
}

// mockDepositManager implements DepositManager for tests.
type mockDepositManager struct {
	byOutpoint map[string]*deposit.Deposit
}

func (m *mockDepositManager) GetAllDeposits(_ context.Context) (
	[]*deposit.Deposit, error) {

	return nil, nil
}

func (m *mockDepositManager) AllStringOutpointsActiveDeposits(_ []string,
	_ fsm.StateType) ([]*deposit.Deposit, bool) {

	return nil, false
}

func (m *mockDepositManager) TransitionDeposits(_ context.Context,
	_ []*deposit.Deposit, _ fsm.EventType, _ fsm.StateType) error {

	return nil
}

func (m *mockDepositManager) DepositsForOutpoints(_ context.Context,
	outpoints []string, ignoreUnknown bool) ([]*deposit.Deposit, error) {

	res := make([]*deposit.Deposit, 0, len(outpoints))
	for _, op := range outpoints {
		if d, ok := m.byOutpoint[op]; ok {
			res = append(res, d)
		}
	}
	return res, nil
}

func (m *mockDepositManager) GetActiveDepositsInState(_ fsm.StateType) (
	[]*deposit.Deposit, error) {

	return nil, nil
}

// mockStore implements StaticAddressLoopInStore for tests.
type mockStore struct {
	loopIns map[lntypes.Hash]*StaticAddressLoopIn
	mapIDs  map[lntypes.Hash][]deposit.ID
}

func (s *mockStore) CreateLoopIn(_ context.Context,
	_ *StaticAddressLoopIn) error {

	return nil
}

func (s *mockStore) UpdateLoopIn(_ context.Context,
	_ *StaticAddressLoopIn) error {

	return nil
}

func (s *mockStore) GetStaticAddressLoopInSwapsByStates(_ context.Context,
	_ []fsm.StateType) ([]*StaticAddressLoopIn, error) {

	return nil, nil
}
func (s *mockStore) IsStored(_ context.Context, _ lntypes.Hash) (bool, error) {
	return false, nil
}

func (s *mockStore) GetLoopInByHash(_ context.Context,
	swapHash lntypes.Hash) (*StaticAddressLoopIn, error) {

	li, ok := s.loopIns[swapHash]
	if !ok {
		return nil, nil
	}
	return li, nil
}
func (s *mockStore) SwapHashesForDepositIDs(_ context.Context,
	depositIDs []deposit.ID) (map[lntypes.Hash][]deposit.ID, error) {

	// Filter the prepared mapping to only include hashes that reference
	// any of the provided deposit IDs.
	idSet := make(map[deposit.ID]struct{}, len(depositIDs))
	for _, id := range depositIDs {
		idSet[id] = struct{}{}
	}
	res := make(map[lntypes.Hash][]deposit.ID)
	for h, ids := range s.mapIDs {
		for _, id := range ids {
			if _, ok := idSet[id]; ok {
				res[h] = ids
				break
			}
		}
	}

	return res, nil
}

// helper to create a deposit with specific outpoint and value.
func makeDeposit(h byte, index uint32, value btcutil.Amount) *deposit.Deposit {
	d := &deposit.Deposit{Value: value}
	d.Hash = chainhash.Hash{h}
	d.Index = index
	var id deposit.ID
	id[0] = h
	d.ID = id

	return d
}

// helper to outpoint string as used by txin.PreviousOutPoint.String().
func outpointString(d *deposit.Deposit) string {
	return wire.OutPoint{Hash: d.Hash, Index: d.Index}.String()
}

// build a sweep tx with given inputs and outputs.
func makeSweepTx(inputs []wire.OutPoint, outputs []*wire.TxOut) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	for _, in := range inputs {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: in})
	}
	for _, out := range outputs {
		tx.AddTxOut(out)
	}

	return tx
}

// TestCheckChange exercises all relevant scenarios for checkChange.
func TestCheckChange(t *testing.T) {
	ctx := context.Background()

	// Prepare a common change address and an alternate address.
	addrStr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx" // testnet

	changeAddr, err := btcutil.DecodeAddress(
		addrStr, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	changePkScript, err := txscript.PayToAddrScript(changeAddr)
	require.NoError(t, err)

	otherAddr := &address.Parameters{PkScript: []byte{0xcc, 0xdd}}
	serverAddr := &address.Parameters{PkScript: []byte{0xee, 0xff}}

	// Prepare swaps (loop-ins) with varying deposit totals and selections.
	// Helper to make a swap with deposits and selected amount.
	makeSwap := func(h byte, deposits []*deposit.Deposit,
		selected btcutil.Amount) (lntypes.Hash, *StaticAddressLoopIn) {

		var hash lntypes.Hash
		hash[0] = h
		li := &StaticAddressLoopIn{
			Deposits:       deposits,
			SelectedAmount: selected,
			ChangeAddress:  changeAddr,
		}
		return hash, li
	}

	// Deposits belonging to different swaps.
	s1d1 := makeDeposit(1, 0, 1000)
	s1d2 := makeDeposit(1, 1, 2000)
	s2d1 := makeDeposit(2, 0, 1500)
	s3d1 := makeDeposit(3, 0, 800)
	s4d1 := makeDeposit(4, 0, 900)

	// Swaps:
	// A: total 3000, selected 3000 => no change.
	hA, liA := makeSwap(10, []*deposit.Deposit{s1d1, s1d2}, 3000)
	// B: total 1500, selected 1000 => change 500.
	hB, liB := makeSwap(11, []*deposit.Deposit{s2d1}, 1000)
	// C: total 800, selected 400 => change 400.
	hC, liC := makeSwap(12, []*deposit.Deposit{s3d1}, 400)
	// D: total 900, selected 500 => change 400.
	hD, liD := makeSwap(13, []*deposit.Deposit{s4d1}, 500)

	// Mapping deposits -> swaps (by deposit IDs).
	mapIDs := map[lntypes.Hash][]deposit.ID{
		hA: {s1d1.ID, s1d2.ID},
		hB: {s2d1.ID},
		hC: {s3d1.ID},
		hD: {s4d1.ID},
	}

	loopIns := map[lntypes.Hash]*StaticAddressLoopIn{
		hA: liA,
		hB: liB,
		hC: liC,
		hD: liD,
	}

	// Common manager with mocked dependencies; will change inputs per test.
	mgr := &Manager{
		cfg: &Config{
			AddressManager: &mockAddressManager{
				OurPkScript: changePkScript,
			},
			DepositManager: &mockDepositManager{
				byOutpoint: map[string]*deposit.Deposit{},
			},
			Store: &mockStore{
				loopIns: loopIns,
				mapIDs:  mapIDs,
			},
		},
	}

	type testCase struct {
		name           string
		inDeps         []*deposit.Deposit // deposits referenced by tx inputs
		outputs        []*wire.TxOut      // outputs in sweep tx
		expectErr      bool
		expectedErrMsg string
	}

	cases := []testCase{
		{
			name:   "no change expected (selected == total)",
			inDeps: []*deposit.Deposit{s1d1, s1d2},
			// No change output required.
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
			},
		},
		{
			name:   "single swap change present",
			inDeps: []*deposit.Deposit{s2d1}, // B -> change 500
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
				{
					Value:    500,
					PkScript: changePkScript,
				},
			},
		},
		{
			name:   "multiple swaps different change amounts",
			inDeps: []*deposit.Deposit{s2d1, s3d1}, // B(500)+C(400)=900
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
				{
					Value:    900,
					PkScript: changePkScript,
				},
			},
		},
		{
			name:   "two swaps with identical change values sum correctly",
			inDeps: []*deposit.Deposit{s3d1, s4d1}, // C(400)+D(400)=800
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
				{
					Value:    800,
					PkScript: changePkScript,
				},
			},
		},
		{
			name:           "missing change output results in error",
			inDeps:         []*deposit.Deposit{s2d1}, // expect 500
			outputs:        []*wire.TxOut{},
			expectErr:      true,
			expectedErrMsg: "couldn't find expected change",
		},
		{
			name:   "wrong address for change output",
			inDeps: []*deposit.Deposit{s2d1}, // expect 500
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
				{
					Value:    500,
					PkScript: otherAddr.PkScript,
				},
			},
			expectErr:      true,
			expectedErrMsg: "couldn't find expected change",
		},
		{
			name:   "wrong amount for change output",
			inDeps: []*deposit.Deposit{s2d1}, // expect 500
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
				{
					Value:    400,
					PkScript: changePkScript,
				},
			},
			expectErr:      true,
			expectedErrMsg: "couldn't find expected change",
		},
		{
			name:   "mixed swaps some with change some without",
			inDeps: []*deposit.Deposit{s1d1, s1d2, s3d1}, // A(0)+C(400)=400
			outputs: []*wire.TxOut{
				{
					Value:    1337,
					PkScript: serverAddr.PkScript,
				},
				{
					Value:    400,
					PkScript: changePkScript,
				},
				{
					Value:    1000,
					PkScript: otherAddr.PkScript,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Prepare inputs mapping for deposit manager.
			mdm := &mockDepositManager{
				byOutpoint: map[string]*deposit.Deposit{},
			}
			inputs := make([]wire.OutPoint, 0, len(tc.inDeps))
			for _, d := range tc.inDeps {
				mdm.byOutpoint[outpointString(d)] = d
				inputs = append(
					inputs, wire.OutPoint{
						Hash:  d.Hash,
						Index: d.Index,
					},
				)
			}
			mgr.cfg.DepositManager = mdm

			tx := makeSweepTx(inputs, tc.outputs)
			err := mgr.checkChange(ctx, tx)
			if tc.expectErr {
				require.Error(t, err)
				if tc.expectedErrMsg != "" {
					require.ErrorContains(
						t, err, tc.expectedErrMsg,
					)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}
