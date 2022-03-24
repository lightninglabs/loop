package swap

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// assertEngineExecution executes the VM returned by the newEngine closure,
// asserting the result matches the validity expectation. In the case where it
// doesn't match the expectation, it executes the script step-by-step and
// prints debug information to stdout.
// This code is adopted from: lnd/input/script_utils_test.go
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
		if err != nil {
			t.Fatalf("stepping (%v)\n", err)
		}
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

// TestHtlcV2 tests the HTLC V2 script success and timeout spend cases.
func TestHtlcV2(t *testing.T) {
	const (
		htlcValue      = btcutil.Amount(1 * 10e8)
		testCltvExpiry = 24
	)

	var (
		testPreimage = lntypes.Preimage([32]byte{1, 2, 3})
		err          error
	)

	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	fundingOut := &wire.OutPoint{
		Hash:  chainhash.Hash(sha256.Sum256([]byte{1, 2, 3})),
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(fakeFundingTxIn)
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    int64(htlcValue),
		},
	)

	// Create sender and receiver keys.
	senderPrivKey, senderPubKey := test.CreateKey(1)
	receiverPrivKey, receiverPubKey := test.CreateKey(2)

	hash := sha256.Sum256(testPreimage[:])

	// Create the htlc.
	htlc, err := NewHtlc(
		HtlcV2, testCltvExpiry, senderPubKey.SerializeCompressed(), receiverPubKey.SerializeCompressed(), hash,
		HtlcP2WSH, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	// Create the htlc output we'll try to spend.
	htlcOutput := &wire.TxOut{
		Value:    int64(htlcValue),
		PkScript: htlc.PkScript,
	}

	// Create signers for sender and receiver.
	senderSigner := &input.MockSigner{
		Privkeys: []*btcec.PrivateKey{senderPrivKey},
	}
	receiverSigner := &input.MockSigner{
		Privkeys: []*btcec.PrivateKey{receiverPrivKey},
	}

	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		htlc.PkScript, int64(htlcValue),
	)

	signTx := func(tx *wire.MsgTx, pubkey *btcec.PublicKey,
		signer *input.MockSigner) (input.Signature, error) {

		signDesc := &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: pubkey,
			},

			WitnessScript: htlc.Script(),
			Output:        htlcOutput,
			HashType:      txscript.SigHashAll,
			SigHashes: txscript.NewTxSigHashes(
				tx, prevOutFetcher,
			),
			InputIndex: 0,
		}

		return signer.SignOutputRaw(tx, signDesc)
	}

	testCases := []struct {
		name    string
		witness func(*testing.T) wire.TxWitness
		valid   bool
	}{
		{
			// Receiver can spend with valid preimage.
			"success case spend with valid preimage",
			func(t *testing.T) wire.TxWitness {
				sweepTx.TxIn[0].Sequence = htlc.SuccessSequence()
				sweepSig, err := signTx(
					sweepTx, receiverPubKey, receiverSigner,
				)
				require.NoError(t, err)

				witness, err := htlc.GenSuccessWitness(
					sweepSig.Serialize(), testPreimage,
				)
				require.NoError(t, err)

				return witness

			}, true,
		},
		{
			// Receiver can't spend with the valid preimage and with
			// zero sequence.
			"success case no spend with valid preimage and zero sequence",
			func(t *testing.T) wire.TxWitness {
				sweepTx.TxIn[0].Sequence = 0
				sweepSig, err := signTx(
					sweepTx, receiverPubKey, receiverSigner,
				)
				require.NoError(t, err)

				witness, err := htlc.GenSuccessWitness(
					sweepSig.Serialize(), testPreimage,
				)
				require.NoError(t, err)

				return witness
			}, false,
		},
		{
			// Sender can't spend when haven't yet timed out.
			"timeout case no spend before timeout",
			func(t *testing.T) wire.TxWitness {
				sweepTx.LockTime = testCltvExpiry - 1
				sweepSig, err := signTx(
					sweepTx, senderPubKey, senderSigner,
				)
				require.NoError(t, err)

				return htlc.GenTimeoutWitness(
					sweepSig.Serialize(),
				)
			}, false,
		},
		{
			// Sender can spend after timeout.
			"timeout case spend after timeout",
			func(t *testing.T) wire.TxWitness {
				sweepTx.LockTime = testCltvExpiry
				sweepSig, err := signTx(
					sweepTx, senderPubKey, senderSigner,
				)
				require.NoError(t, err)

				return htlc.GenTimeoutWitness(
					sweepSig.Serialize(),
				)
			}, true,
		},
		{
			// Receiver can't spend after timeout.
			"timeout case receiver cannot spend",
			func(t *testing.T) wire.TxWitness {
				sweepTx.LockTime = testCltvExpiry
				sweepSig, err := signTx(
					sweepTx, receiverPubKey, receiverSigner,
				)
				require.NoError(t, err)

				return htlc.GenTimeoutWitness(
					sweepSig.Serialize(),
				)
			}, false,
		},
		{
			// Sender can't spend after timeout with wrong sender
			// key.
			"timeout case cannot spend with wrong key",
			func(t *testing.T) wire.TxWitness {
				bogusKey := []byte{0xb, 0xa, 0xd}

				// Create the htlc with the bogus key.
				htlc, err = NewHtlc(
					HtlcV2, testCltvExpiry,
					bogusKey, receiverPubKey.SerializeCompressed(), hash,
					HtlcP2WSH, &chaincfg.MainNetParams,
				)
				require.NoError(t, err)

				// Create the htlc output we'll try to spend.
				htlcOutput = &wire.TxOut{
					Value:    int64(htlcValue),
					PkScript: htlc.PkScript,
				}

				sweepTx.LockTime = testCltvExpiry
				sweepSig, err := signTx(
					sweepTx, senderPubKey, senderSigner,
				)
				require.NoError(t, err)

				return htlc.GenTimeoutWitness(
					sweepSig.Serialize(),
				)
			}, false,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			sweepTx.TxIn[0].Witness = testCase.witness(t)

			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					htlc.PkScript, sweepTx, 0,
					txscript.StandardVerifyFlags, nil,
					nil, int64(htlcValue), prevOutFetcher)
			}

			assertEngineExecution(t, testCase.valid, newEngine)
		})
	}
}

