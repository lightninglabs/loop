package script

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestStaticAddressScript tests the taproot 2:2 multisig script success and CSV
// timeout spend cases.
func TestStaticAddressScript(t *testing.T) {
	var (
		value     int64 = 800_000
		csvExpiry int64 = 10

		version = input.MuSig2Version100RC2
	)

	clientPrivKey, clientPubKey := test.CreateKey(1)
	serverPrivKey, serverPubKey := test.CreateKey(2)

	// Keys used for the Musig2 session.
	privKeys := []*btcec.PrivateKey{clientPrivKey, serverPrivKey}
	pubKeys := []*btcec.PublicKey{clientPubKey, serverPubKey}

	// Create a new static address.
	staticAddress, err := NewStaticAddress(
		version, csvExpiry, clientPubKey, serverPubKey,
	)
	require.NoError(t, err)

	// Retrieve the static address pkScript.
	staticAddressScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	// Create a fake transaction. The prevOutFetcher will determine which
	// output the signer will fetch, independent of the tx.TxOut.
	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  sha256.Sum256([]byte{1, 2, 3}),
			Index: 50,
		},
	}}
	tx.TxOut = []*wire.TxOut{{
		PkScript: []byte{
			0, 20, 2, 141, 221, 230, 144,
			171, 89, 230, 219, 198, 90, 157,
			110, 89, 89, 67, 128, 16, 150, 186,
		},
		Value: value,
	}}

	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		staticAddressScript, value,
	)

	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	testCases := []struct {
		name    string
		witness func(*testing.T) wire.TxWitness
		valid   bool
	}{
		{
			"success case coop spend with combined internal key",
			func(t *testing.T) wire.TxWitness {
				tx.TxIn[0].Sequence = 1

				// This is what gets signed.
				taprootSigHash, err := txscript.CalcTaprootSignatureHash(
					sigHashes, txscript.SigHashDefault, tx, 0,
					prevOutFetcher,
				)
				require.NoError(t, err)

				var msg [32]byte
				copy(msg[:], taprootSigHash)

				tweak := &input.MuSig2Tweaks{
					TaprootTweak: staticAddress.RootHash[:],
				}

				sig, err := utils.MuSig2Sign(
					version, privKeys, pubKeys, tweak, msg,
				)
				require.NoError(t, err)

				witness, err := staticAddress.GenSuccessWitness(
					sig,
				)
				require.NoError(t, err)

				return witness
			}, true,
		},
		{
			"success case timeout spend with client key",
			func(t *testing.T) wire.TxWitness {
				tx.TxIn[0].Sequence = uint32(csvExpiry)

				sig, err := txscript.RawTxInTapscriptSignature(
					tx, sigHashes, 0, value,
					staticAddressScript,
					*staticAddress.TimeoutLeaf,
					txscript.SigHashAll, clientPrivKey,
				)
				require.NoError(t, err)

				witness, err := staticAddress.GenTimeoutWitness(
					sig,
				)
				require.NoError(t, err)

				return witness
			}, true,
		},
		{
			"error case timeout spend with client key, wrong " +
				"sequence",
			func(t *testing.T) wire.TxWitness {
				tx.TxIn[0].Sequence = uint32(csvExpiry - 1)

				sig, err := txscript.RawTxInTapscriptSignature(
					tx, sigHashes, 0, value,
					staticAddressScript,
					*staticAddress.TimeoutLeaf,
					txscript.SigHashAll, clientPrivKey,
				)
				require.NoError(t, err)

				witness, err := staticAddress.GenTimeoutWitness(
					sig,
				)
				require.NoError(t, err)

				return witness
			}, false,
		},
		{
			"error case timeout spend with server key, server " +
				"cannot claim timeout path",
			func(t *testing.T) wire.TxWitness {
				tx.TxIn[0].Sequence = uint32(csvExpiry)

				sig, err := txscript.RawTxInTapscriptSignature(
					tx, sigHashes, 0, value,
					staticAddressScript,
					*staticAddress.TimeoutLeaf,
					txscript.SigHashAll, serverPrivKey,
				)
				require.NoError(t, err)

				witness, err := staticAddress.GenTimeoutWitness(
					sig,
				)
				require.NoError(t, err)

				return witness
			}, false,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			tx.TxIn[0].Witness = testCase.witness(t)

			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					staticAddressScript, tx, 0,
					txscript.StandardVerifyFlags, nil,
					sigHashes, value, prevOutFetcher,
				)
			}

			assertEngineExecution(t, testCase.valid, newEngine)
		})
	}
}

// assertEngineExecution executes the VM returned by the newEngine closure,
// asserting the result matches the validity expectation. In the case where it
// doesn't match the expectation, it executes the script step-by-step and
// prints debug information to stdout.
// This code is adopted from: lnd/input/script_utils_test.go .
func assertEngineExecution(t *testing.T, valid bool,
	newEngine func() (*txscript.Engine, error)) {

	t.Helper()

	// Get a new VM to execute.
	vm, err := newEngine()
	require.NoError(t, err, "unable to create engine")

	// Execute the VM, only go on to the step-by-step execution if it
	// doesn't validate as expected.
	vmErr := vm.Execute()
	executionValid := vmErr == nil
	if valid == executionValid {
		return
	}

	// Now that the execution didn't match what we expected, fetch a new VM
	// to step through.
	vm, err = newEngine()
	require.NoError(t, err, "unable to create engine")

	// This buffer will trace execution of the Script, dumping out to
	// stdout.
	var debugBuf bytes.Buffer

	done := false
	for !done {
		dis, err := vm.DisasmPC()
		require.NoError(t, err, "stepping")
		debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

		done, err = vm.Step()
		if err != nil && valid {
			fmt.Println(debugBuf.String())
			t.Fatalf("spend test case failed, spend "+
				"should be valid: %v", err)
		} else if err == nil && !valid && done {
			fmt.Println(debugBuf.String())
			t.Fatalf("spend test case succeed, spend "+
				"should be invalid: %v", err)
		}

		debugBuf.WriteString(
			fmt.Sprintf("Stack: %v", vm.GetStack()),
		)
		debugBuf.WriteString(
			fmt.Sprintf("AltStack: %v", vm.GetAltStack()),
		)
	}

	// If we get to this point the unexpected case was not reached
	// during step execution, which happens for some checks, like
	// the clean-stack rule.
	validity := "invalid"
	if valid {
		validity = "valid"
	}

	fmt.Println(debugBuf.String())
	t.Fatalf(
		"%v spend test case execution ended with: %v", validity, vmErr,
	)
}
