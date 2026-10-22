package tapsdk

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

// OutputBinding is an independently approved concrete asset allocation. A
// grouped allocation expanded into multiple issuances needs one entry per
// issuance. The caller retains these terms before accepting a peer request.
// ScriptKey includes the raw derivation so a peer cannot change later wallet
// recovery authority while retaining the same public output key.
type OutputBinding struct {
	ID                string
	IssuanceID        sdk.AssetID
	AssetRef          sdk.AssetRef
	Amount            uint64
	AnchorOutputIndex uint32
	AnchorValueSat    int64
	ScriptMode        sdk.CustomAssetScriptMode
	ScriptKey         sdk.ScriptKey
	Anchor            swapkit.AnchorPlan
	OPTrue            *sdk.CustomAssetOPTrueSpendInfo
}

// ApprovePlan checks every native allocation and the whole Bitcoin template
// before Commit can sign virtual inputs. The independent template fixes every
// input, sequence, lock time, output value and non-asset output script; asset
// scripts are derived from approved contracts and native SDK commitment
// previews. No output ordering or fee is inferred from a peer's proposal.
// Native Build/Commit and durable approval remain owned by the caller's FSM.
func ApprovePlan(plan *sdk.CustomAnchorPlan, template *wire.MsgTx,
	outputs []OutputBinding, maxFee uint64) error {

	return approvePlan(plan, template, outputs, maxFee, false)
}

// ApproveWalletFundedPlan checks fixed allocations and template terms before
// wallet funding. A deficit template cannot establish a final fee or wallet
// change. Set the native request MaxFeeSat from independent policy and call
// CheckFundedPackage on Commit's result before signing, releasing or publishing
// its Bitcoin anchor. Only newly added wallet-generated change is supported.
func ApproveWalletFundedPlan(plan *sdk.CustomAnchorPlan, template *wire.MsgTx,
	outputs []OutputBinding) error {

	return approvePlan(plan, template, outputs, 0, true)
}

func approvePlan(plan *sdk.CustomAnchorPlan, template *wire.MsgTx,
	outputs []OutputBinding, maxFee uint64, deferredFunding bool) error {

	if plan == nil || template == nil {
		return swapkit.ErrInvalid
	}
	actual, err := DecodePSBT(plan.AnchorPSBT())
	if err != nil {
		return err
	}
	request := plan.Request()
	if request == nil {
		return swapkit.ErrInvalid
	}
	if err := checkSigningPlans(actual, request.SigningPlans); err != nil {
		return err
	}
	if deferredFunding || request.Funding.Mode ==
		sdk.CustomAnchorFundingWalletFunded {

		if err := checkWalletFunding(request.Funding); err != nil {
			return err
		}
	}
	tx := actual.UnsignedTx
	if tx.Version != template.Version || tx.LockTime != template.LockTime ||
		len(
			tx.TxIn,
		) != len(
			template.TxIn,
		) || len(tx.TxOut) != len(template.TxOut) ||
		len(actual.Inputs) != len(tx.TxIn) {

		return fmt.Errorf("Bitcoin template differs from approval")
	}
	for i, input := range tx.TxIn {
		expected := template.TxIn[i]
		if input == nil || expected == nil ||
			input.PreviousOutPoint != expected.PreviousOutPoint ||
			input.Sequence != expected.Sequence {

			return fmt.Errorf(
				"Bitcoin funding differs from approval",
			)
		}
	}
	planned := plan.Outputs()
	previews, err := plan.PreviewOutputCommitments()
	if err != nil {
		return err
	}
	if len(planned) != len(outputs) || len(previews) != len(outputs) {
		return fmt.Errorf(
			"asset allocation count differs from approval",
		)
	}
	active, passive := plan.ActiveVirtualPSBTs(), plan.PassiveVirtualPSBTs()
	anchors := make(map[uint32][]byte)
	matched := make([]bool, len(outputs))
	for i, out := range planned {
		index := -1
		for j, expected := range outputs {
			if !matched[j] && out.LogicalOutputID == expected.ID &&
				out.IssuanceID == expected.IssuanceID {

				index = j
				break
			}
		}
		if index < 0 {
			return fmt.Errorf(
				"unapproved asset issuance or allocation",
			)
		}
		matched[index] = true
		expected := outputs[index]
		if expected.ID == "" ||
			expected.IssuanceID == (sdk.AssetID{}) ||
			expected.Amount == 0 ||
			!out.AssetRef.Equivalent(expected.AssetRef) ||
			out.Amount != expected.Amount ||
			out.ScriptMode != expected.ScriptMode ||
			out.ScriptKey != expected.ScriptKey.PubKey ||
			out.AnchorOutputIndex != expected.AnchorOutputIndex ||
			out.AnchorValueSat != uint64(expected.AnchorValueSat) ||
			expected.AnchorValueSat < 330 {

			return fmt.Errorf(
				"asset allocation differs from approval",
			)
		}
		preview := previews[i]
		if preview.LogicalOutputID != out.LogicalOutputID ||
			preview.PacketIndex != out.PacketIndex ||
			preview.VirtualOutputIndex != out.VirtualOutputIndex ||
			preview.AnchorOutputIndex != out.AnchorOutputIndex {

			return swapkit.ErrInvalid
		}
		script, err := anchorScript(
			expected.Anchor,
			preview.TaprootAssetRoot,
		)
		if err != nil {
			return err
		}
		// Preview's full root incorporates the peer's anchor plan; deriving its
		// output key from the independent plan catches a substituted Bitcoin tree.
		actualScript, err := previewAnchorScript(
			expected.Anchor.InternalKey,
			preview.TaprootMerkleRoot,
		)
		if err != nil {
			return err
		}
		if !bytes.Equal(script, actualScript) {
			return fmt.Errorf(
				"output contract differs from approval",
			)
		}
		if prior := anchors[out.AnchorOutputIndex]; prior != nil &&
			!bytes.Equal(prior, script) {

			return fmt.Errorf(
				"asset output has conflicting contracts",
			)
		}
		anchors[out.AnchorOutputIndex] = script
		packets := active
		if out.PacketRole == sdk.CustomAnchorPacketRolePassive {
			packets = passive
		}
		if out.PacketIndex >= uint32(len(packets)) {
			return swapkit.ErrInvalid
		}
		packet, err := tappsbt.Decode(packets[out.PacketIndex])
		if err != nil {
			return err
		}
		if out.VirtualOutputIndex >= uint32(len(packet.Outputs)) {
			return swapkit.ErrInvalid
		}
		virtual := packet.Outputs[out.VirtualOutputIndex]
		if virtual.AnchorOutputInternalKey == nil ||
			!bytes.Equal(
				virtual.AnchorOutputInternalKey.SerializeCompressed(),
				expected.Anchor.InternalKey[:],
			) ||
			virtual.LockTime != 0 ||
			virtual.RelativeLockTime != 0 {

			return fmt.Errorf(
				"asset anchor authority or timelocks differ from approval",
			)
		}
		key := virtual.ScriptKey
		if key.TweakedScriptKey == nil || key.RawKey.PubKey == nil ||
			!bytes.Equal(
				key.RawKey.PubKey.SerializeCompressed(),
				expected.ScriptKey.KeyDesc.RawKeyBytes[:],
			) ||
			uint32(
				key.RawKey.Family,
			) != expected.ScriptKey.KeyDesc.KeyLocator.Family ||
			key.RawKey.Index != expected.ScriptKey.KeyDesc.KeyLocator.Index ||
			!bytes.Equal(key.Tweak, expected.ScriptKey.TapTweak) {

			return fmt.Errorf(
				"asset key derivation differs from approval",
			)
		}
		if expected.OPTrue != nil {
			if err := expected.OPTrue.Validate(out.ScriptKey); err != nil {
				return err
			}
		}
	}
	var inputValue, outputValue uint64
	for _, input := range actual.Inputs {
		if input.WitnessUtxo == nil || input.WitnessUtxo.Value < 0 ||
			uint64(
				input.WitnessUtxo.Value,
			) > math.MaxUint64-inputValue {

			return swapkit.ErrInvalid
		}
		inputValue += uint64(input.WitnessUtxo.Value)
	}
	for i, output := range tx.TxOut {
		expected := template.TxOut[i]
		if output == nil || expected == nil || output.Value < 0 ||
			output.Value != expected.Value ||
			uint64(output.Value) > math.MaxUint64-outputValue {

			return swapkit.ErrInvalid
		}
		// The SDK installs asset commitments during Commit. The template still
		// carries the caller's placeholder; the approved contract was checked above.
		if anchors[uint32(i)] == nil &&
			!bytes.Equal(output.PkScript, expected.PkScript) {

			return fmt.Errorf(
				"Bitcoin recipient differs from approval",
			)
		}
		outputValue += uint64(output.Value)
	}
	if !deferredFunding &&
		(outputValue > inputValue || inputValue-outputValue > maxFee) {

		return fmt.Errorf("Bitcoin fee exceeds approval")
	}
	return nil
}

