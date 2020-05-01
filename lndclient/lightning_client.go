package lndclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LightningClient exposes base lightning functionality.
type LightningClient interface {
	PayInvoice(ctx context.Context, invoice string,
		maxFee btcutil.Amount,
		outgoingChannel *uint64) chan PaymentResult

	GetInfo(ctx context.Context) (*Info, error)

	EstimateFeeToP2WSH(ctx context.Context, amt btcutil.Amount,
		confTarget int32) (btcutil.Amount, error)

	ConfirmedWalletBalance(ctx context.Context) (btcutil.Amount, error)

	AddInvoice(ctx context.Context, in *invoicesrpc.AddInvoiceData) (
		lntypes.Hash, string, error)

	// LookupInvoice looks up an invoice.
	LookupInvoice(ctx context.Context, hash lntypes.Hash) (*Invoice, error)

	// ListTransactions returns all known transactions of the backing lnd
	// node.
	ListTransactions(ctx context.Context) ([]*wire.MsgTx, error)

	// ChannelBackup retrieves the backup for a particular channel. The
	// backup is returned as an encrypted chanbackup.Single payload.
	ChannelBackup(context.Context, wire.OutPoint) ([]byte, error)

	// ChannelBackups retrieves backups for all existing pending open and
	// open channels. The backups are returned as an encrypted
	// chanbackup.Multi payload.
	ChannelBackups(ctx context.Context) ([]byte, error)
}

// Info contains info about the connected lnd node.
type Info struct {
	BlockHeight    uint32
	IdentityPubkey [33]byte
	Alias          string
	Network        string
	Uris           []string
}

var (
	// ErrMalformedServerResponse is returned when the swap and/or prepay
	// invoice is malformed.
	ErrMalformedServerResponse = errors.New(
		"one or more invoices are malformed",
	)

	// ErrNoRouteToServer is returned if no quote can returned because there
	// is no route to the server.
	ErrNoRouteToServer = errors.New("no off-chain route to server")

	// PaymentResultUnknownPaymentHash is the string result returned by
	// SendPayment when the final node indicates the hash is unknown.
	PaymentResultUnknownPaymentHash = "UnknownPaymentHash"

	// PaymentResultSuccess is the string result returned by SendPayment
	// when the payment was successful.
	PaymentResultSuccess = ""

	// PaymentResultAlreadyPaid is the string result returned by SendPayment
	// when the payment was already completed in a previous SendPayment
	// call.
	PaymentResultAlreadyPaid = channeldb.ErrAlreadyPaid.Error()

	// PaymentResultInFlight is the string result returned by SendPayment
	// when the payment was initiated in a previous SendPayment call and
	// still in flight.
	PaymentResultInFlight = channeldb.ErrPaymentInFlight.Error()

	paymentPollInterval = 3 * time.Second
)

type lightningClient struct {
	client   lnrpc.LightningClient
	wg       sync.WaitGroup
	params   *chaincfg.Params
	adminMac serializedMacaroon
}

func newLightningClient(conn *grpc.ClientConn,
	params *chaincfg.Params, adminMac serializedMacaroon) *lightningClient {

	return &lightningClient{
		client:   lnrpc.NewLightningClient(conn),
		params:   params,
		adminMac: adminMac,
	}
}

// PaymentResult signals the result of a payment.
type PaymentResult struct {
	Err      error
	Preimage lntypes.Preimage
	PaidFee  btcutil.Amount
	PaidAmt  btcutil.Amount
}

func (s *lightningClient) WaitForFinished() {
	s.wg.Wait()
}

func (s *lightningClient) ConfirmedWalletBalance(ctx context.Context) (
	btcutil.Amount, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.WalletBalance(rpcCtx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		return 0, err
	}

	return btcutil.Amount(resp.ConfirmedBalance), nil
}

func (s *lightningClient) GetInfo(ctx context.Context) (*Info, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.GetInfo(rpcCtx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return nil, err
	}

	pubKey, err := hex.DecodeString(resp.IdentityPubkey)
	if err != nil {
		return nil, err
	}

	var pubKeyArray [33]byte
	copy(pubKeyArray[:], pubKey)

	return &Info{
		BlockHeight:    resp.BlockHeight,
		IdentityPubkey: pubKeyArray,
		Alias:          resp.Alias,
		Network:        resp.Chains[0].Network,
		Uris:           resp.Uris,
	}, nil
}

