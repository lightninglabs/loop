package swapkit

import (
	"github.com/btcsuite/btcd/txscript"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // Bitcoin OP_HASH160.
)

// HTLCSuccessScript serializes Loop's Bitcoin HTLC success leaf. The key is
// x-only. This primitive accepts any payment hash, including the zero hash used
// for fee estimation; funded contract validation belongs to the caller.
func HTLCSuccessScript(receiver [32]byte, paymentHash Hash) ([]byte, error) {
	// #nosec G406 -- Bitcoin OP_HASH160 requires RIPEMD160.
	hash160 := ripemd160.New()
	_, _ = hash160.Write(paymentHash[:])
	return txscript.NewScriptBuilder().
		AddData(receiver[:]).
		AddOp(txscript.OP_CHECKSIGVERIFY).
		AddOp(txscript.OP_SIZE).AddInt64(32).
		AddOp(txscript.OP_EQUALVERIFY).
		AddOp(txscript.OP_HASH160).AddData(hash160.Sum(nil)).
		AddOp(txscript.OP_EQUALVERIFY).AddInt64(1).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).Script()
}

// CSVTimeoutScript serializes Loop's Bitcoin deposit refund leaf. The key is
// x-only. It preserves the caller's signed script number without applying
// contract policy; callers must validate expiry before funding an output.
func CSVTimeoutScript(sender [32]byte, expiry int64) ([]byte, error) {
	return timeoutScript(sender, expiry, txscript.OP_CHECKSEQUENCEVERIFY)
}

// CLTVTimeoutScript serializes Loop's V3 Bitcoin HTLC refund leaf. Like
// CSVTimeoutScript, it leaves expiry validation to the contract caller.
func CLTVTimeoutScript(sender [32]byte, expiry int64) ([]byte, error) {
	return timeoutScript(sender, expiry, txscript.OP_CHECKLOCKTIMEVERIFY)
}

func timeoutScript(sender [32]byte, expiry int64, opcode byte) ([]byte, error) {
	return txscript.NewScriptBuilder().AddData(sender[:]).
		AddOp(txscript.OP_CHECKSIGVERIFY).AddInt64(expiry).
		AddOp(opcode).Script()
}
