package lndclient

import (
	"context"
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InvoicesClient exposes invoice functionality.
type InvoicesClient interface {
	SubscribeSingleInvoice(ctx context.Context, hash lntypes.Hash) (
		<-chan channeldb.ContractState, <-chan error, error)

	SettleInvoice(ctx context.Context, preimage lntypes.Preimage) error

	CancelInvoice(ctx context.Context, hash lntypes.Hash) error

	AddHoldInvoice(ctx context.Context, in *invoicesrpc.AddInvoiceData) (
		string, error)
}

type invoicesClient struct {
	client invoicesrpc.InvoicesClient
	wg     sync.WaitGroup
}

func newInvoicesClient(conn *grpc.ClientConn) *invoicesClient {
	return &invoicesClient{
		client: invoicesrpc.NewInvoicesClient(conn),
	}
}

func (s *invoicesClient) WaitForFinished() {
	s.wg.Wait()
}

func (s *invoicesClient) SettleInvoice(ctx context.Context,
	preimage lntypes.Preimage) error {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	_, err := s.client.SettleInvoice(rpcCtx, &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	})

	return err
}

func (s *invoicesClient) CancelInvoice(ctx context.Context,
	hash lntypes.Hash) error {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	_, err := s.client.CancelInvoice(rpcCtx, &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: hash[:],
	})

	return err
}

func (s *invoicesClient) SubscribeSingleInvoice(ctx context.Context,
	hash lntypes.Hash) (<-chan channeldb.ContractState,
	<-chan error, error) {

	invoiceStream, err := s.client.
		SubscribeSingleInvoice(ctx,
			&lnrpc.PaymentHash{
				RHash: hash[:],
			})
	if err != nil {
		return nil, nil, err
	}

	updateChan := make(chan channeldb.ContractState)
	errChan := make(chan error, 1)

	// Invoice updates goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			invoice, err := invoiceStream.Recv()
			if err != nil {
				if status.Code(err) != codes.Canceled {
					errChan <- err
				}
				return
			}

			state, err := fromRPCInvoiceState(invoice.State)
			if err != nil {
				errChan <- err
				return
			}

			select {
			case updateChan <- state:
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
		Value:      int64(in.Value),
		Expiry:     in.Expiry,
		CltvExpiry: in.CltvExpiry,
		Private:    true,
	}

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
