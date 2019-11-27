package lsat

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// GRPCErrCode is the error code we receive from a gRPC call if the
	// server expects a payment.
	GRPCErrCode = codes.Internal

	// GRPCErrMessage is the error message we receive from a gRPC call in
	// conjunction with the GRPCErrCode to signal the client that a payment
	// is required to access the service.
	GRPCErrMessage = "payment required"

	// AuthHeader is is the HTTP response header that contains the payment
	// challenge.
	AuthHeader = "WWW-Authenticate"

	// MaxRoutingFee is the maximum routing fee in satoshis that we are
	// going to pay to acquire an LSAT token.
	// TODO(guggero): make this configurable
	MaxRoutingFeeSats = 10

	// PaymentTimeout is the maximum time we allow a payment to take before
	// we stop waiting for it.
	PaymentTimeout = 60 * time.Second

	// manualRetryHint is the error text we return to tell the user how a
	// token payment can be retried if the payment fails.
	manualRetryHint = "consider removing pending token file if error " +
		"persists. use 'listauth' command to find out token file name"
)

var (
	// authHeaderRegex is the regular expression the payment challenge must
	// match for us to be able to parse the macaroon and invoice.
	authHeaderRegex = regexp.MustCompile(
		"LSAT macaroon='(.*?)' invoice='(.*?)'",
	)
)

// Interceptor is a gRPC client interceptor that can handle LSAT authentication
// challenges with embedded payment requests. It uses a connection to lnd to
// automatically pay for an authentication token.
type Interceptor struct {
	lnd         *lndclient.LndServices
	store       Store
	callTimeout time.Duration
	lock        sync.Mutex
}

// NewInterceptor creates a new gRPC client interceptor that uses the provided
// lnd connection to automatically acquire and pay for LSAT tokens, unless the
// indicated store already contains a usable token.
func NewInterceptor(lnd *lndclient.LndServices, store Store,
	rpcCallTimeout time.Duration) *Interceptor {

	return &Interceptor{
		lnd:         lnd,
		store:       store,
		callTimeout: rpcCallTimeout,
	}
}

// UnaryInterceptor is an interceptor method that can be used directly by gRPC
// for unary calls. If the store contains a token, it is attached as credentials
// to every call before patching it through. The response error is also
// intercepted for every call. If there is an error returned and it is
// indicating a payment challenge, a token is acquired and paid for
// automatically. The original request is then repeated back to the server, now
// with the new token attached.
func (i *Interceptor) UnaryInterceptor(ctx context.Context, method string,
	req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {

	// To avoid paying for a token twice if two parallel requests are
	// happening, we require an exclusive lock here.
	i.lock.Lock()
	defer i.lock.Unlock()

	addLsatCredentials := func(token *Token) error {
		macaroon, err := token.PaidMacaroon()
		if err != nil {
			return err
		}
		opts = append(opts, grpc.PerRPCCredentials(
			macaroons.NewMacaroonCredential(macaroon),
		))
		return nil
	}

	// Let's see if the store already contains a token and what state it
	// might be in. If a previous call was aborted, we might have a pending
	// token that needs to be handled separately.
	token, err := i.store.CurrentToken()
	switch {
	// If there is no token yet, nothing to do at this point.
	case err == ErrNoToken:

	// Some other error happened that we have to surface.
	case err != nil:
		log.Errorf("Failed to get token from store: %v", err)
		return fmt.Errorf("getting token from store failed: %v", err)

	// Only if we have a paid token append it. We don't resume a pending
	// payment just yet, since we don't even know if a token is required for
	// this call. We also never send a pending payment to the server since
	// we know it's not valid.
	case !token.isPending():
		if err = addLsatCredentials(token); err != nil {
			log.Errorf("Adding macaroon to request failed: %v", err)
			return fmt.Errorf("adding macaroon failed: %v", err)
		}
	}

	// We need a way to extract the response headers sent by the server.
	// This can only be done through the experimental grpc.Trailer call
	// option. We execute the request and inspect the error. If it's the
	// LSAT specific payment required error, we might execute the same
	// method again later with the paid LSAT token.
	trailerMetadata := &metadata.MD{}
	opts = append(opts, grpc.Trailer(trailerMetadata))
	rpcCtx, cancel := context.WithTimeout(ctx, i.callTimeout)
	defer cancel()
	err = invoker(rpcCtx, method, req, reply, cc, opts...)

	// Only handle the LSAT error message that comes in the form of
	// a gRPC status error.
	if isPaymentRequired(err) {
		paidToken, err := i.handlePayment(ctx, token, trailerMetadata)
		if err != nil {
			return err
		}
		if err = addLsatCredentials(paidToken); err != nil {
			log.Errorf("Adding macaroon to request failed: %v", err)
			return fmt.Errorf("adding macaroon failed: %v", err)
		}

		// Execute the same request again, now with the LSAT
		// token added as an RPC credential.
		rpcCtx2, cancel2 := context.WithTimeout(ctx, i.callTimeout)
		defer cancel2()
		return invoker(rpcCtx2, method, req, reply, cc, opts...)
	}
	return err
}

// handlePayment tries to obtain a valid token by either tracking the payment
// status of a pending token or paying for a new one.
func (i *Interceptor) handlePayment(ctx context.Context, token *Token,
	md *metadata.MD) (*Token, error) {

	switch {
	// Resume/track a pending payment if it was interrupted for some reason.
	case token != nil && token.isPending():
		log.Infof("Payment of LSAT token is required, resuming/" +
			"tracking previous payment from pending LSAT token")
		err := i.trackPayment(ctx, token)
		if err != nil {
			return nil, err
		}
		return token, nil

	// We don't have a token yet, try to get a new one.
	case token == nil:
		// We don't have a token yet, get a new one.
		log.Infof("Payment of LSAT token is required, paying invoice")
		return i.payLsatToken(ctx, md)

	// We have a token and it's valid, nothing more to do here.
	default:
		log.Debugf("Found valid LSAT token to add to request")
		return token, nil
	}
}

// payLsatToken reads the payment challenge from the response metadata and tries
// to pay the invoice encoded in them, returning a paid LSAT token if
// successful.
func (i *Interceptor) payLsatToken(ctx context.Context, md *metadata.MD) (
	*Token, error) {

	// First parse the authentication header that was stored in the
	// metadata.
	authHeader := md.Get(AuthHeader)
	if len(authHeader) == 0 {
		return nil, fmt.Errorf("auth header not found in response")
	}
	matches := authHeaderRegex.FindStringSubmatch(authHeader[0])
	if len(matches) != 3 {
		return nil, fmt.Errorf("invalid auth header "+
			"format: %s", authHeader[0])
	}

	// Decode the base64 macaroon and the invoice so we can store the
	// information in our store later.
	macBase64, invoiceStr := matches[1], matches[2]
	macBytes, err := base64.StdEncoding.DecodeString(macBase64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode of macaroon failed: "+
			"%v", err)
	}
	invoice, err := zpay32.Decode(invoiceStr, i.lnd.ChainParams)
	if err != nil {
		return nil, fmt.Errorf("unable to decode invoice: %v", err)
	}

	// Create and store the pending token so we can resume the payment in
	// case the payment is interrupted somehow.
	token, err := tokenFromChallenge(macBytes, invoice.PaymentHash)
	if err != nil {
		return nil, fmt.Errorf("unable to create token: %v", err)
	}
	err = i.store.StoreToken(token)
	if err != nil {
		return nil, fmt.Errorf("unable to store pending token: %v", err)
	}

	// Pay invoice now and wait for the result to arrive or the main context
	// being canceled.
	payCtx, cancel := context.WithTimeout(ctx, PaymentTimeout)
	defer cancel()
	respChan := i.lnd.Client.PayInvoice(
		payCtx, invoiceStr, MaxRoutingFeeSats, nil,
	)
	select {
	case result := <-respChan:
		if result.Err != nil {
			return nil, result.Err
		}
		token.Preimage = result.Preimage
		token.AmountPaid = lnwire.NewMSatFromSatoshis(result.PaidAmt)
		token.RoutingFeePaid = lnwire.NewMSatFromSatoshis(
			result.PaidFee,
		)
		return token, i.store.StoreToken(token)

	case <-payCtx.Done():
		return nil, fmt.Errorf("payment timed out. try again to track "+
			"payment. %s", manualRetryHint)

	case <-ctx.Done():
		return nil, fmt.Errorf("parent context canceled. try again to"+
			"track payment. %s", manualRetryHint)
	}
}

