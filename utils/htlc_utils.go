package utils

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
)

// GetHtlc composes and returns the on-chain swap script.
func GetHtlc(hash lntypes.Hash, contract *loopdb.SwapContract,
	chainParams *chaincfg.Params) (*swap.Htlc, error) {

	switch GetHtlcScriptVersion(contract.ProtocolVersion) {
	case swap.HtlcV2:
		return swap.NewHtlcV2(
			contract.CltvExpiry, contract.HtlcKeys.SenderScriptKey,
			contract.HtlcKeys.ReceiverScriptKey, hash,
			chainParams,
		)

	case swap.HtlcV3:
		// Swaps that implement the new MuSig2 protocol will be expected
		// to use the 1.0RC2 MuSig2 key derivation scheme.
		muSig2Version := input.MuSig2Version040
		if contract.ProtocolVersion >= loopdb.ProtocolVersionMuSig2 {
			muSig2Version = input.MuSig2Version100RC2
		}

		return swap.NewHtlcV3(
			muSig2Version,
			contract.CltvExpiry,
			contract.HtlcKeys.SenderInternalPubKey,
			contract.HtlcKeys.ReceiverInternalPubKey,
			contract.HtlcKeys.SenderScriptKey,
			contract.HtlcKeys.ReceiverScriptKey,
			hash, chainParams,
		)
	}

	return nil, swap.ErrInvalidScriptVersion
}

// GetHtlcScriptVersion returns the correct HTLC script version for the passed
// protocol version.
func GetHtlcScriptVersion(
	protocolVersion loopdb.ProtocolVersion) swap.ScriptVersion {

	// If the swap was initiated before we had our v3 script, use v2.
	if protocolVersion < loopdb.ProtocolVersionHtlcV3 ||
		protocolVersion == loopdb.ProtocolVersionUnrecorded {

		return swap.HtlcV2
	}

	return swap.HtlcV3
}

// ObtainSwapPaymentAddr will retrieve the payment addr from the passed invoice.
//
// Deprecated: Use ObtainLoopOutPaymentAddr to support blinded swap invoices.
func ObtainSwapPaymentAddr(swapInvoice string, chainParams *chaincfg.Params) (
	*[32]byte, error) {

	swapPayReq, err := zpay32.Decode(swapInvoice, chainParams)
	if err != nil {
		return nil, err
	}

	payAddr, err := swapPayReq.PaymentAddr.UnwrapOrErr(
		fmt.Errorf("expected payment address for invoice"),
	)
	if err != nil {
		return nil, err
	}

	return &payAddr, nil
}

// ObtainLoopOutPaymentAddr returns the secret used to authenticate loop-out
// RPCs. Blinded invoices require a separately supplied, persisted secret.
// Ordinary invoices retain their invoice-derived secret, including old swaps.
func ObtainLoopOutPaymentAddr(contract *loopdb.LoopOutContract,
	chainParams *chaincfg.Params) (*[32]byte, error) {

	invoice, err := zpay32.Decode(contract.SwapInvoice, chainParams)
	if err != nil {
		return nil, err
	}

	if len(invoice.BlindedPaymentPaths) > 0 {
		if contract.PaymentAddr == nil {
			return nil, fmt.Errorf("missing blinded swap payment address")
		}
		return contract.PaymentAddr, nil
	}

	addr, err := invoice.PaymentAddr.UnwrapOrErr(
		fmt.Errorf("expected payment address for invoice"),
	)
	if err != nil {
		return nil, err
	}
	if contract.PaymentAddr != nil && *contract.PaymentAddr != addr {
		return nil, fmt.Errorf("swap payment address differs from invoice")
	}
	return &addr, nil
}
