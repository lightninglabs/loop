package loopdb

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

// TestMigrationUpdates asserts that the swap updates migration is carried out
// correctly.
func TestMigrationUpdates(t *testing.T) {
	var (
		legacyDbVersion = Hex("00000003")
	)

	legacyDb := map[string]any{
		"metadata": map[string]any{
			"dbp": legacyDbVersion,
		},
		"loop-in": map[string]any{
			Hex(
				"acae09fec9020b7996042613eede68a9eaf29eb28c2" +
					"1ea9943b19e344365a4bb",
			): map[string]any{
				"contract": Hex(
					"161b25277262bdb5c7c2827b975b2cbc7eb" +
						"13e222b30cf88ea6daef4bcf22b" +
						"dac4116c23071472cb000000000" +
						"000ea6003f2f513a8fd7958b6a2" +
						"29dfb8835f6ab2c9c63cc3e1387" +
						"84d3e8c0e0ebbdd4e61033f26c4" +
						"0666977ed497eea4694d6dd3f07" +
						"dbcf037089234ff665cd0a07fea" +
						"329400007b8a00000000000059a" +
						"600000000000009ca000077a200" +
						"000006000000000000000000000" +
						"000000000000000000000000000" +
						"00000000000000000000",
				),
				"updates": map[string]any{
					Hex("0000000000000001"): Hex(
						"161b252772cb524508000000000" +
							"0000000000000000000" +
							"00000000000000000000",
					),
					Hex("0000000000000002"): Hex(
						"161b252837115e9b09fffffffff" +
							"fff1f6a000000000000" +
							"00000000000000000000",
					),
					Hex("0000000000000003"): Hex(
						"161b252ab670360d02000000000" +
							"00009ca000000000000" +
							"00000000000000000000",
					),
				},
			},
		},
		"uncharge-swaps": map[string]any{
			Hex(
				"c3b3d7a145dbd2bab5aa1f505305f31ee432fe23b08" +
					"01f065fac453dd9b1f923",
			): map[string]any{
				"contract": Hex(
					"161b2526643767387ca76e58c964a8f2b6c" +
						"0a13392b2dea93bde260226a263" +
						"fb836954054ed1756b000000000" +
						"000c350fd11016c6e6263727431" +
						"333337306e31703030723437757" +
						"07035366c767166383675356576" +
						"6135647868686c706c783037337" +
						"56a70676e397976797737613076" +
						"6a37746d3076787932766835767" +
						"164713277706578327572703079" +
						"63717a7279787139377a7675717" +
						"3703570373232733970686a6e6e" +
						"6e706c3778716e796a783533737" +
						"06863346c396735306b396e3478" +
						"36703761793577707539306b667" +
						"3397179397173717a353766676a" +
						"7a67676838343439377375716b3" +
						"83436787a3333336a713036736c" +
						"6b38637a3238726574663636727" +
						"96b7876396a746e6a3072683979" +
						"666a61707770656172657130713" +
						"96679797a666664676d68746879" +
						"73617370757565746e6b72306b3" +
						"2376370326173366a750269d66f" +
						"d2cea620dc06f1f7de7838f0c8b" +
						"145b82c7033080c398862f3421a" +
						"23230382cb637badbb07f9926a0" +
						"6ecd88b6150513ea0060dc8d6dc" +
						"1c1fb623926b0a0f000077d4000" +
						"00000000b458c00000000000005" +
						"f10000000000000024000077a22" +
						"c62637274317132717563326667" +
						"77737971376463617a73666e333" +
						"2636a7874667671647671366a6c" +
						"70706574fd0f016c6e626372743" +
						"530313834306e31703030723437" +
						"7570703563776561306732396d3" +
						"066743464643272616739787030" +
						"6e726d6a72396c33726b7a71703" +
						"7706a6c34337a6e6d6b64336c79" +
						"337364713877646d6b7a7571637" +
						"17a7279787139377a7675717370" +
						"356164787175387661686437307" +
						"43776747165777578366d6d6433" +
						"797763663976783573647671756" +
						"7753833327230676e3734667339" +
						"717939717371687467736366383" +
						"86e377664767136716e71307a65" +
						"7775366d7471616e326c7a306e7" +
						"534737a72376c6b36646d343673" +
						"336c78726572656e333972616b7" +
						"a6c777378346c61353873396677" +
						"3630356d6767766b766879716e7" +
						"433397139767373677778793675" +
						"717072367132737800000006000" +
						"00000000003f200000000000000" +
						"00161b25262710ce00",
				),
				"outgoing-chan-set": nil,
				"updates": map[string]any{
					Hex("0000000000000001"): Hex(
						"161b252a770e649b01000000000" +
							"0000539000000000000" +
							"00000000000000000001",
					),
					Hex("0000000000000002"): Hex(
						"161b252ab671bdd902000000000" +
							"00005f1000000000000" +
							"1a9c0000000000000003",
					),
				},
			},
		},
	}

	ctxb := context.Background()

	// Restore a legacy database.
	tempDirName, err := ioutil.TempDir("", "clientstore")
	require.NoError(t, err)
	defer os.RemoveAll(tempDirName)

	tempPath := filepath.Join(tempDirName, dbFileName)
	db, err := bbolt.Open(tempPath, 0600, nil)
	require.NoError(t, err)

	err = db.Update(func(tx *bbolt.Tx) error {
		return RestoreDB(tx, legacyDb)
	})

	// Close database regardless of update result.
	db.Close()

	// Assert update was successful.
	require.NoError(t, err)

	// Open db and migrate to the latest version.
	store, err := NewBoltSwapStore(tempDirName, &chaincfg.MainNetParams)
	require.NoError(t, err)

	// Fetch the legacy loop out swap and assert that the updates are still
	// there.
	outSwaps, err := store.FetchLoopOutSwaps(ctxb)
	require.NoError(t, err)

	outSwap := outSwaps[0]
	require.Len(t, outSwap.Events, 2)
	require.Equal(t, StateSuccess, outSwap.Events[1].State)

	// Fetch the legacy loop in swap and assert that the updates are still
	// there.
	inSwaps, err := store.FetchLoopInSwaps(ctxb)
	require.NoError(t, err)

	inSwap := inSwaps[0]
	require.Len(t, inSwap.Events, 3)
	require.Equal(t, StateSuccess, outSwap.Events[1].State)
}