func (s *lightningClient) EstimateFeeToP2WSH(ctx context.Context,
	amt btcutil.Amount, confTarget int32) (btcutil.Amount,
	error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	// Generate dummy p2wsh address for fee estimation.
	wsh := [32]byte{}
	p2wshAddress, err := btcutil.NewAddressWitnessScriptHash(
		wsh[:], s.params,
	)
	if err != nil {
		return 0, err
	}

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.EstimateFee(
		rpcCtx,
		&lnrpc.EstimateFeeRequest{
			TargetConf: confTarget,
			AddrToAmount: map[string]int64{
				p2wshAddress.String(): int64(amt),
			},
		},
	)
	if err != nil {
		return 0, err
	}
	return btcutil.Amount(resp.FeeSat), nil
}

// PayInvoice pays an invoice.
func (s *lightningClient) PayInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount, outgoingChannel *uint64) chan PaymentResult {

	// Use buffer to prevent blocking.
	paymentChan := make(chan PaymentResult, 1)

	// Execute payment in parallel, because it will block until server
	// discovers preimage.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		result := s.payInvoice(ctx, invoice, maxFee, outgoingChannel)
		if result != nil {
			paymentChan <- *result
		}
	}()

	return paymentChan
}

// payInvoice tries to send a payment and returns the final result. If
// necessary, it will poll lnd for the payment result.
func (s *lightningClient) payInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount, outgoingChannel *uint64) *PaymentResult {

	payReq, err := zpay32.Decode(invoice, s.params)
	if err != nil {
		return &PaymentResult{
			Err: fmt.Errorf("invoice decode: %v", err),
		}
	}

	if payReq.MilliSat == nil {
		return &PaymentResult{
			Err: errors.New("no amount in invoice"),
		}
	}

	hash := lntypes.Hash(*payReq.PaymentHash)

	ctx = s.adminMac.WithMacaroonAuth(ctx)
	for {
		// Create no timeout context as this call can block for a long
		// time.

		req := &lnrpc.SendRequest{
			FeeLimit: &lnrpc.FeeLimit{
				Limit: &lnrpc.FeeLimit_Fixed{
					Fixed: int64(maxFee),
				},
			},
			PaymentRequest: invoice,
		}

		if outgoingChannel != nil {
			req.OutgoingChanId = *outgoingChannel
		}

		payResp, err := s.client.SendPaymentSync(ctx, req)

		if status.Code(err) == codes.Canceled {
			return nil
		}

		if err == nil {
			// TODO: Use structured payment error when available,
			// instead of this britle string matching.
			switch payResp.PaymentError {

			// Paid successfully.
			case PaymentResultSuccess:
				log.Infof(
					"Payment %v completed", hash,
				)

				r := payResp.PaymentRoute
				preimage, err := lntypes.MakePreimage(
					payResp.PaymentPreimage,
				)
				if err != nil {
					return &PaymentResult{Err: err}
				}
				return &PaymentResult{
					PaidFee: btcutil.Amount(r.TotalFees),
					PaidAmt: btcutil.Amount(
						r.TotalAmt - r.TotalFees,
					),
					Preimage: preimage,
				}

			// Invoice was already paid on a previous run.
			case PaymentResultAlreadyPaid:
				log.Infof(
					"Payment %v already completed", hash,
				)

				// Unfortunately lnd doesn't return the route if
				// the payment was successful in a previous
				// call. Assume paid fees 0 and take paid amount
				// from invoice.

				return &PaymentResult{
					PaidFee: 0,
					PaidAmt: payReq.MilliSat.ToSatoshis(),
				}

			// If the payment is already in flight, we will poll
			// again later for an outcome.
			//
			// TODO: Improve this when lnd expose more API to
			// tracking existing payments.
			case PaymentResultInFlight:
				log.Infof(
					"Payment %v already in flight", hash,
				)

				time.Sleep(paymentPollInterval)

			// Other errors are transformed into an error struct.
			default:
				log.Warnf(
					"Payment %v failed: %v", hash,
					payResp.PaymentError,
				)

				return &PaymentResult{
					Err: errors.New(payResp.PaymentError),
				}
			}
		}
	}
}

func (s *lightningClient) AddInvoice(ctx context.Context,
	in *invoicesrpc.AddInvoiceData) (lntypes.Hash, string, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &lnrpc.Invoice{
		Memo:       in.Memo,
		Value:      int64(in.Value.ToSatoshis()),
		Expiry:     in.Expiry,
		CltvExpiry: in.CltvExpiry,
		Private:    true,
	}

	if in.Preimage != nil {
		rpcIn.RPreimage = in.Preimage[:]
	}
	if in.Hash != nil {
		rpcIn.RHash = in.Hash[:]
	}

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.AddInvoice(rpcCtx, rpcIn)
	if err != nil {
		return lntypes.Hash{}, "", err
	}
	hash, err := lntypes.MakeHash(resp.RHash)
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	return hash, resp.PaymentRequest, nil
}