// trackPayment tries to resume a pending payment by tracking its state and
// waiting for a conclusive result.
func (i *Interceptor) trackPayment(ctx context.Context, token *Token) error {
	// Lookup state of the payment.
	paymentStateCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	payStatusChan, payErrChan, err := i.lnd.Router.TrackPayment(
		paymentStateCtx, token.PaymentHash,
	)
	if err != nil {
		log.Errorf("Could not call TrackPayment on lnd: %v", err)
		return fmt.Errorf("track payment call to lnd failed: %v", err)
	}

	// We can't wait forever, so we give the payment tracking the same
	// timeout as the original payment.
	payCtx, cancel := context.WithTimeout(ctx, PaymentTimeout)
	defer cancel()

	// We'll consume status updates until we reach a conclusive state or
	// reach the timeout.
	for {
		select {
		// If we receive a state without an error, the payment has been
		// initiated. Loop until the payment
		case result := <-payStatusChan:
			switch result.State {
			// If the payment was successful, we have all the
			// information we need and we can return the fully paid
			// token.
			case routerrpc.PaymentState_SUCCEEDED:
				extractPaymentDetails(token, result)
				return i.store.StoreToken(token)

			// The payment is still in transit, we'll give it more
			// time to complete.
			case routerrpc.PaymentState_IN_FLIGHT:

			// Any other state means either error or timeout.
			default:
				return fmt.Errorf("payment tracking failed "+
					"with state %s. %s",
					result.State.String(), manualRetryHint)
			}

		// Abort the payment execution for any error.
		case err := <-payErrChan:
			return fmt.Errorf("payment tracking failed: %v. %s",
				err, manualRetryHint)

		case <-payCtx.Done():
			return fmt.Errorf("payment tracking timed out. %s",
				manualRetryHint)
		}
	}
}

// isPaymentRequired inspects an error to find out if it's the specific gRPC
// error returned by the server to indicate a payment is required to access the
// service.
func isPaymentRequired(err error) bool {
	statusErr, ok := status.FromError(err)
	return ok &&
		statusErr.Message() == GRPCErrMessage &&
		statusErr.Code() == GRPCErrCode
}

// extractPaymentDetails extracts the preimage and amounts paid for a payment
// from the payment status and stores them in the token.
func extractPaymentDetails(token *Token, status lndclient.PaymentStatus) {
	token.Preimage = status.Preimage
	total := status.Route.TotalAmount
	fees := status.Route.TotalFees()
	token.AmountPaid = total - fees
	token.RoutingFeePaid = fees
}
