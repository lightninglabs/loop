package lndclient

import (
	"context"
	"errors"
	"sync"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

// InvoicesClient exposes invoice functionality.
type InvoicesClient interface {
	SubscribeSingleInvoice(ctx context.Context, hash lntypes.Hash) (
		<-chan InvoiceUpdate, <-chan error, error)

	SettleInvoice(ctx context.Context, preimage lntypes.Preimage) error

	CancelInvoice(ctx context.Context, hash lntypes.Hash) error

	AddHoldInvoice(ctx context.Context, in *invoicesrpc.AddInvoiceData) (
		string, error)
}

// InvoiceUpdate contains a state update for an invoice.
type InvoiceUpdate struct {
	State   channeldb.ContractState
	AmtPaid btcutil.Amount
}

type invoicesClient struct {
	client     invoicesrpc.InvoicesClient
	invoiceMac serializedMacaroon
	wg         sync.WaitGroup
}

func newInvoicesClient(conn *grpc.ClientConn, invoiceMac serializedMacaroon) *invoicesClient {
	return &invoicesClient{
		client:     invoicesrpc.NewInvoicesClient(conn),
		invoiceMac: invoiceMac,
	}
}

func (s *invoicesClient) WaitForFinished() {
	s.wg.Wait()
}

func (s *invoicesClient) SettleInvoice(ctx context.Context,
	preimage lntypes.Preimage) error {

	timeoutCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx := s.invoiceMac.WithMacaroonAuth(timeoutCtx)
	_, err := s.client.SettleInvoice(rpcCtx, &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	})

	return err
}

func (s *invoicesClient) CancelInvoice(ctx context.Context,
	hash lntypes.Hash) error {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = s.invoiceMac.WithMacaroonAuth(rpcCtx)
	_, err := s.client.CancelInvoice(rpcCtx, &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: hash[:],
	})

	return err
}

func (s *invoicesClient) SubscribeSingleInvoice(ctx context.Context,
	hash lntypes.Hash) (<-chan InvoiceUpdate,
	<-chan error, error) {

	invoiceStream, err := s.client.SubscribeSingleInvoice(
		s.invoiceMac.WithMacaroonAuth(ctx),
		&invoicesrpc.SubscribeSingleInvoiceRequest{
			RHash: hash[:],
		},
	)
	if err != nil {
		return nil, nil, err
	}

	updateChan := make(chan InvoiceUpdate)
	errChan := make(chan error, 1)

	// Invoice updates goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			invoice, err := invoiceStream.Recv()
			if err != nil {
				errChan <- err
				return
			}

			state, err := fromRPCInvoiceState(invoice.State)
			if err != nil {
				errChan <- err
				return
			}

			select {
			case updateChan <- InvoiceUpdate{
				State:   state,
				AmtPaid: btcutil.Amount(invoice.AmtPaidSat),
			}:
			case <-ctx.Done():
				return
			}
		}
	}()

	return updateChan, errChan, nil
}

func (s *invoicesClient) AddHoldInvoice(ctx context.Context,
	in *invoicesrpc.AddInvoiceData) (string, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcIn := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:       in.Memo,
		Hash:       in.Hash[:],
		Value:      int64(in.Value.ToSatoshis()),
		Expiry:     in.Expiry,
		CltvExpiry: in.CltvExpiry,
		Private:    true,
	}

	rpcCtx = s.invoiceMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.AddHoldInvoice(rpcCtx, rpcIn)
	if err != nil {
		return "", err
	}
	return resp.PaymentRequest, nil
}

func fromRPCInvoiceState(state lnrpc.Invoice_InvoiceState) (
	channeldb.ContractState, error) {

	switch state {
	case lnrpc.Invoice_OPEN:
		return channeldb.ContractOpen, nil
	case lnrpc.Invoice_ACCEPTED:
		return channeldb.ContractAccepted, nil
	case lnrpc.Invoice_SETTLED:
		return channeldb.ContractSettled, nil
	case lnrpc.Invoice_CANCELED:
		return channeldb.ContractCanceled, nil
	}

	return 0, errors.New("unknown state")
}
