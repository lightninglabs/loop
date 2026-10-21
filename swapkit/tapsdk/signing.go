package tapsdk

import (
	"bytes"
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// ErrSigningRequest means an SDK signing request failed its fixed binding.
var ErrSigningRequest = errors.New("invalid asset signing request")

// FinalizeScriptSpend uses the normal LND signer for a unilateral spend.
// The SDK checks the resulting complete witness against its sealed package.
func FinalizeScriptSpend(ctx context.Context, signer lndclient.SignerClient,
	key keychain.KeyLocator, pkg *sdk.CustomAnchorTransferPackage,
	preimage *[32]byte) (*sdk.CustomAnchorTransferPackage, error) {

	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.ScriptPath) != 1 {
		return nil, ErrSigningRequest
	}
	request := requests.ScriptPath[0]
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(pkg.AnchorPsbt), false)
	if err != nil {
		return nil, err
	}
	previous := make([]*wire.TxOut, len(packet.Inputs))
	for i := range packet.Inputs {
		previous[i] = packet.Inputs[i].WitnessUtxo
		if previous[i] == nil {
			return nil, ErrSigningRequest
		}
	}
	signatures, err := signer.SignOutputRawKeyLocator(ctx,
		packet.UnsignedTx, []*lndclient.SignDescriptor{{
			KeyDesc:       keychain.KeyDescriptor{KeyLocator: key},
			SignMethod:    input.TaprootScriptSpendSignMethod,
			WitnessScript: request.LeafScript,
			HashType:      txscript.SigHashType(request.SighashType),
			Output:        previous[request.InputIndex],
			InputIndex:    int(request.InputIndex),
		}}, previous)
	if err != nil {
		return nil, err
	}
	if len(signatures) != 1 {
		return nil, ErrSigningRequest
	}
	signature := signatures[0]
	if request.SighashType != uint32(txscript.SigHashDefault) {
		signature = append(signature, byte(request.SighashType))
	}
	unlock := [][]byte{signature}
	if preimage != nil {
		unlock = append([][]byte{preimage[:]}, unlock...)
	}
	final, err := pkg.ApplyScriptPathWitness(request.ID, unlock)
	if err != nil {
		return nil, err
	}
	packet, err = DecodePSBT(final.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	if err := psbt.MaybeFinalizeAll(packet); err != nil {
		return nil, err
	}
	raw, err := EncodePSBT(packet)
	if err != nil {
		return nil, err
	}
	return pkg.WithFinalAnchorPSBT(raw)
}
