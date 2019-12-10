package test

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
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

	h.lnd.SendPaymentChannel <- PaymentChannelMessage{
		PaymentRequest: invoice,
		Done:           done,
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

func (h *mockLightningClient) EstimateFeeToP2WSH(ctx context.Context,
	amt btcutil.Amount, confTarget int32) (btcutil.Amount,
	error) {

	return 3000, nil
}

func (h *mockLightningClient) AddInvoice(ctx context.Context,
	in *invoicesrpc.AddInvoiceData) (lntypes.Hash, string, error) {

	h.lnd.lock.Lock()
	defer h.lnd.lock.Unlock()

	var hash lntypes.Hash
	switch {
	case in.Hash != nil:
		hash = *in.Hash
	case in.Preimage != nil:
		hash = (*in.Preimage).Hash()
	default:
		if _, err := rand.Read(hash[:]); err != nil {
			return lntypes.Hash{}, "", err
		}
	}

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()

	payReq, err := zpay32.NewInvoice(
		h.lnd.ChainParams, hash, creationDate,
		zpay32.Description(in.Memo),
		zpay32.CLTVExpiry(in.CltvExpiry),
		zpay32.Amount(in.Value),
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

// ListTransactions returns all known transactions of the backing lnd node.
func (h *mockLightningClient) ListTransactions(
	ctx context.Context) ([]*wire.MsgTx, error) {

	h.lnd.lock.Lock()
	txs := h.lnd.Transactions
	h.lnd.lock.Unlock()
	return txs, nil
}
