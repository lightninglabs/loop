package test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
)

type mockInvoices struct {
	lnd *LndMockServices
	wg  sync.WaitGroup
}

func (s *mockInvoices) SettleInvoice(ctx context.Context,
	preimage lntypes.Preimage) error {

	logger.Infof("Settle invoice %v with preimage %v", preimage.Hash(),
		preimage)

	s.lnd.SettleInvoiceChannel <- preimage

	return nil
}

func (s *mockInvoices) WaitForFinished() {
	s.wg.Wait()
}

func (s *mockInvoices) CancelInvoice(ctx context.Context,
	hash lntypes.Hash) error {

	s.lnd.FailInvoiceChannel <- hash

	return nil
}

func (s *mockInvoices) SubscribeSingleInvoice(ctx context.Context,
	hash lntypes.Hash) (<-chan lndclient.InvoiceUpdate,
	<-chan error, error) {

	updateChan := make(chan lndclient.InvoiceUpdate, 2)
	errChan := make(chan error)

	select {
	case s.lnd.SingleInvoiceSubcribeChannel <- &SingleInvoiceSubscription{
		Update: updateChan,
		Err:    errChan,
		Hash:   hash,
	}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	return updateChan, errChan, nil
}

func (s *mockInvoices) AddHoldInvoice(ctx context.Context,
	in *invoicesrpc.AddInvoiceData) (string, error) {

	s.lnd.lock.Lock()
	defer s.lnd.lock.Unlock()

	hash := in.Hash

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()

	payReq, err := zpay32.NewInvoice(
		s.lnd.ChainParams, *hash, creationDate,
		zpay32.Description(in.Memo),
		zpay32.CLTVExpiry(in.CltvExpiry),
		zpay32.Amount(in.Value),
	)
	if err != nil {
		return "", err
	}

	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return "", err
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
		return "", err
	}

	return payReqString, nil
}
