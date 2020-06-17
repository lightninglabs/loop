package test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/channeldb"
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

	pubKeyBytes, err := hex.DecodeString(h.lnd.NodePubkey)
	if err != nil {
		return nil, err
	}
	var pubKey [33]byte
	copy(pubKey[:], pubKeyBytes)
	return &lndclient.Info{
		BlockHeight:    600,
		IdentityPubkey: pubKey,
		Uris:           []string{h.lnd.NodePubkey + "@127.0.0.1:9735"},
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

	// Add the invoice we have created to our mock's set of invoices.
	h.lnd.Invoices[hash] = &lndclient.Invoice{
		Preimage:       nil,
		Hash:           hash,
		PaymentRequest: payReqString,
		Amount:         in.Value,
		CreationDate:   creationDate,
		State:          channeldb.ContractOpen,
		IsKeysend:      false,
	}

	return hash, payReqString, nil
}

// LookupInvoice looks up an invoice in the mock's set of stored invoices.
// If it is not found, this call will fail. Note that these invoices should
// be settled using settleInvoice to have a preimage, settled state and settled
// date set.
func (h *mockLightningClient) LookupInvoice(_ context.Context,
	hash lntypes.Hash) (*lndclient.Invoice, error) {

	h.lnd.lock.Lock()
	defer h.lnd.lock.Unlock()

	inv, ok := h.lnd.Invoices[hash]
	if !ok {
		return nil, fmt.Errorf("invoice: %x not found", hash)
	}

	return inv, nil
}

// ListTransactions returns all known transactions of the backing lnd node.
func (h *mockLightningClient) ListTransactions(
	ctx context.Context) ([]*wire.MsgTx, error) {

	h.lnd.lock.Lock()
	txs := h.lnd.Transactions
	h.lnd.lock.Unlock()
	return txs, nil
}

// ListChannels retrieves all channels of the backing lnd node.
func (h *mockLightningClient) ListChannels(ctx context.Context) (
	[]lndclient.ChannelInfo, error) {

	return h.lnd.Channels, nil
}

// ClosedChannels returns a list of our closed channels.
func (h *mockLightningClient) ClosedChannels(_ context.Context) ([]lndclient.ClosedChannel,
	error) {

	return h.lnd.ClosedChannels, nil
}

// ForwardingHistory returns the mock's set of forwarding events.
func (h *mockLightningClient) ForwardingHistory(_ context.Context,
	_ lndclient.ForwardingHistoryRequest) (*lndclient.ForwardingHistoryResponse,
	error) {

	return &lndclient.ForwardingHistoryResponse{
		LastIndexOffset: 0,
		Events:          h.lnd.ForwardingEvents,
	}, nil
}

// ListInvoices returns our mock's invoices.
func (h *mockLightningClient) ListInvoices(_ context.Context,
	_ lndclient.ListInvoicesRequest) (*lndclient.ListInvoicesResponse,
	error) {

	invoices := make([]lndclient.Invoice, 0, len(h.lnd.Invoices))
	for _, invoice := range h.lnd.Invoices {
		invoices = append(invoices, *invoice)
	}

	return &lndclient.ListInvoicesResponse{
		Invoices: invoices,
	}, nil
}

// ListPayments makes a paginated call to our list payments endpoint.
func (h *mockLightningClient) ListPayments(_ context.Context,
	_ lndclient.ListPaymentsRequest) (*lndclient.ListPaymentsResponse,
	error) {

	return &lndclient.ListPaymentsResponse{
		Payments: h.lnd.Payments,
	}, nil
}

// ChannelBackup retrieves the backup for a particular channel. The
// backup is returned as an encrypted chanbackup.Single payload.
func (h *mockLightningClient) ChannelBackup(context.Context, wire.OutPoint) ([]byte, error) {
	return nil, nil
}

// ChannelBackups retrieves backups for all existing pending open and
// open channels. The backups are returned as an encrypted
// chanbackup.Multi payload.
func (h *mockLightningClient) ChannelBackups(ctx context.Context) ([]byte, error) {
	return nil, nil
}
