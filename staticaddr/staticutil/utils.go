package staticutil

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// ToPrevOuts converts a slice of deposits to a map of outpoints to TxOuts.
func ToPrevOuts(deposits []*deposit.Deposit) (map[wire.OutPoint]*wire.TxOut,
	error) {

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(deposits))
	for _, d := range deposits {
		outpoint := wire.OutPoint{
			Hash:  d.Hash,
			Index: d.Index,
		}
		txOut := &wire.TxOut{
			Value:    int64(d.Value),
			PkScript: d.AddressParams.PkScript,
		}
		if _, ok := prevOuts[outpoint]; ok {
			return nil, fmt.Errorf("duplicate outpoint %v",
				outpoint)
		}
		prevOuts[outpoint] = txOut
	}

	return prevOuts, nil
}

// CreateMusig2Sessions creates a musig2 session for a number of deposits.
func CreateMusig2Sessions(ctx context.Context,
	signer lndclient.SignerClient, deposits []*deposit.Deposit,
	addrParams *address.Parameters,
	staticAddress *script.StaticAddress) ([]*input.MuSig2SessionInfo,
	[][]byte, error) {

	musig2Sessions := make([]*input.MuSig2SessionInfo, len(deposits))
	clientNonces := make([][]byte, len(deposits))

	// Create the sessions and nonces from the deposits.
	for i := 0; i < len(deposits); i++ {
		session, err := createMusig2Session(
			ctx, signer, addrParams, staticAddress,
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
// deposits.
func CreateMusig2SessionsPerDeposit(ctx context.Context,
	signer lndclient.SignerClient, deposits []*deposit.Deposit) (
	map[string]*input.MuSig2SessionInfo, map[string][]byte, map[string]int,
	error) {

	sessions := make(map[string]*input.MuSig2SessionInfo)
	nonces := make(map[string][]byte)
	depositToIdx := make(map[string]int)

	// Create the musig2 sessions for the sweepless sweep tx.
	for i, deposit := range deposits {
		addressScript, err := deposit.GetStaticAddressScript()
		if err != nil {
			return nil, nil, nil, err
		}

		session, err := createMusig2Session(
			ctx, signer, deposit.AddressParams, addressScript,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		sessions[deposit.String()] = session
		nonces[deposit.String()] = session.PublicNonce[:]
		depositToIdx[deposit.String()] = i
	}

	return sessions, nonces, depositToIdx, nil
}

// createMusig2Session creates a musig2 session for the deposit.
func createMusig2Session(ctx context.Context,
	signer lndclient.SignerClient, addrParams *address.Parameters,
	staticAddress *script.StaticAddress) (*input.MuSig2SessionInfo, error) {

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
	for b := 0; b < hashSize/2; b++ {
		ihash[b], ihash[hashSize-1-b] = ihash[hashSize-1-b], ihash[b]
		jhash[b], jhash[hashSize-1-b] = jhash[hashSize-1-b], jhash[b]
	}
	return bytes.Compare(ihash[:], jhash[:]) == -1
}

// SelectDeposits sorts the deposits by amount in descending order. It then
// selects the deposits that are needed to cover the amount requested without
// leaving a dust change. It returns an error if the sum of deposits minus dust
// is less than the requested amount.
func SelectDeposits(deposits []*deposit.Deposit, amount int64) (
	[]*deposit.Deposit, error) {

	// Check that sum of deposits covers the swap amount while leaving no
	// dust change.
	dustLimit := lnwallet.DustLimitForSize(input.P2TRSize)
	var depositSum btcutil.Amount
	for _, deposit := range deposits {
		depositSum += deposit.Value
	}
	if depositSum-dustLimit < btcutil.Amount(amount) {
		return nil, fmt.Errorf("insufficient funds to cover swap " +
			"amount, try manually selecting deposits")
	}

	// Sort the deposits by amount in descending order.
	sort.Slice(deposits, func(i, j int) bool {
		return deposits[i].Value > deposits[j].Value
	})

	// Select the deposits that are needed to cover the swap amount without
	// leaving a dust change.
	var selectedDeposits []*deposit.Deposit
	var selectedAmount btcutil.Amount
	for _, deposit := range deposits {
		if selectedAmount >= btcutil.Amount(amount)+dustLimit {
			break
		}
		selectedDeposits = append(selectedDeposits, deposit)
		selectedAmount += deposit.Value
	}

	return selectedDeposits, nil
}
