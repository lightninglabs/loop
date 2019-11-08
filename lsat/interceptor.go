package lsat

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"sync"

	"github.com/lightninglabs/loop/lndclient"
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
	lnd   *lndclient.LndServices
	store Store
	lock  sync.Mutex
}

// NewInterceptor creates a new gRPC client interceptor that uses the provided
// lnd connection to automatically acquire and pay for LSAT tokens, unless the
// indicated store already contains a usable token.
func NewInterceptor(lnd *lndclient.LndServices, store Store) *Interceptor {
	return &Interceptor{
		lnd:   lnd,
		store: store,
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

	// If we already have a token, let's append it.
	if i.store.HasToken() {
		lsat, err := i.store.Token()
		if err != nil {
			return err
		}
		if err = addLsatCredentials(lsat); err != nil {
			return err
		}
	}

	// We need a way to extract the response headers sent by the
	// server. This can only be done through the experimental
	// grpc.Trailer call option.
	// We execute the request and inspect the error. If it's the
	// LSAT specific payment required error, we might execute the
	// same method again later with the paid LSAT token.
	trailerMetadata := &metadata.MD{}
	opts = append(opts, grpc.Trailer(trailerMetadata))
	err := invoker(ctx, method, req, reply, cc, opts...)

	// Only handle the LSAT error message that comes in the form of
	// a gRPC status error.
	if isPaymentRequired(err) {
		lsat, err := i.payLsatToken(ctx, trailerMetadata)
		if err != nil {
			return err
		}
		if err = addLsatCredentials(lsat); err != nil {
			return err
		}

		// Execute the same request again, now with the LSAT
		// token added as an RPC credential.
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	return err
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

	// Pay invoice now and wait for the result to arrive or the main context
	// being canceled.
	// TODO(guggero): Store payment information so we can track the payment
	//  later in case the client shuts down while the payment is in flight.
	respChan := i.lnd.Client.PayInvoice(
		ctx, invoiceStr, MaxRoutingFeeSats, nil,
	)
	select {
	case result := <-respChan:
		if result.Err != nil {
			return nil, result.Err
		}
		token, err := NewToken(
			macBytes, invoice.PaymentHash, result.Preimage,
			lnwire.NewMSatFromSatoshis(result.PaidAmt),
			lnwire.NewMSatFromSatoshis(result.PaidFee),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create token: %v",
				err)
		}
		return token, i.store.StoreToken(token)

	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled")
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
