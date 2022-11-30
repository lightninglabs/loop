package test

import (
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

var (
	// Timeout is the default timeout when tests wait for something to
	// happen.
	Timeout = time.Second * 5

	// ErrTimeout is returned on timeout.
	ErrTimeout = errors.New("test timeout")

	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)
)

// GetDestAddr deterministically generates a sweep address for testing.
func GetDestAddr(t *testing.T, nr byte) btcutil.Address {
	destAddr, err := btcutil.NewAddressScriptHash(
		[]byte{nr}, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	return destAddr
}

// EncodePayReq encodes a zpay32 invoice with a fixed key.
func EncodePayReq(payReq *zpay32.Invoice) (string, error) {
	privKey, _ := CreateKey(5)
	reqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: func(hash []byte) ([]byte, error) {
				// ecdsa.SignCompact returns a
				// pubkey-recoverable signature.
				sig, err := ecdsa.SignCompact(
					privKey, payReq.PaymentHash[:], true,
				)
				if err != nil {
					return nil, fmt.Errorf("can't sign "+
						"the hash: %v", err)
				}

				return sig, nil
			},
		},
	)
	if err != nil {
		return "", err
	}

	return reqString, nil
}

// GetInvoice creates a testnet payment request with the given parameters.
func GetInvoice(hash lntypes.Hash, amt btcutil.Amount, memo string) (
	string, error) {

	req, err := zpay32.NewInvoice(
		&chaincfg.TestNet3Params, hash, testTime,
		zpay32.Description(memo),
		zpay32.Amount(lnwire.NewMSatFromSatoshis(amt)),
	)
	if err != nil {
		return "", err
	}

	reqString, err := EncodePayReq(req)
	if err != nil {
		return "", err
	}

	return reqString, nil
}

// DumpGoroutines dumps all currently running goroutines.
func DumpGoroutines() {
	err := pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
	if err != nil {
		panic(err)
	}
}
