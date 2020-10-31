package sweep

import (
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
)

func TestDeduceDestinations(t *testing.T) {
	wpkh := [20]byte{}
	addr1, err := btcutil.NewAddressWitnessPubKeyHash(wpkh[:], &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	wsh := [32]byte{}
	addr2, err := btcutil.NewAddressWitnessScriptHash(wsh[:], &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		name                                                    string
		amount, destAmount, feeOnlyDest, feeOnlyChange, feeBoth btcutil.Amount
		destAddr, changeAddr                                    btcutil.Address
		wantedDestinations                                      []destination
		wantError                                               bool
	}{
		{
			name:          "missing changeAddr",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   150,
			feeOnlyChange: 150,
			feeBoth:       200,
			destAddr:      addr1,
			changeAddr:    nil, // Error!
			wantError:     true,
		},
		{
			name:          "missing feeOnlyChange",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   150,
			feeOnlyChange: 0, // Error!
			feeBoth:       200,
			destAddr:      addr1,
			changeAddr:    addr2,
			wantError:     true,
		},
		{
			name:          "missing feeBoth",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   150,
			feeOnlyChange: 150,
			feeBoth:       0, // Error!
			destAddr:      addr1,
			changeAddr:    addr2,
			wantError:     true,
		},

		{
			name:        "exact amount was not requested",
			amount:      1000000,
			feeOnlyDest: 150,
			destAddr:    addr1,
			wantedDestinations: []destination{
				{
					addr:   addr1,
					amount: 1000000 - 150,
				},
			},
		},
		{
			name:          "exact amount and change",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   150,
			feeOnlyChange: 160,
			feeBoth:       200,
			destAddr:      addr1,
			changeAddr:    addr2,
			wantedDestinations: []destination{
				{
					addr:   addr1,
					amount: 950000,
				},
				{
					addr:   addr2,
					amount: 1000000 - 950000 - 200,
				},
			},
		},
		{
			name:          "fee for both outputs is too large, but can send to destAddr",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   50000,
			feeOnlyChange: 51000,
			feeBoth:       70000,
			destAddr:      addr1,
			changeAddr:    addr2,
			wantedDestinations: []destination{
				{
					addr:   addr1,
					amount: 950000,
				},
			},
		},
		{
			name:          "fee for both outputs does not left room for change output",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   45000,
			feeOnlyChange: 46000,
			feeBoth:       50000,
			destAddr:      addr1,
			changeAddr:    addr2,
			wantedDestinations: []destination{
				{
					addr:   addr1,
					amount: 950000,
				},
			},
		},
		{
			name:          "fee for both outputs makes change output below dust",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   45000,
			feeOnlyChange: 46000,
			feeBoth:       49990,
			destAddr:      addr1,
			changeAddr:    addr2,
			wantedDestinations: []destination{
				{
					addr:   addr1,
					amount: 950000,
				},
			},
		},
		{
			name:          "fee for both and for dest is too large, send everything to change address",
			amount:        1000000,
			destAmount:    950000,
			feeOnlyDest:   51000,
			feeOnlyChange: 52000,
			feeBoth:       53000,
			destAddr:      addr1,
			changeAddr:    addr2,
			wantedDestinations: []destination{
				{
					addr:   addr2,
					amount: 948000,
				},
			},
		},
	}

	for _, tc := range testCases {
		gotDestinations, err := deduceDestinations(
			tc.amount, tc.destAmount, tc.feeOnlyDest, tc.feeOnlyChange, tc.feeBoth,
			tc.destAddr, tc.changeAddr,
		)

		if tc.wantError {
			if err == nil {
				t.Errorf("case %q: wanted error, but there is no error", tc.name)
			}
			continue
		}

		if err != nil {
			t.Errorf("case %q: failed: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(tc.wantedDestinations, gotDestinations) {
			t.Errorf("case %q: wanted %v, got %v", tc.name, tc.wantedDestinations, gotDestinations)
		}
	}
}