func CreateKey(index int32) (*btcec.PrivateKey, *btcec.PublicKey) {
	// Avoid all zeros, because it results in an invalid key.
	privKey, pubKey := btcec.PrivKeyFromBytes(
		[]byte{0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, byte(index + 1)})
	return privKey, pubKey
}

func TestHtlcV3(t *testing.T) {
	preimage := [32]byte{1, 2, 3}
	p := lntypes.Preimage(preimage)
	hashedPreimage := sha256.Sum256(p[:])
	value := int64(800_000 - 500) // TODO(guggero): Calculate actual fee.

	senderPrivKey, senderPubKey := CreateKey(1)
	receiverPrivKey, receiverPubKey := CreateKey(2)

	cltvExpiry := int32(10)

	shnorrSenderKey := schnorr.SerializePubKey(senderPubKey)
	schnorrReceiverKey := schnorr.SerializePubKey(receiverPubKey)

	htlc, err := NewHtlc(
		HtlcV3, cltvExpiry, shnorrSenderKey, schnorrReceiverKey, hashedPreimage, HtlcP2TR, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	var trAddress *btcutil.AddressTaproot
	trAddress, ok := htlc.Address.(*btcutil.AddressTaproot)
	require.True(t, ok)

	p2trPkScript, err := txscript.PayToAddrScript(trAddress)
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	tx.LockTime = uint32(cltvExpiry)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash(sha256.Sum256([]byte{1, 2, 3})),
			Index: 50,
		},
		Sequence: 10,
	}}
	tx.TxOut = []*wire.TxOut{{
		PkScript: []byte{0, 20, 2, 141, 221, 230, 144, 171, 89, 230, 219, 198, 90, 157, 110, 89, 89, 67, 128, 16, 150, 186},
		Value:    value,
	}}

	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		p2trPkScript, 800_000,
	)
	hashCache := txscript.NewTxSigHashes(
		tx, prevOutFetcher,
	)

	signTx := func(
		tx *wire.MsgTx, privateKey *secp.PrivateKey, leaf txscript.TapLeaf) []byte {

		sig, err := txscript.RawTxInTapscriptSignature(
			tx, hashCache, 0, int64(value), p2trPkScript, leaf,
			txscript.SigHashDefault, privateKey,
		)
		require.NoError(t, err)

		return sig
	}

	testCases := []struct {
		name    string
		witness func(*testing.T) wire.TxWitness
		valid   bool
	}{
		{
			"claim path spend",
			func(t *testing.T) wire.TxWitness {
				tx.TxIn[0].Sequence = htlc.SuccessSequence()

				var trHtlc *HtlcScriptV3
				trHtlc, ok := htlc.HtlcScript.(*HtlcScriptV3)
				require.True(t, ok)

				sig := signTx(
					tx, senderPrivKey, txscript.NewBaseTapLeaf(trHtlc.ClaimScript),
				)

				return htlc.genSuccessWitness(sig, preimage)
			}, true,
		},
		{
			"timeout path spend",
			func(t *testing.T) wire.TxWitness {
				tx.TxIn[0].Sequence = htlc.SuccessSequence()

				var trHtlc *HtlcScriptV3
				trHtlc, ok := htlc.HtlcScript.(*HtlcScriptV3)
				require.True(t, ok)

				sig := signTx(
					tx, receiverPrivKey, txscript.NewBaseTapLeaf(trHtlc.TimeoutScript),
				)

				return htlc.GenTimeoutWitness(sig)
			}, true,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			tx.TxIn[0].Witness = testCase.witness(t)

			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					p2trPkScript, tx, 0,
					txscript.StandardVerifyFlags, nil,
					hashCache, int64(value), prevOutFetcher)
			}

			assertEngineExecution(t, testCase.valid, newEngine)
		})
	}
}
