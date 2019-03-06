package test

import (
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"golang.org/x/net/context"
)

type mockLightningClient struct {
	lnd *LndMockServices
	wg  sync.WaitGroup
}

// PayInvoice pays an invoice.
func (h *mockLightningClient) PayInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount,
	outgoingChannel *uint64) chan lndclient.PaymentResult {

	done := make(chan lndclient.PaymentResult, 1)

	mockChan := make(chan error)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		amt, err := utils.GetInvoiceAmt(&chaincfg.TestNet3Params, invoice)
		if err != nil {
			select {
			case done <- lndclient.PaymentResult{
				Err: err,
			}:
			case <-ctx.Done():
			}
			return
		}

		var paidFee btcutil.Amount

		err = <-mockChan
		if err != nil {
			amt = 0
		} else {
			paidFee = 1
		}

		select {
		case done <- lndclient.PaymentResult{
			Err:     err,
			PaidFee: paidFee,
			PaidAmt: amt,
		}:
		case <-ctx.Done():
		}
	}()

	h.lnd.SendPaymentChannel <- PaymentChannelMessage{
		PaymentRequest: invoice,
		Done:           mockChan,
	}

	return done
}

func (h *mockLightningClient) WaitForFinished() {
	h.wg.Wait()
}

func (h *mockLightningClient) ConfirmedWalletBalance(ctx context.Context) (
	btcutil.Amount, error) {

	return 1000000, nil
}

func (h *mockLightningClient) GetInfo(ctx context.Context) (*lndclient.Info,
	error) {

	var pubKey [33]byte
	return &lndclient.Info{
		BlockHeight:    600,
		IdentityPubkey: pubKey,
	}, nil
}

func (h *mockLightningClient) GetFeeEstimate(ctx context.Context, amt btcutil.Amount, dest [33]byte) (
	lnwire.MilliSatoshi, error) {

	return 0, nil
}

func (h *mockLightningClient) AddInvoice(ctx context.Context,
	in *invoicesrpc.AddInvoiceData) (lntypes.Hash, string, error) {

	h.lnd.lock.Lock()
	defer h.lnd.lock.Unlock()

	var hash lntypes.Hash
	if in.Hash != nil {
		hash = *in.Hash
	} else {
		hash = (*in.Preimage).Hash()
	}

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()

	payReq, err := zpay32.NewInvoice(
		h.lnd.ChainParams, hash, creationDate,
		zpay32.Description(in.Memo),
		zpay32.CLTVExpiry(in.CltvExpiry),
		zpay32.Amount(lnwire.MilliSatoshi(in.Value)),
	)
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	payReqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: func(hash []byte) ([]byte, error) {
				// btcec.SignCompact returns a pubkey-recoverable signature
				sig, err := btcec.SignCompact(
					btcec.S256(), privKey, hash, true,
				)
				if err != nil {
					return nil, fmt.Errorf("can't sign the hash: %v", err)
				}

				return sig, nil
			},
		},
	)
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	return hash, payReqString, nil
}
