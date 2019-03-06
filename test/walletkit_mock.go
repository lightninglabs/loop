package test

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

type mockWalletKit struct {
	lnd      *LndMockServices
	keyIndex int32
}

func (m *mockWalletKit) DeriveNextKey(ctx context.Context, family int32) (
	*keychain.KeyDescriptor, error) {

	index := m.keyIndex

	_, pubKey := CreateKey(index)
	m.keyIndex++

	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(family),
			Index:  uint32(index),
		},
		PubKey: pubKey,
	}, nil
}

func (m *mockWalletKit) DeriveKey(ctx context.Context, in *keychain.KeyLocator) (
	*keychain.KeyDescriptor, error) {

	_, pubKey := CreateKey(int32(in.Index))

	return &keychain.KeyDescriptor{
		KeyLocator: *in,
		PubKey:     pubKey,
	}, nil
}

func (m *mockWalletKit) NextAddr(ctx context.Context) (btcutil.Address, error) {
	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.TestNet3Params,
	)
	if err != nil {
		return nil, err
	}
	return addr, nil
}

func (m *mockWalletKit) PublishTransaction(ctx context.Context, tx *wire.MsgTx) error {
	m.lnd.TxPublishChannel <- tx
	return nil
}

func (m *mockWalletKit) SendOutputs(ctx context.Context, outputs []*wire.TxOut,
	feeRate lnwallet.SatPerKWeight) (*wire.MsgTx, error) {

	var inputTxHash chainhash.Hash

	tx := wire.MsgTx{}
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  inputTxHash,
			Index: 0,
		},
	})

	for _, out := range outputs {
		tx.AddTxOut(&wire.TxOut{
			PkScript: out.PkScript,
			Value:    out.Value,
		})
	}

	m.lnd.SendOutputsChannel <- tx

	return &tx, nil
}

func (m *mockWalletKit) EstimateFee(ctx context.Context, confTarget int32) (
	lnwallet.SatPerKWeight, error) {
	if confTarget <= 1 {
		return 0, errors.New("conf target must be greater than 1")
	}

	return 10000, nil
}
