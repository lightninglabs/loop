package staticutil

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/outpoint"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ToPrevOuts converts a slice of deposits to a map of outpoints to TxOuts.
//
// Each deposit carries the static address parameters that produced its output.
// Using the per-deposit script here keeps signing correct when one transaction
// spends deposits from multiple static addresses.
func ToPrevOuts(deposits []*deposit.Deposit) (
	map[wire.OutPoint]*wire.TxOut, error) {

	outpoints := make([]wire.OutPoint, len(deposits))
	for i, d := range deposits {
		outpoints[i] = d.OutPoint
	}
	if duplicate, ok := outpoint.FirstDuplicate(outpoints); ok {
		return nil, fmt.Errorf("duplicate outpoint %v", duplicate)
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(deposits))
	for i, d := range deposits {
		outpoint := outpoints[i]
		if d.AddressParams == nil {
			return nil, fmt.Errorf("missing static address "+
				"parameters for deposit %v", d.OutPoint)
		}
		txOut := &wire.TxOut{
			Value:    int64(d.Value),
			PkScript: d.AddressParams.PkScript,
		}
		prevOuts[outpoint] = txOut
	}

	return prevOuts, nil
}

// DepositClientPubkeys maps each deposit outpoint to the client static address
// pubkey that derives that output.
//
// The server receives this proof material with swap and withdrawal requests and
// verifies it against the L402's server key and expiry before co-signing any
// input.
func DepositClientPubkeys(deposits []*deposit.Deposit) (
	map[string][]byte, error) {

	clientPubkeys := make(map[string][]byte, len(deposits))
	for _, d := range deposits {
		if d.AddressParams == nil {
			return nil, fmt.Errorf("missing static address "+
				"parameters for deposit %v", d.OutPoint)
		}
		if d.AddressParams.ClientPubkey == nil {
			return nil, fmt.Errorf("missing static address client "+
				"pubkey for deposit %v", d.OutPoint)
		}

		depositKey := d.String()
		if _, ok := clientPubkeys[depositKey]; ok {
			return nil, fmt.Errorf("duplicate outpoint %v",
				depositKey)
		}

		clientPubkeys[depositKey] =
			d.AddressParams.ClientPubkey.SerializeCompressed()
	}

	return clientPubkeys, nil
}

// CreateMusig2Sessions creates a musig2 session for a number of deposits.
func CreateMusig2Sessions(ctx context.Context,
	signer lndclient.SignerClient, deposits []*deposit.Deposit) (
	[]*input.MuSig2SessionInfo,
	[][]byte, error) {

	musig2Sessions := make([]*input.MuSig2SessionInfo, len(deposits))
	clientNonces := make([][]byte, len(deposits))

	// Create the sessions and nonces from the deposits.
	for i := range len(deposits) {
		session, err := CreateMusig2Session(
			ctx, signer, deposits[i],
		)
		if err != nil {
			return nil, nil, err
		}

		musig2Sessions[i] = session
		clientNonces[i] = session.PublicNonce[:]
	}

	return musig2Sessions, clientNonces, nil
}

// CreateMusig2SessionsPerDeposit creates a musig2 session for a number of
// deposits and returns the sessions keyed by outpoint string.
//
// The per-deposit keying mirrors the server response format and avoids relying
// on positional ordering after the request crosses the wire.
func CreateMusig2SessionsPerDeposit(ctx context.Context,
	signer lndclient.SignerClient, deposits []*deposit.Deposit) (
	map[string]*input.MuSig2SessionInfo, map[string][]byte, map[string]int,
	error) {

	sessions := make(map[string]*input.MuSig2SessionInfo)
	nonces := make(map[string][]byte)
	depositToIdx := make(map[string]int)

	// Create the musig2 sessions for the sweepless sweep tx.
	for i, deposit := range deposits {
		depositKey := deposit.String()
		if _, ok := sessions[depositKey]; ok {
			return nil, nil, nil, fmt.Errorf("duplicate outpoint "+
				"%v", depositKey)
		}

		session, err := CreateMusig2Session(
			ctx, signer, deposit,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		sessions[depositKey] = session
		nonces[depositKey] = session.PublicNonce[:]
		depositToIdx[depositKey] = i
	}

	return sessions, nonces, depositToIdx, nil
}

// CreateMusig2Session creates a musig2 session for the deposit's static
// address.
func CreateMusig2Session(ctx context.Context,
	signer lndclient.SignerClient, d *deposit.Deposit) (
	*input.MuSig2SessionInfo, error) {

	if d.AddressParams == nil {
		return nil, fmt.Errorf("missing static address parameters "+
			"for deposit %v", d.OutPoint)
	}

	staticAddress, err := d.GetStaticAddressScript()
	if err != nil {
		return nil, err
	}

	addrParams := d.AddressParams

	signers := [][]byte{
		addrParams.ClientPubkey.SerializeCompressed(),
		addrParams.ServerPubkey.SerializeCompressed(),
	}

	expiryLeaf := staticAddress.TimeoutLeaf

	rootHash := expiryLeaf.TapHash()

	return signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &addrParams.KeyLocator,
		signers, lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
	)
}

