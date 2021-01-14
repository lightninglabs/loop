package test

import (
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lntypes"
	"golang.org/x/net/context"
)

type mockRouter struct {
	lndclient.RouterClient
	lnd *LndMockServices
}

func (r *mockRouter) SendPayment(ctx context.Context,
	request lndclient.SendPaymentRequest) (chan lndclient.PaymentStatus,
	chan error, error) {

	statusChan := make(chan lndclient.PaymentStatus)
	errorChan := make(chan error)

	r.lnd.RouterSendPaymentChannel <- RouterPaymentChannelMessage{
		SendPaymentRequest: request,
		TrackPaymentMessage: TrackPaymentMessage{
			Updates: statusChan,
			Errors:  errorChan,
		},
	}

	return statusChan, errorChan, nil
}

func (r *mockRouter) TrackPayment(ctx context.Context,
	hash lntypes.Hash) (chan lndclient.PaymentStatus, chan error, error) {

	statusChan := make(chan lndclient.PaymentStatus)
	errorChan := make(chan error)
	r.lnd.TrackPaymentChannel <- TrackPaymentMessage{
		Hash:    hash,
		Updates: statusChan,
		Errors:  errorChan,
	}

	return statusChan, errorChan, nil
}
