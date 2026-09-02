package server

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type serverKey struct {
	pubKey  *btcec.PublicKey
	locator keychain.KeyLocator
}

func (s *Server) deriveKey(ctx context.Context, family int32) (*serverKey,
	error) {

	keyDesc, err := s.cfg.Lnd.WalletKit.DeriveNextKey(ctx, family)
	if err != nil {
		return nil, err
	}

	return &serverKey{
		pubKey:  keyDesc.PubKey,
		locator: keyDesc.KeyLocator,
	}, nil
}

func keyBytes(key *btcec.PublicKey) [btcec.PubKeyBytesLenCompressed]byte {
	var serialized [btcec.PubKeyBytesLenCompressed]byte
	copy(serialized[:], key.SerializeCompressed())

	return serialized
}

func parseKey(name string, serialized []byte) (*btcec.PublicKey, error) {
	if len(serialized) != btcec.PubKeyBytesLenCompressed {
		return nil, status.Errorf(
			codes.InvalidArgument, "%s must be 33 bytes", name,
		)
	}

	key, err := btcec.ParsePubKey(serialized)
	if err != nil {
		return nil, status.Errorf(
			codes.InvalidArgument, "invalid %s: %v", name, err,
		)
	}

	return key, nil
}

func parseHash(serialized []byte) (lntypes.Hash, error) {
	hash, err := lntypes.MakeHash(serialized)
	if err != nil {
		return lntypes.Hash{}, status.Error(
			codes.InvalidArgument, "swap hash must be 32 bytes",
		)
	}

	return hash, nil
}

func (s *Server) swapFee(amount btcutil.Amount) btcutil.Amount {
	return s.cfg.FeeBaseSat + btcutil.Amount(
		uint64(amount)*s.cfg.FeePPM/1_000_000,
	)
}

func (s *Server) validateAmount(amount btcutil.Amount) error {
	switch {
	case amount < s.cfg.MinSwapAmount:
		return status.Errorf(
			codes.InvalidArgument, "amount %d below minimum %d",
			amount, s.cfg.MinSwapAmount,
		)

	case amount > s.cfg.MaxSwapAmount:
		return status.Errorf(
			codes.InvalidArgument, "amount %d above maximum %d",
			amount, s.cfg.MaxSwapAmount,
		)
	}

	if s.swapFee(amount) >= amount {
		return status.Error(codes.InvalidArgument, "swap fee exceeds amount")
	}

	return nil
}

func (s *Server) currentHeight(ctx context.Context) (int32, error) {
	info, err := s.cfg.Lnd.Client.GetInfo(ctx)
	if err != nil {
		return 0, err
	}

	return int32(info.BlockHeight), nil
}

func decodeInvoice(params *zpay32.Invoice) error {
	if params.MilliSat == nil {
		return errors.New("amountless invoices are not supported")
	}
	if params.PaymentHash == nil {
		return errors.New("invoice has no payment hash")
	}

	return nil
}

func (s *Server) validateInvoice(invoice string, hash lntypes.Hash,
	expectedAmount btcutil.Amount) (*zpay32.Invoice, error) {

	decoded, err := zpay32.Decode(invoice, s.cfg.Lnd.ChainParams)
	if err != nil {
		return nil, status.Errorf(
			codes.InvalidArgument, "invalid invoice: %v", err,
		)
	}
	if err := decodeInvoice(decoded); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if *decoded.PaymentHash != hash {
		return nil, status.Error(
			codes.InvalidArgument, "invoice payment hash mismatch",
		)
	}

	want := lnwire.NewMSatFromSatoshis(expectedAmount)
	if *decoded.MilliSat != want {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"invoice amount %d msat does not match %d msat",
			*decoded.MilliSat, want,
		)
	}

	return decoded, nil
}

func cloneHash(hash chainhash.Hash) *chainhash.Hash {
	hashCopy := hash
	return &hashCopy
}