// GetPrevoutInfo converts a map of prevOuts to protobuf.
func GetPrevoutInfo(prevOuts map[wire.OutPoint]*wire.TxOut,
) []*swapserverrpc.PrevoutInfo {

	prevoutInfos := make([]*swapserverrpc.PrevoutInfo, 0, len(prevOuts))

	for outpoint, txOut := range prevOuts {
		prevoutInfo := &swapserverrpc.PrevoutInfo{
			TxidBytes:   outpoint.Hash[:],
			OutputIndex: outpoint.Index,
			Value:       uint64(txOut.Value),
			PkScript:    txOut.PkScript,
		}
		prevoutInfos = append(prevoutInfos, prevoutInfo)
	}

	// Sort UTXOs by txid:index using BIP-0069 rule. The function is used
	// in unit tests a lot, and it is useful to make it deterministic.
	sort.Slice(prevoutInfos, func(i, j int) bool {
		return bip69inputLess(prevoutInfos[i], prevoutInfos[j])
	})

	return prevoutInfos
}

// bip69inputLess returns true if input1 < input2 according to BIP-0069
// First sort based on input hash (reversed / rpc-style), then index.
// The code is based on btcd/btcutil/txsort/txsort.go.
func bip69inputLess(input1, input2 *swapserverrpc.PrevoutInfo) bool {
	// Input hashes are the same, so compare the index.
	var ihash, jhash chainhash.Hash
	copy(ihash[:], input1.TxidBytes)
	copy(jhash[:], input2.TxidBytes)
	if ihash == jhash {
		return input1.OutputIndex < input2.OutputIndex
	}

	// At this point, the hashes are not equal, so reverse them to
	// big-endian and return the result of the comparison.
	const hashSize = chainhash.HashSize
	for b := range hashSize / 2 {
		ihash[b], ihash[hashSize-1-b] = ihash[hashSize-1-b], ihash[b]
		jhash[b], jhash[hashSize-1-b] = jhash[hashSize-1-b], jhash[b]
	}
	return bytes.Compare(ihash[:], jhash[:]) == -1
}

// SelectDeposits sorts the deposits by amount in descending order. It then
// selects the deposits that are needed to cover the requested amount plus
// transaction fees and dust. The fee rate and commitment type are used to
// estimate the transaction fee for the current selection, since each
// additional input increases the fee.
func SelectDeposits(deposits []*deposit.Deposit, amount int64,
	feeRate chainfee.SatPerKWeight,
	commitmentType lnrpc.CommitmentType) ([]*deposit.Deposit, error) {

	dustLimit := lnwallet.DustLimitForSize(input.P2TRSize)

	// Quick check: if total deposits can't even cover amount + dust
	// (ignoring fees), there's no way to succeed.
	var depositSum btcutil.Amount
	for _, d := range deposits {
		depositSum += d.Value
	}
	if depositSum < btcutil.Amount(amount)+dustLimit {
		return nil, fmt.Errorf("insufficient funds to cover swap " +
			"amount, try manually selecting deposits")
	}

	// Sort the deposits by amount in descending order.
	sort.Slice(deposits, func(i, j int) bool {
		return deposits[i].Value > deposits[j].Value
	})

	// Select deposits until the total covers the requested amount plus
	// the estimated fee and dust reserve. We estimate the fee
	// pessimistically with a change output to ensure we always select
	// enough.
	var selectedDeposits []*deposit.Deposit
	var selectedAmount btcutil.Amount
	for _, d := range deposits {
		selectedDeposits = append(selectedDeposits, d)
		selectedAmount += d.Value

		fee := estimateFee(
			len(selectedDeposits), feeRate, commitmentType,
		)

		if selectedAmount >= btcutil.Amount(amount)+fee+dustLimit {
			return selectedDeposits, nil
		}
	}

	// We exhausted all deposits without meeting the threshold.
	return nil, fmt.Errorf("insufficient funds to cover swap " +
		"amount plus fees, try manually selecting deposits")
}

// estimateFee returns the estimated fee for a transaction with the given
// number of taproot keyspend inputs and a single output determined by
// the commitment type. It includes a change output in the estimate to
// be conservative.
func estimateFee(numInputs int, feeRate chainfee.SatPerKWeight,
	commitmentType lnrpc.CommitmentType) btcutil.Amount {

	var we input.TxWeightEstimator
	for range numInputs {
		we.AddTaprootKeySpendInput(txscript.SigHashDefault)
	}

	// Add the funding output based on commitment type.
	switch commitmentType {
	case lnrpc.CommitmentType_SIMPLE_TAPROOT,
		lnrpc.CommitmentType_TAPROOT:
		we.AddP2TROutput()

	default:
		we.AddP2WSHOutput()
	}

	// Add a change output (P2TR) to be conservative.
	we.AddP2TROutput()

	return feeRate.FeeForWeight(we.Weight())
}
