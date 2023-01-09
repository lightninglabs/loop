package test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"golang.org/x/net/context"
)

type mockLightningClient struct {
	lnd *LndMockServices
	wg  sync.WaitGroup

	// Embed lndclient's interface so that lndclient can be expanded
	// without the need to implement unused functions on the mock.
	lndclient.LightningClient
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

// DecodePaymentRequest returns a non-nil payment request.
func (h *mockLightningClient) DecodePaymentRequest(_ context.Context,
	_ string) (*lndclient.PaymentRequest, error) {

	return &lndclient.PaymentRequest{}, nil
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

	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	payReqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: func(hash []byte) ([]byte, error) {
				// ecdsa.SignCompact returns a
				// pubkey-recoverable signature.
				sig, err := ecdsa.SignCompact(
					privKey, hash, true,
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
		return lntypes.Hash{}, "", err
	}

	// Add the invoice we have created to our mock's set of invoices.
	h.lnd.Invoices[hash] = &lndclient.Invoice{
		Preimage:       nil,
		Hash:           hash,
		PaymentRequest: payReqString,
		Amount:         in.Value,
		CreationDate:   creationDate,
		State:          invpkg.ContractOpen,
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
	_ context.Context, _, _ int32, _ ...lndclient.ListTransactionsOption) (
	[]lndclient.Transaction, error) {

	h.lnd.lock.Lock()
	txs := h.lnd.Transactions
	h.lnd.lock.Unlock()

	return txs, nil
}

// GetNodeInfo retrieves info on the node, and if includeChannels is True,
// will return other channels the node may have with other peers.
func (h *mockLightningClient) GetNodeInfo(ctx context.Context,
	pubKeyBytes route.Vertex, includeChannels bool) (*lndclient.NodeInfo, error) {

	nodeInfo := &lndclient.NodeInfo{
		Node: &lndclient.Node{
			PubKey: pubKeyBytes,
		},
	}

	if !includeChannels {
		return nodeInfo, nil
	}

	nodePubKey, err := route.NewVertexFromStr(h.lnd.NodePubkey)
	if err != nil {
		return nil, err
	}

	// NodeInfo.Channels should only contain channels which: do not belong
	// to the queried node; are not private; have the provided vertex as a
	// participant
	for _, edge := range h.lnd.ChannelEdges {
		if (edge.Node1 == pubKeyBytes || edge.Node2 == pubKeyBytes) &&
			(edge.Node1 != nodePubKey || edge.Node2 != nodePubKey) {

			for _, channel := range h.lnd.Channels {
				if channel.ChannelID == edge.ChannelID && !channel.Private {
					nodeInfo.Channels = append(nodeInfo.Channels, *edge)
				}
			}
		}
	}

	nodeInfo.ChannelCount = len(nodeInfo.Channels)

	return nodeInfo, nil
}

// GetChanInfo retrieves all the info the node has on the given channel.
func (h *mockLightningClient) GetChanInfo(ctx context.Context,
	channelID uint64) (*lndclient.ChannelEdge, error) {

	var channelEdge *lndclient.ChannelEdge
	if channelEdge, ok := h.lnd.ChannelEdges[channelID]; ok {
		return channelEdge, nil
	}
	return channelEdge, fmt.Errorf("not found")
}

// ListChannels retrieves all channels of the backing lnd node.
func (h *mockLightningClient) ListChannels(ctx context.Context, _, _ bool) (
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
