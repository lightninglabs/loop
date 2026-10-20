package walletsigner

import (
	"bytes"
	"context"
	"math"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

const (
	scriptMethod = signrpc.SignMethod_SIGN_METHOD_TAPROOT_SCRIPT_SPEND
	bip86Method  = signrpc.SignMethod_SIGN_METHOD_TAPROOT_KEY_SPEND_BIP0086
)

// ScriptPath signs one validated SDK leaf with an authenticated wallet key.
// The caller authorizes the package and assembles its preimage/timeout witness;
// ApplyScriptPathWitness verifies the complete witness before publication.
func (s *Signer) ScriptPath(ctx context.Context, key keychain.KeyLocator,
	pkg *sdk.CustomAnchorTransferPackage, requestID sdk.Hash) (
	[]byte, error) {

	if pkg == nil || key.Family < 0 || key.Index > math.MaxInt32 {
		return nil, ErrInvalid
	}
	requests, err := pkg.SigningRequests()
	if err != nil {
		return nil, err
	}
	public, err := s.deriveKey(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, request := range requests.ScriptPath {
		if request.ID != requestID {
			continue
		}
		found := false
		for _, required := range request.RequiredSigners {
			found = found || bytes.Equal(required[:], public[1:])
		}
		if !found ||
			request.LeafVersion != byte(txscript.BaseLeafVersion) {

			return nil, ErrBinding
		}
		return s.signOutput(ctx, key, public, pkg, request.InputIndex,
			request.Sighash, &signrpc.SignDescriptor{
				SignMethod:    scriptMethod,
				WitnessScript: bytes.Clone(request.LeafScript),
				Sighash:       request.SighashType,
			})
	}
	return nil, ErrBinding
}

// KeyPath signs a single-key Taproot request using its wallet locator.
func (s *Signer) KeyPath(ctx context.Context, key keychain.KeyLocator,
	pkg *sdk.CustomAnchorTransferPackage, requestID sdk.Hash) (
	[]byte, error) {

	if pkg == nil || key.Family < 0 || key.Index > math.MaxInt32 {
		return nil, ErrInvalid
	}
	requests, err := pkg.SigningRequests()
	if err != nil {
		return nil, err
	}
	public, err := s.deriveKey(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, request := range requests.KeyPath {
		if request.ID != requestID {
			continue
		}
		if !bytes.Equal(request.Signer[:], public[1:]) {
			return nil, ErrBinding
		}
		method := signrpc.SignMethod_SIGN_METHOD_TAPROOT_KEY_SPEND
		if len(request.TaprootMerkleRoot) == 0 {
			method = bip86Method
		}
		return s.signOutput(ctx, key, public, pkg, request.InputIndex,
			request.Sighash, &signrpc.SignDescriptor{
				SignMethod: method,
				Sighash:    request.SighashType,
				TapTweak: bytes.Clone(
					request.TaprootMerkleRoot,
				),
			})
	}
	return nil, ErrBinding
}

func (s *Signer) signOutput(ctx context.Context, key keychain.KeyLocator,
	public sdk.PubKey, pkg *sdk.CustomAnchorTransferPackage, index uint32,
	sighash sdk.Hash, descriptor *signrpc.SignDescriptor) ([]byte, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(pkg.AnchorPsbt), false,
	)
	if err != nil {
		return nil, err
	}
	if uint64(index) >= uint64(len(packet.Inputs)) ||
		index > math.MaxInt32 {

		return nil, ErrInvalid
	}
	previous := make([]*signrpc.TxOut, len(packet.Inputs))
	for i, input := range packet.Inputs {
		output := input.WitnessUtxo
		if output == nil && input.NonWitnessUtxo != nil {
			point := packet.UnsignedTx.TxIn[i].PreviousOutPoint
			outputs := input.NonWitnessUtxo.TxOut
			if point.Hash != input.NonWitnessUtxo.TxHash() ||
				uint64(point.Index) >= uint64(len(outputs)) {

				return nil, ErrInvalid
			}
			output = input.NonWitnessUtxo.TxOut[point.Index]
		}
		if output == nil {
			return nil, ErrInvalid
		}
		previous[i] = &signrpc.TxOut{
			Value:    output.Value,
			PkScript: bytes.Clone(output.PkScript),
		}
	}
	var transaction bytes.Buffer
	if err := packet.UnsignedTx.Serialize(&transaction); err != nil {
		return nil, err
	}
	descriptor.KeyDesc = &signrpc.KeyDescriptor{
		KeyLoc: rpcLocator(key), RawKeyBytes: bytes.Clone(public[:]),
	}
	descriptor.InputIndex = int32(index)
	descriptor.Output = previous[index]
	response, err := s.rpc.SignOutputRaw(ctx, &signrpc.SignReq{
		RawTxBytes: transaction.Bytes(), PrevOutputs: previous,
		SignDescs: []*signrpc.SignDescriptor{descriptor},
	})
	if err != nil {
		return nil, err
	}
	if response == nil || len(response.RawSigs) != 1 ||
		len(response.RawSigs[0]) != 64 {

		return nil, ErrInvalid
	}
	signature, err := schnorr.ParseSignature(response.RawSigs[0])
	if err != nil {
		return nil, ErrInvalid
	}
	verificationKey, err := schnorr.ParsePubKey(public[1:])
	if err != nil {
		return nil, err
	}
	if descriptor.SignMethod !=
		scriptMethod {

		verificationKey = txscript.ComputeTaprootOutputKey(
			verificationKey, descriptor.TapTweak,
		)
	}
	if !signature.Verify(sighash[:], verificationKey) {
		return nil, ErrInvalid
	}
	signatureBytes := withSighash(
		bytes.Clone(response.RawSigs[0]), descriptor.Sighash,
	)
	return signatureBytes, nil
}

// AnchorSigner returns the SDK callback for wallet-managed Bitcoin inputs.
// The SDK must still verify the returned PSBT against its sealed package.
func (s *Signer) AnchorSigner() sdk.AnchorSigner {
	return func(ctx context.Context, raw []byte) ([]byte, error) {
		signed, err := s.wallet.SignPsbt(
			ctx, &walletrpc.SignPsbtRequest{
				FundedPsbt: bytes.Clone(raw),
			},
		)
		if err != nil {
			return nil, err
		}
		if signed == nil || len(signed.SignedPsbt) == 0 {
			return nil, ErrInvalid
		}
		final, err := s.wallet.FinalizePsbt(ctx,
			&walletrpc.FinalizePsbtRequest{
				FundedPsbt: signed.SignedPsbt,
			})
		if err != nil {
			return nil, err
		}
		if final == nil || len(final.SignedPsbt) == 0 {
			return nil, ErrInvalid
		}
		return bytes.Clone(final.SignedPsbt), nil
	}
}
