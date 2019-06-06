package lndclient

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
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
}

// Info contains info about the connected lnd node.
type Info struct {
	BlockHeight    uint32
	IdentityPubkey [33]byte
	Alias          string
	Network        string
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
				logger.Infof(
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
				logger.Infof(
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
				logger.Infof(
					"Payment %v already in flight", hash,
				)

				time.Sleep(paymentPollInterval)

			// Other errors are transformed into an error struct.
			default:
				logger.Warnf(
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
		Value:      int64(in.Value),
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