func previewAnchorScript(internalKey [33]byte, root sdk.Hash) ([]byte, error) {
	internal, err := btcec.ParsePubKey(internalKey[:])
	if err != nil {
		return nil, err
	}
	return txscript.PayToTaprootScript(
		txscript.ComputeTaprootOutputKey(internal, root[:]),
	)
}

// CheckFundedPackage validates native wallet funding and its independently held
// absolute fee cap. It supplements fixed-allocation preapproval; the caller
// also retains the SDK committed digest and owns anchor signing/publication.
func CheckFundedPackage(
	pkg *sdk.CustomAnchorTransferPackage,
	maxFee uint64,
) error {

	if pkg == nil || maxFee == 0 {
		return swapkit.ErrInvalid
	}
	if err := CheckSigningPolicy(pkg); err != nil {
		return err
	}
	if pkg.Funding.Mode != sdk.CustomAnchorFundingWalletFunded ||
		pkg.Funding.MaxFeeSat != maxFee || pkg.Funding.ActualFeeSat > maxFee {

		return fmt.Errorf(
			"native wallet funding differs from approved fee cap",
		)
	}
	if pkg.ChangeOutputIndex >= 0 {
		packet, err := DecodePSBT(pkg.CommittedAnchorPsbt)
		if err != nil {
			return err
		}
		if int(pkg.ChangeOutputIndex) != len(packet.UnsignedTx.TxOut)-1 {
			return fmt.Errorf("wallet change must be the added final output")
		}
		for _, output := range pkg.Outputs {
			if output.AnchorOutputIndex == uint32(pkg.ChangeOutputIndex) {
				return fmt.Errorf("wallet change cannot replace an asset output")
			}
		}
	}
	return nil
}

// checkWalletFunding retains fixed approved recipients by allowing only a new
// wallet-generated change output. Existing and NoNew can change the accounting
// of an independently approved recipient without changing its script.
func checkWalletFunding(funding sdk.CustomAnchorFundingPlan) error {
	if funding.Mode != sdk.CustomAnchorFundingWalletFunded ||
		funding.WalletFunded == nil || funding.WalletFunded.ChangeOutput.Mode !=
		sdk.AnchorChangeOutputAdd {

		return fmt.Errorf("wallet funding requires newly added wallet change")
	}
	return nil
}
