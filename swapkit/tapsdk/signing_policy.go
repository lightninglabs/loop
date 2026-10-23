package tapsdk

import (
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
)

// CheckSigningPolicy validates a native package and requires SIGHASH_DEFAULT
// for every caller-declared signing plan. Call it before creating a MuSig2
// session or signing a caller-funded package. The native serialized package
// must also fit DecodePackage's recovery limit before signatures are exposed.
// Backend-managed wallet fee inputs retain the native wallet's signing policy.
func CheckSigningPolicy(pkg *sdk.CustomAnchorTransferPackage) error {
	if pkg == nil {
		return ErrSigningRequest
	}
	if err := pkg.Validate(); err != nil {
		return err
	}
	// Finalized PSBTs may omit sighash metadata. The immutable committed
	// PSBT is the source used by the SDK's signing requests.
	packet, err := DecodePSBT(pkg.CommittedAnchorPsbt)
	if err != nil {
		return err
	}
	if err := checkSigningPlans(packet, pkg.SigningPlans); err != nil {
		return err
	}
	raw, err := pkg.MarshalBinary()
	if err != nil {
		return err
	}
	if len(raw) > MaxPackageBytes {
		return fmt.Errorf("%w: native package exceeds %d-byte recovery limit",
			ErrSigningRequest, MaxPackageBytes)
	}
	return nil
}

func checkSigningPlans(packet *psbt.Packet,
	plans []sdk.CustomAnchorInputSigningPlan) error {

	if packet == nil || packet.UnsignedTx == nil ||
		len(packet.Inputs) != len(packet.UnsignedTx.TxIn) {

		return ErrSigningRequest
	}
	for _, plan := range plans {
		if plan.InputIndex >= uint32(len(packet.Inputs)) {
			return ErrSigningRequest
		}
		if packet.Inputs[plan.InputIndex].SighashType !=
			txscript.SigHashDefault {

			return fmt.Errorf("%w: input %d requires SIGHASH_DEFAULT",
				ErrSigningRequest, plan.InputIndex)
		}
	}
	return nil
}