// Invoice represents an invoice in lnd.
type Invoice struct {
	// Preimage is the invoice's preimage, which is set if the invoice
	// is settled.
	Preiamge *lntypes.Preimage

	// Hash is invoice hash.
	Hash lntypes.Hash

	// Memo is an optional memo field for hte invoice.
	Memo string

	// PaymentRequest is the invoice's payment request.
	PaymentRequest string

	// Amount is the amount of the invoice in millisatoshis.
	Amount lnwire.MilliSatoshi

	// AmountPaid is the amount that was paid for the invoice. This field
	// will only be set if the invoice is settled.
	AmountPaid lnwire.MilliSatoshi

	// CreationDate is the time the invoice was created.
	CreationDate time.Time

	// SettleDate is the time the invoice was settled.
	SettleDate time.Time

	// State is the invoice's current state.
	State channeldb.ContractState

	// IsKeysend indicates whether the invoice was a spontaneous payment.
	IsKeysend bool
}

// LookupInvoice looks up an invoice in lnd, it will error if the invoice is
// not known to lnd.
func (s *lightningClient) LookupInvoice(ctx context.Context,
	hash lntypes.Hash) (*Invoice, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &lnrpc.PaymentHash{
		RHash: hash[:],
	}

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.LookupInvoice(rpcCtx, rpcIn)
	if err != nil {
		return nil, err
	}

	invoice := &Invoice{
		Preiamge:       nil,
		Hash:           hash,
		Memo:           resp.Memo,
		PaymentRequest: resp.PaymentRequest,
		Amount:         lnwire.MilliSatoshi(resp.ValueMsat),
		AmountPaid:     lnwire.MilliSatoshi(resp.AmtPaidMsat),
		CreationDate:   time.Unix(resp.CreationDate, 0),
		IsKeysend:      resp.IsKeysend,
	}

	switch resp.State {
	case lnrpc.Invoice_OPEN:
		invoice.State = channeldb.ContractOpen

	case lnrpc.Invoice_ACCEPTED:
		invoice.State = channeldb.ContractAccepted

	// If the invoice is settled, it also has a non-nil preimage, which we
	// can set on our invoice.
	case lnrpc.Invoice_SETTLED:
		invoice.State = channeldb.ContractSettled
		preimage, err := lntypes.MakePreimage(resp.RPreimage)
		if err != nil {
			return nil, err
		}
		invoice.Preiamge = &preimage

	case lnrpc.Invoice_CANCELED:
		invoice.State = channeldb.ContractCanceled

	default:
		return nil, fmt.Errorf("unknown invoice state: %v", resp.State)
	}

	// Only set settle date if it is non-zero, because 0 unix time is
	// not the same as a zero time struct.
	if resp.SettleDate != 0 {
		invoice.SettleDate = time.Unix(resp.SettleDate, 0)
	}

	return invoice, nil
}

// ListTransactions returns all known transactions of the backing lnd node.
func (s *lightningClient) ListTransactions(ctx context.Context) ([]*wire.MsgTx, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	rpcIn := &lnrpc.GetTransactionsRequest{}
	resp, err := s.client.GetTransactions(rpcCtx, rpcIn)
	if err != nil {
		return nil, err
	}

	txs := make([]*wire.MsgTx, 0, len(resp.Transactions))
	for _, respTx := range resp.Transactions {
		rawTx, err := hex.DecodeString(respTx.RawTxHex)
		if err != nil {
			return nil, err
		}

		var tx wire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
			return nil, err
		}
		txs = append(txs, &tx)
	}

	return txs, nil
}

// ChannelBackup retrieves the backup for a particular channel. The backup is
// returned as an encrypted chanbackup.Single payload.
func (s *lightningClient) ChannelBackup(ctx context.Context,
	channelPoint wire.OutPoint) ([]byte, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	req := &lnrpc.ExportChannelBackupRequest{
		ChanPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: channelPoint.Hash[:],
			},
			OutputIndex: channelPoint.Index,
		},
	}
	resp, err := s.client.ExportChannelBackup(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	return resp.ChanBackup, nil
}

// ChannelBackups retrieves backups for all existing pending open and open
// channels. The backups are returned as an encrypted chanbackup.Multi payload.
func (s *lightningClient) ChannelBackups(ctx context.Context) ([]byte, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	req := &lnrpc.ChanBackupExportRequest{}
	resp, err := s.client.ExportAllChannelBackups(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	return resp.MultiChanBackup.MultiChanBackup, nil
}
