package tapsdk

import (
	"bytes"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomAnchorRequestValidate(t *testing.T) {
	request := validCustomAnchorRequest(t)
	require.NoError(t, request.Validate())

	var nilRequest *CustomAnchorRequest
	require.ErrorContains(t, nilRequest.Validate(), "nil custom anchor request")
}

func TestCustomAnchorRequestValidateRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CustomAnchorRequest)
		wantErr string
	}{
		{
			name: "missing anchor psbt",
			mutate: func(request *CustomAnchorRequest) {
				request.AnchorPSBT = nil
			},
			wantErr: "anchor PSBT is required",
		},
		{
			name: "missing inputs",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs = nil
			},
			wantErr: "at least one asset input is required",
		},
		{
			name: "missing outputs",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs = nil
			},
			wantErr: "at least one asset output is required",
		},
		{
			name: "missing input id",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].ID = ""
			},
			wantErr: "input ID is required",
		},
		{
			name: "duplicate input id",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs = append(
					request.Inputs, request.Inputs[0],
				)
				request.Inputs[0].Amount = 50
				request.Inputs[1].Amount = 50
			},
			wantErr: testDuplicateId,
		},
		{
			name: "invalid input asset ref",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].AssetRef = AssetRef("invalid")
			},
			wantErr: "input 0: asset ref",
		},
		{
			name: "missing input proof",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].ProofFile = nil
			},
			wantErr: "exactly one proof file or proof path is required",
		},
		{
			name: "missing output id",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].ID = ""
			},
			wantErr: "output ID is required",
		},
		{
			name: "duplicate output id",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].Amount = 200
				request.Outputs = append(
					request.Outputs, request.Outputs[0],
				)
			},
			wantErr: testDuplicateId,
		},
		{
			name: "invalid output asset ref",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].AssetRef = AssetRef("invalid")
			},
			wantErr: "output 0: asset ref",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validCustomAnchorRequest(t)
			test.mutate(request)

			err := request.Validate()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAssetWitnessPlanValidate(t *testing.T) {
	tests := []struct {
		name    string
		plan    CustomAssetWitnessPlan
		wantErr string
	}{
		{
			name:    "unspecified",
			plan:    CustomAssetWitnessPlan{},
			wantErr: "asset witness mode is required",
		},
		{
			name: "backend with stack",
			plan: CustomAssetWitnessPlan{
				Mode:  CustomAssetWitnessBackendSigner,
				Stack: [][]byte{{1}},
			},
			wantErr: "cannot include a caller witness stack",
		},
		{
			name: "caller without stack",
			plan: CustomAssetWitnessPlan{
				Mode: CustomAssetWitnessCallerProvided,
			},
			wantErr: "caller-provided witness stack is required",
		},
		{
			name: "backend",
			plan: CustomAssetWitnessPlan{
				Mode: CustomAssetWitnessBackendSigner,
			},
		},
		{
			name: "caller",
			plan: CustomAssetWitnessPlan{
				Mode:  CustomAssetWitnessCallerProvided,
				Stack: [][]byte{{}, {1}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.plan.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAssetScriptPlanValidateExactVariant(t *testing.T) {
	key := customAnchorTypesTestPubKey(t)
	validExternal := &CustomAssetExternalScriptPlan{
		ScriptKey: ScriptKey{
			PubKey: key,
		},
	}

	tests := []struct {
		name    string
		plan    CustomAssetScriptPlan
		wantErr string
	}{
		{
			name:    "missing variant",
			plan:    CustomAssetScriptPlan{},
			wantErr: testExactlyOneVariant,
		},
		{
			name: testMultipleVariants,
			plan: CustomAssetScriptPlan{
				Mode:     CustomAssetScriptExternal,
				External: validExternal,
				Burn:     &CustomAssetBurnScriptPlan{},
			},
			wantErr: testExactlyOneVariant,
		},
		{
			name: "mode mismatch",
			plan: CustomAssetScriptPlan{
				Mode:     CustomAssetScriptWallet,
				External: validExternal,
			},
			wantErr: "wallet variant",
		},
		{
			name: "invalid external key",
			plan: CustomAssetScriptPlan{
				Mode: CustomAssetScriptExternal,
				External: &CustomAssetExternalScriptPlan{
					ScriptKey: ScriptKey{},
				},
			},
			wantErr: "external script key",
		},
		{
			name: "invalid op true key",
			plan: CustomAssetScriptPlan{
				Mode: CustomAssetScriptOPTrue,
				OPTrue: &CustomAssetOPTrueScriptPlan{
					InternalKey: KeyDescriptor{},
				},
			},
			wantErr: "OP_TRUE internal key",
		},
		{
			name: "wallet",
			plan: CustomAssetScriptPlan{
				Mode:   CustomAssetScriptWallet,
				Wallet: &CustomAssetWalletScriptPlan{},
			},
		},
		{
			name: "external",
			plan: CustomAssetScriptPlan{
				Mode:     CustomAssetScriptExternal,
				External: validExternal,
			},
		},
		{
			name: "op true",
			plan: CustomAssetScriptPlan{
				Mode: CustomAssetScriptOPTrue,
				OPTrue: &CustomAssetOPTrueScriptPlan{
					InternalKey: KeyDescriptor{
						RawKeyBytes: key,
					},
				},
			},
		},
		{
			name: "burn",
			plan: CustomAssetScriptPlan{
				Mode: CustomAssetScriptBurn,
				Burn: &CustomAssetBurnScriptPlan{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.plan.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTapscriptPlanValidate(t *testing.T) {
	tests := []struct {
		name    string
		plan    CustomAnchorTapscriptPlan
		wantErr string
	}{
		{
			name: "direct anchor",
		},
		{
			name: "tap leaves",
			plan: CustomAnchorTapscriptPlan{
				TapLeaves: []TapLeaf{{Script: []byte{1}}},
			},
		},
		{
			name: "serialized sibling",
			plan: CustomAnchorTapscriptPlan{
				SerializedSibling: []byte{1},
			},
		},
		{
			name: "both forms",
			plan: CustomAnchorTapscriptPlan{
				TapLeaves:         []TapLeaf{{Script: []byte{1}}},
				SerializedSibling: []byte{2},
			},
			wantErr: "cannot contain both",
		},
		{
			name: "empty leaf",
			plan: CustomAnchorTapscriptPlan{
				TapLeaves: []TapLeaf{{}},
			},
			wantErr: "tap leaf 0 script is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.plan.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorFundingPlanValidateExactVariant(t *testing.T) {
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)

	validWallet := &CustomAnchorWalletFunding{
		ChangeOutput: AnchorChangeOutput{
			Mode: AnchorChangeOutputNoNew,
		},
		Fee: AnchorFee{
			Mode:    AnchorFeeSatPerVByte,
			FeeRate: feeRate,
		},
		MaxFeeSat:    10_000,
		CustomLockID: bytes.Repeat([]byte{1}, 32),
	}

	tests := []struct {
		name    string
		plan    CustomAnchorFundingPlan
		wantErr string
	}{
		{
			name:    "missing variant",
			plan:    CustomAnchorFundingPlan{},
			wantErr: testExactlyOneVariant,
		},
		{
			name: testMultipleVariants,
			plan: CustomAnchorFundingPlan{
				Mode:              CustomAnchorFundingWalletFunded,
				WalletFunded:      validWallet,
				CallerFundedExact: &CustomAnchorCallerFundedExact{},
			},
			wantErr: testExactlyOneVariant,
		},
		{
			name: "mode mismatch",
			plan: CustomAnchorFundingPlan{
				Mode:              CustomAnchorFundingWalletFunded,
				CallerFundedExact: &CustomAnchorCallerFundedExact{},
			},
			wantErr: "wallet variant",
		},
		{
			name: "wallet missing change",
			plan: CustomAnchorFundingPlan{
				Mode: CustomAnchorFundingWalletFunded,
				WalletFunded: &CustomAnchorWalletFunding{
					Fee: validWallet.Fee,
				},
			},
			wantErr: "anchor change output is required",
		},
		{
			name: "wallet missing fee",
			plan: CustomAnchorFundingPlan{
				Mode: CustomAnchorFundingWalletFunded,
				WalletFunded: &CustomAnchorWalletFunding{
					ChangeOutput: validWallet.ChangeOutput,
				},
			},
			wantErr: "anchor fee is required",
		},
		{
			name: "wallet missing maximum fee",
			plan: CustomAnchorFundingPlan{
				Mode: CustomAnchorFundingWalletFunded,
				WalletFunded: &CustomAnchorWalletFunding{
					ChangeOutput: validWallet.ChangeOutput,
					Fee:          validWallet.Fee,
					CustomLockID: validWallet.CustomLockID,
				},
			},
			wantErr: "maximum fee is required",
		},
		{
			name: "wallet funded",
			plan: CustomAnchorFundingPlan{
				Mode:         CustomAnchorFundingWalletFunded,
				WalletFunded: validWallet,
			},
		},
		{
			name: "caller exact",
			plan: CustomAnchorFundingPlan{
				Mode:              CustomAnchorFundingCallerFundedExact,
				CallerFundedExact: &CustomAnchorCallerFundedExact{},
			},
		},
		{
			name: "external p2a",
			plan: CustomAnchorFundingPlan{
				Mode: CustomAnchorFundingExternalP2AFeeBump,
				ExternalP2AFeeBump: &CustomAnchorExternalP2AFeeBump{
					P2AOutputIndex: 0,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.plan.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorFundingPlanValidateLockID(t *testing.T) {
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)

	tests := []struct {
		name    string
		lockID  []byte
		wantErr string
	}{
		{
			name:    testEmpty,
			wantErr: "wallet-funded custom lock ID is required",
		},
		{
			name:    "31 bytes",
			lockID:  bytes.Repeat([]byte{1}, 31),
			wantErr: "custom lock ID must be exactly 32 bytes",
		},
		{
			name:   "32 bytes",
			lockID: bytes.Repeat([]byte{2}, 32),
		},
		{
			name:    "33 bytes",
			lockID:  bytes.Repeat([]byte{3}, 33),
			wantErr: "custom lock ID must be exactly 32 bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			funding := CustomAnchorFundingPlan{
				Mode: CustomAnchorFundingWalletFunded,
				WalletFunded: &CustomAnchorWalletFunding{
					ChangeOutput: AnchorChangeOutput{
						Mode: AnchorChangeOutputAdd,
					},
					Fee: AnchorFee{
						Mode:    AnchorFeeSatPerVByte,
						FeeRate: feeRate,
					},
					MaxFeeSat:    10_000,
					CustomLockID: test.lockID,
				},
			}

			err := funding.Validate()
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestCommitVirtualPsbtsRequestValidateLockID(t *testing.T) {
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)

	tests := []struct {
		name    string
		lockID  []byte
		wantErr bool
	}{
		{
			name: testEmpty,
		},
		{
			name:    "31 bytes",
			lockID:  bytes.Repeat([]byte{1}, 31),
			wantErr: true,
		},
		{
			name:   "32 bytes",
			lockID: bytes.Repeat([]byte{2}, 32),
		},
		{
			name:    "33 bytes",
			lockID:  bytes.Repeat([]byte{3}, 33),
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &CommitVirtualPsbtsRequest{
				AnchorPsbt:   []byte{1},
				VirtualPsbts: [][]byte{{2}},
				Funding: AnchorFundingPlan{
					ChangeOutput: AnchorChangeOutput{
						Mode: AnchorChangeOutputAdd,
					},
					Fee: AnchorFee{
						Mode:    AnchorFeeSatPerVByte,
						FeeRate: feeRate,
					},
					CustomLockID: test.lockID,
				},
			}

			err := request.Validate()
			if test.wantErr {
				require.ErrorContains(
					t, err, "custom lock ID must be exactly 32 bytes",
				)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestCustomAnchorFundingLockExpiration(t *testing.T) {
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)

	tests := []struct {
		name    string
		seconds uint64
		wantErr bool
	}{
		{
			name:    "maximum",
			seconds: maxCustomAnchorLockExpirationSeconds,
		},
		{
			name:    "overflow",
			seconds: maxCustomAnchorLockExpirationSeconds + 1,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			highLevel := CustomAnchorFundingPlan{
				Mode: CustomAnchorFundingWalletFunded,
				WalletFunded: &CustomAnchorWalletFunding{
					ChangeOutput: AnchorChangeOutput{
						Mode: AnchorChangeOutputAdd,
					},
					Fee: AnchorFee{
						Mode:    AnchorFeeSatPerVByte,
						FeeRate: feeRate,
					},
					MaxFeeSat:             10_000,
					CustomLockID:          bytes.Repeat([]byte{1}, 32),
					LockExpirationSeconds: test.seconds,
				},
			}
			lowLevel := CommitVirtualPsbtsRequest{
				AnchorPsbt:   []byte{1},
				VirtualPsbts: [][]byte{{2}},
				Funding: AnchorFundingPlan{
					ChangeOutput: AnchorChangeOutput{
						Mode: AnchorChangeOutputAdd,
					},
					Fee: AnchorFee{
						Mode:    AnchorFeeSatPerVByte,
						FeeRate: feeRate,
					},
					LockExpirationSeconds: test.seconds,
				},
			}

			highErr := highLevel.Validate()
			lowErr := lowLevel.Validate()
			if test.wantErr {
				require.ErrorContains(
					t, highErr, "maximum safe duration",
				)
				require.ErrorContains(
					t, lowErr, "maximum safe duration",
				)
				return
			}

			require.NoError(t, highErr)
			require.NoError(t, lowErr)
		})
	}
}

func TestCustomAssetOutputValidateMVPConstraints(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CustomAnchorRequest)
		wantErr string
	}{
		{
			name: "zero amount",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 0
			},
			wantErr: "amount is required",
		},
		{
			name: "zero anchor value",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].AnchorValueSat = 0
			},
			wantErr: "anchor value is required",
		},
		{
			name: "zero burn anchor value",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].AnchorValueSat = 0
				request.Outputs[0].Script = customAnchorBurnScript()
				request.LossPolicy = customAnchorLossPolicy(
					request.Inputs[0].AssetRef, 100,
				)
			},
			wantErr: "anchor value is required",
		},
		{
			name: "absolute timelock",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Timelocks.Absolute = 1
			},
			wantErr: "asset timelocks are not supported",
		},
		{
			name: "relative timelock",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Timelocks.Relative = 1
			},
			wantErr: "asset timelocks are not supported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validCustomAnchorRequest(t)
			test.mutate(request)

			err := request.Validate()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorRequestValidateCoAnchoredOutputs(t *testing.T) {
	valid := validCustomAnchorRequest(t)
	valid.Inputs[0].Amount = 200
	valid.Outputs = append(valid.Outputs, valid.Outputs[0])
	valid.Outputs[1].ID = "output-2"
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*CustomAnchorRequest)
	}{
		{
			name: "different value",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[1].AnchorValueSat++
			},
		},
		{
			name: "different internal key",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[1].Anchor.InternalKey.PubKey =
					customAnchorTypesTestOddPubKey(t)
			},
		},
		{
			name: "different serialized sibling",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[1].Anchor.Tapscript.SerializedSibling =
					[]byte{9}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid.Clone()
			test.mutate(request)

			err := request.Validate()
			require.ErrorContains(t, err, "conflicts with anchor output")
		})
	}
}

func TestCustomAnchorRequestValidateCheckedAmounts(t *testing.T) {
	secondRef := AssetRefFromAssetID(AssetID{9})

	tests := []struct {
		name    string
		mutate  func(*CustomAnchorRequest)
		wantErr string
	}{
		{
			name: "zero input",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].Amount = 0
			},
			wantErr: "input 0: amount is required",
		},
		{
			name: "input sum overflow",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].Amount = math.MaxUint64
				second := request.Inputs[0]
				second.ID = "input-2"
				second.Amount = 1
				request.Inputs = append(request.Inputs, second)
				request.Outputs[0].Amount = math.MaxUint64
			},
			wantErr: "input amount for asset",
		},
		{
			name: "output sum overflow",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].Amount = math.MaxUint64
				request.Outputs[0].Amount = math.MaxUint64
				second := request.Outputs[0]
				second.ID = "output-2"
				second.Amount = 1
				second.AnchorOutputIndex = 2
				request.Outputs = append(request.Outputs, second)
			},
			wantErr: "output amount for asset",
		},
		{
			name: "output exceeds input",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 101
			},
			wantErr: "exceeds input amount",
		},
		{
			name: "unknown output asset",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].AssetRef = secondRef
			},
			wantErr: "exceeds input amount",
		},
		{
			name: "unapproved deficit",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 90
			},
			wantErr: testImplicitAssetLossIsUnsupported,
		},
		{
			name: "approved deficit",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 90
				request.LossPolicy = customAnchorLossPolicy(
					request.Inputs[0].AssetRef, 10,
				)
			},
			wantErr: testImplicitAssetLossIsUnsupported,
		},
		{
			name: "deficit exceeds bound",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 89
				request.LossPolicy = customAnchorLossPolicy(
					request.Inputs[0].AssetRef, 10,
				)
			},
			wantErr: testImplicitAssetLossIsUnsupported,
		},
		{
			name: "unconfirmed loss",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 90
				request.LossPolicy = customAnchorLossPolicy(
					request.Inputs[0].AssetRef, 10,
				)
				request.LossPolicy.ConfirmIrreversibleLoss = false
			},
			wantErr: "irreversible asset loss is not confirmed",
		},
		{
			name: "approved burn output",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Amount = 60
				burn := request.Outputs[0]
				burn.ID = "burn-output"
				burn.Amount = 40
				burn.AnchorOutputIndex = 2
				burn.Script = customAnchorBurnScript()
				request.Outputs = append(request.Outputs, burn)
				request.LossPolicy = customAnchorLossPolicy(
					request.Inputs[0].AssetRef, 40,
				)
			},
		},
		{
			name: "burn output rejected by default",
			mutate: func(request *CustomAnchorRequest) {
				request.Outputs[0].Script = customAnchorBurnScript()
			},
			wantErr: "irreversible loss 100 exceeds allowance 0",
		},
		{
			name: "loss allowance sum overflow",
			mutate: func(request *CustomAnchorRequest) {
				request.LossPolicy = CustomAnchorLossPolicy{
					Mode: CustomAnchorLossBurn,
					Allowances: []CustomAnchorLossAllowance{
						{
							AssetRef:  request.Inputs[0].AssetRef,
							MaxAmount: math.MaxUint64,
						},
						{
							AssetRef:  secondRef,
							MaxAmount: 1,
						},
					},
					ConfirmIrreversibleLoss: true,
				}
			},
			wantErr: "loss allowances: amount overflows uint64",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validCustomAnchorRequest(t)
			test.mutate(request)

			err := request.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorRequestValidateSemanticGroupAmounts(t *testing.T) {
	evenKey := customAnchorTypesTestPubKey(t)
	oddKey := customAnchorTypesTestOddPubKey(t)
	evenRef := AssetRefFromGroupKey(evenKey)
	oddRef := AssetRefFromGroupKey(oddKey)
	require.True(t, evenRef.Equivalent(oddRef))

	request := validCustomAnchorRequest(t)
	request.Inputs[0].AssetRef = evenRef
	request.Outputs[0].AssetRef = oddRef

	require.NoError(t, request.Validate())
}

func TestCustomAnchorLossPolicyValidate(t *testing.T) {
	assetRef := AssetRefFromAssetID(AssetID{1})

	tests := []struct {
		name    string
		policy  CustomAnchorLossPolicy
		wantErr string
	}{
		{
			name: "reject",
		},
		{
			name: "reject with confirmation",
			policy: CustomAnchorLossPolicy{
				ConfirmIrreversibleLoss: true,
			},
			wantErr: "reject loss mode cannot include",
		},
		{
			name: "burn without confirmation",
			policy: CustomAnchorLossPolicy{
				Mode: CustomAnchorLossBurn,
				Allowances: []CustomAnchorLossAllowance{{
					AssetRef:  assetRef,
					MaxAmount: 1,
				}},
			},
			wantErr: "irreversible asset loss is not confirmed",
		},
		{
			name: "burn without allowance",
			policy: CustomAnchorLossPolicy{
				Mode:                    CustomAnchorLossBurn,
				ConfirmIrreversibleLoss: true,
			},
			wantErr: "at least one loss allowance is required",
		},
		{
			name: "zero allowance",
			policy: CustomAnchorLossPolicy{
				Mode: CustomAnchorLossBurn,
				Allowances: []CustomAnchorLossAllowance{{
					AssetRef: assetRef,
				}},
				ConfirmIrreversibleLoss: true,
			},
			wantErr: "allowance 0 amount is required",
		},
		{
			name: "duplicate allowance",
			policy: CustomAnchorLossPolicy{
				Mode: CustomAnchorLossBurn,
				Allowances: []CustomAnchorLossAllowance{
					{
						AssetRef:  assetRef,
						MaxAmount: 1,
					},
					{
						AssetRef:  assetRef,
						MaxAmount: 2,
					},
				},
				ConfirmIrreversibleLoss: true,
			},
			wantErr: "duplicates asset",
		},
		{
			name:   "burn",
			policy: customAnchorLossPolicy(assetRef, 1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.policy.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorPassiveAssetsValidate(t *testing.T) {
	assetRef := AssetRefFromAssetID(AssetID{1})
	packet := CustomAnchorPassivePacket{
		ID:          testPassive1,
		AssetRef:    assetRef,
		Amount:      10,
		VirtualPSBT: []byte{1},
		ProofFile:   []byte{2},
	}

	tests := []struct {
		name    string
		plan    CustomAnchorPassiveAssets
		wantErr string
	}{
		{
			name: "reject",
		},
		{
			name: "preserve",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassivePreserve,
			},
		},
		{
			name: "reject with packet",
			plan: CustomAnchorPassiveAssets{
				Packets: []CustomAnchorPassivePacket{packet},
			},
			wantErr: "packets require caller re-anchor",
		},
		{
			name: "preserve with packet",
			plan: CustomAnchorPassiveAssets{
				Policy:  CustomAnchorPassivePreserve,
				Packets: []CustomAnchorPassivePacket{packet},
			},
			wantErr: "packets require caller re-anchor",
		},
		{
			name: "caller without packet",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassiveCallerReanchor,
			},
			wantErr: "caller re-anchor requires passive packets",
		},
		{
			name: "caller missing packet id",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{
					{
						AssetRef:    assetRef,
						Amount:      1,
						VirtualPSBT: []byte{1},
					},
				},
			},
			wantErr: "ID is required",
		},
		{
			name: "caller duplicate packet id",
			plan: CustomAnchorPassiveAssets{
				Policy:  CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{packet, packet},
			},
			wantErr: testDuplicateId,
		},
		{
			name: "caller missing amount",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{
					{
						ID:          testPassive1,
						AssetRef:    assetRef,
						VirtualPSBT: []byte{1},
					},
				},
			},
			wantErr: "amount is required",
		},
		{
			name: "caller missing virtual psbt",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{
					{
						ID:       testPassive1,
						AssetRef: assetRef,
						Amount:   1,
					},
				},
			},
			wantErr: "virtual PSBT is required",
		},
		{
			name: "caller missing proof file",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{
					{
						ID:          testPassive1,
						AssetRef:    assetRef,
						Amount:      1,
						VirtualPSBT: []byte{1},
					},
				},
			},
			wantErr: "proof file is required",
		},
		{
			name: "caller amount overflow",
			plan: CustomAnchorPassiveAssets{
				Policy: CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{
					packet,
					{
						ID:          "passive-2",
						AssetRef:    assetRef,
						Amount:      math.MaxUint64,
						VirtualPSBT: []byte{2},
						ProofFile:   []byte{3},
					},
				},
			},
			wantErr: "amount overflows uint64",
		},
		{
			name: "caller reanchor",
			plan: CustomAnchorPassiveAssets{
				Policy:  CustomAnchorPassiveCallerReanchor,
				Packets: []CustomAnchorPassivePacket{packet},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.plan.Validate()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorVerificationResult(t *testing.T) {
	valid := validCustomAnchorVerificationResult()
	require.NoError(t, valid.Validate())
	require.True(t, valid.Valid())

	tests := []struct {
		name    string
		mutate  func(*CustomAnchorVerificationResult)
		wantErr string
	}{
		{
			name: "missing checks",
			mutate: func(result *CustomAnchorVerificationResult) {
				result.Checks = nil
			},
			wantErr: "at least one verification check is required",
		},
		{
			name: "missing check code",
			mutate: func(result *CustomAnchorVerificationResult) {
				result.Checks[0].Code = ""
			},
			wantErr: "check 0 code is required",
		},
		{
			name: "unknown check scope",
			mutate: func(result *CustomAnchorVerificationResult) {
				result.Checks[0].Scope = unknownString
			},
			wantErr: "unknown scope",
		},
		{
			name: "unknown check origin",
			mutate: func(result *CustomAnchorVerificationResult) {
				result.Checks[0].Origin =
					CustomAnchorVerificationOriginUnknown
			},
			wantErr: "unknown origin",
		},
		{
			name: "missing issue code",
			mutate: func(result *CustomAnchorVerificationResult) {
				result.Issues[0].Code = ""
			},
			wantErr: "issue 0 code is required",
		},
		{
			name: "unknown issue severity",
			mutate: func(result *CustomAnchorVerificationResult) {
				result.Issues[0].Severity =
					CustomAnchorVerificationSeverityUnknown
			},
			wantErr: "unknown severity",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := valid.Clone()
			test.mutate(result)

			err := result.Validate()
			require.ErrorContains(t, err, test.wantErr)
			require.False(t, result.Valid())
		})
	}

	failedCheck := valid.Clone()
	failedCheck.Checks[0].Passed = false
	require.False(t, failedCheck.Valid())

	errorIssue := valid.Clone()
	errorIssue.Issues[0].Severity =
		CustomAnchorVerificationSeverityError
	require.False(t, errorIssue.Valid())

	var nilResult *CustomAnchorVerificationResult
	require.ErrorContains(
		t, nilResult.Validate(), "nil custom anchor verification result",
	)
	require.False(t, nilResult.Valid())
}

func TestCustomAnchorRequestCloneNoAlias(t *testing.T) {
	request := richCustomAnchorRequest(t)
	require.NoError(t, request.Validate())

	clone := request.Clone()
	require.Equal(t, request, clone)

	request.AnchorPSBT[0] = 90
	request.Inputs[0].ProofFile[0] = 91
	request.Inputs[0].Witness.Stack[0][0] = 92
	request.Outputs[0].Script.Wallet.KeyLocator.Index = 93
	request.Outputs[0].Anchor.Tapscript.TapLeaves[0].Script[0] = 94
	request.Outputs[0].ProofDelivery.OpaqueMetadata[0] = 95
	request.Outputs[1].Script.External.ScriptKey.TapTweak[0] = 96
	request.Outputs[1].Anchor.Tapscript.SerializedSibling[0] = 97
	request.Outputs[2].Script.OPTrue.InternalKey.KeyLocator.Index = 98
	request.Outputs[3].Script.Burn = nil
	request.Funding.WalletFunded.CustomLockID[0] = 99
	request.PassiveAssets.Packets[0].VirtualPSBT[0] = 100
	request.PassiveAssets.Packets[0].ProofFile[0] = 101
	request.LossPolicy.Allowances[0].MaxAmount = 102
	request.SigningPlans[0].MuSig2.SessionContext[0] = 103
	request.SigningPlans[0].MuSig2.Participants[0][0] = 104

	require.Equal(t, []byte{1, 2}, clone.AnchorPSBT)
	require.Equal(t, []byte{3, 4}, clone.Inputs[0].ProofFile)
	require.Equal(t, []byte{5, 6}, clone.Inputs[0].Witness.Stack[0])
	require.Equal(
		t, uint32(2),
		clone.Outputs[0].Script.Wallet.KeyLocator.Index,
	)
	require.Equal(
		t, []byte{7, 8},
		clone.Outputs[0].Anchor.Tapscript.TapLeaves[0].Script,
	)
	require.Equal(
		t, []byte{9, 10},
		clone.Outputs[0].ProofDelivery.OpaqueMetadata,
	)
	require.Equal(
		t, []byte{11, 12},
		clone.Outputs[1].Script.External.ScriptKey.TapTweak,
	)
	require.Equal(
		t, []byte{13, 14},
		clone.Outputs[1].Anchor.Tapscript.SerializedSibling,
	)
	require.Equal(
		t, uint32(3),
		clone.Outputs[2].Script.OPTrue.InternalKey.KeyLocator.Index,
	)
	require.NotNil(t, clone.Outputs[3].Script.Burn)
	require.Equal(t, bytes.Repeat([]byte{15, 16}, 16),
		clone.Funding.WalletFunded.CustomLockID)
	require.Equal(
		t, []byte{17, 18},
		clone.PassiveAssets.Packets[0].VirtualPSBT,
	)
	require.Equal(
		t, []byte{19, 20}, clone.PassiveAssets.Packets[0].ProofFile,
	)
	require.Equal(t, uint64(100), clone.LossPolicy.Allowances[0].MaxAmount)
	require.Equal(
		t, []byte{21, 22},
		clone.SigningPlans[0].MuSig2.SessionContext,
	)
	require.Equal(
		t, customAnchorTypesTestPubKey(t),
		clone.SigningPlans[0].MuSig2.Participants[0],
	)
}

func TestCustomAnchorRequestCloneNil(t *testing.T) {
	var request *CustomAnchorRequest
	require.Nil(t, request.Clone())
}

func TestCustomAnchorVerificationResultCloneNoAlias(t *testing.T) {
	result := validCustomAnchorVerificationResult()
	clone := result.Clone()

	*result.Checks[0].InputIndex = 9
	*result.Issues[0].OutputIndex = 10
	result.Checks[0].Message = "changed"
	result.Issues[0].Message = "changed"

	require.Equal(t, uint32(1), *clone.Checks[0].InputIndex)
	require.Equal(t, uint32(2), *clone.Issues[0].OutputIndex)
	require.Equal(t, "locally verified", clone.Checks[0].Message)
	require.Equal(t, "backend warning", clone.Issues[0].Message)

	var nilResult *CustomAnchorVerificationResult
	require.Nil(t, nilResult.Clone())
}

func validCustomAnchorRequest(t *testing.T) *CustomAnchorRequest {
	t.Helper()

	assetRef := AssetRefFromAssetID(AssetID{1})
	key := customAnchorTypesTestPubKey(t)

	return &CustomAnchorRequest{
		Inputs: []CustomAssetInput{
			{
				ID:        "input-1",
				AssetRef:  assetRef,
				Amount:    100,
				ProofFile: []byte{1},
				Witness: CustomAssetWitnessPlan{
					Mode: CustomAssetWitnessBackendSigner,
				},
			},
		},
		Outputs: []CustomAssetOutput{
			{
				ID:                "output-1",
				AssetRef:          assetRef,
				Amount:            100,
				AnchorOutputIndex: 1,
				AnchorValueSat:    330,
				Script: CustomAssetScriptPlan{
					Mode: CustomAssetScriptExternal,
					External: &CustomAssetExternalScriptPlan{
						ScriptKey: ScriptKey{
							PubKey: key,
						},
					},
				},
				Anchor: CustomAnchorOutputPlan{
					InternalKey: InternalKey{
						PubKey: key,
					},
					Tapscript: CustomAnchorTapscriptPlan{
						SerializedSibling: []byte{2},
					},
				},
			},
		},
		AnchorPSBT: []byte{3},
		Funding: CustomAnchorFundingPlan{
			Mode:              CustomAnchorFundingCallerFundedExact,
			CallerFundedExact: &CustomAnchorCallerFundedExact{},
		},
		PassiveAssets: CustomAnchorPassiveAssets{
			Policy: CustomAnchorPassiveReject,
		},
		LossPolicy: CustomAnchorLossPolicy{
			Mode: CustomAnchorLossReject,
		},
	}
}

func richCustomAnchorRequest(t *testing.T) *CustomAnchorRequest {
	t.Helper()

	request := validCustomAnchorRequest(t)
	assetRef := request.Inputs[0].AssetRef
	key := customAnchorTypesTestPubKey(t)
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)

	request.AnchorPSBT = []byte{1, 2}
	request.Inputs[0].Amount = 400
	request.Inputs[0].ProofFile = []byte{3, 4}
	request.Inputs[0].Witness = CustomAssetWitnessPlan{
		Mode:  CustomAssetWitnessCallerProvided,
		Stack: [][]byte{{5, 6}, {1}},
	}
	request.Outputs = []CustomAssetOutput{
		{
			ID:                "wallet-output",
			AssetRef:          assetRef,
			Amount:            100,
			AnchorOutputIndex: 1,
			AnchorValueSat:    330,
			Script: CustomAssetScriptPlan{
				Mode: CustomAssetScriptWallet,
				Wallet: &CustomAssetWalletScriptPlan{
					KeyLocator: &KeyLocator{
						Family: 1,
						Index:  2,
					},
				},
			},
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: key,
				},
				Tapscript: CustomAnchorTapscriptPlan{
					TapLeaves: []TapLeaf{{Script: []byte{7, 8}}},
				},
			},
			ProofDelivery: CustomAssetProofDelivery{
				RecipientID:    "recipient-1",
				CourierAddress: "hashmail://courier",
				OpaqueMetadata: []byte{9, 10},
			},
		},
		{
			ID:                "external-output",
			AssetRef:          assetRef,
			Amount:            100,
			AnchorOutputIndex: 2,
			AnchorValueSat:    330,
			Script: CustomAssetScriptPlan{
				Mode: CustomAssetScriptExternal,
				External: &CustomAssetExternalScriptPlan{
					ScriptKey: ScriptKey{
						PubKey:   key,
						TapTweak: []byte{11, 12},
					},
				},
			},
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: key,
				},
				Tapscript: CustomAnchorTapscriptPlan{
					SerializedSibling: []byte{13, 14},
				},
			},
		},
		{
			ID:                "op-true-output",
			AssetRef:          assetRef,
			Amount:            100,
			AnchorOutputIndex: 3,
			AnchorValueSat:    330,
			Script: CustomAssetScriptPlan{
				Mode: CustomAssetScriptOPTrue,
				OPTrue: &CustomAssetOPTrueScriptPlan{
					InternalKey: KeyDescriptor{
						RawKeyBytes: key,
						KeyLocator: KeyLocator{
							Family: 2,
							Index:  3,
						},
					},
				},
			},
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: key,
				},
			},
		},
		{
			ID:                "burn-output",
			AssetRef:          assetRef,
			Amount:            100,
			AnchorOutputIndex: 4,
			AnchorValueSat:    330,
			Script:            customAnchorBurnScript(),
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: key,
				},
			},
		},
	}
	request.Funding = CustomAnchorFundingPlan{
		Mode: CustomAnchorFundingWalletFunded,
		WalletFunded: &CustomAnchorWalletFunding{
			ChangeOutput: AnchorChangeOutput{
				Mode: AnchorChangeOutputNoNew,
			},
			Fee: AnchorFee{
				Mode:    AnchorFeeSatPerVByte,
				FeeRate: feeRate,
			},
			MaxFeeSat:             10_000,
			CustomLockID:          bytes.Repeat([]byte{15, 16}, 16),
			LockExpirationSeconds: 600,
		},
	}
	request.PassiveAssets = CustomAnchorPassiveAssets{
		Policy: CustomAnchorPassiveCallerReanchor,
		Packets: []CustomAnchorPassivePacket{
			{
				ID:          testPassive1,
				AssetRef:    AssetRefFromAssetID(AssetID{2}),
				Amount:      1,
				VirtualPSBT: []byte{17, 18},
				ProofFile:   []byte{19, 20},
			},
		},
	}
	request.LossPolicy = customAnchorLossPolicy(assetRef, 100)
	request.SigningPlans = []CustomAnchorInputSigningPlan{
		{
			InputIndex: 0,
			MuSig2: &CustomAnchorMuSig2SigningPlan{
				Participants: []PubKey{
					key, customAnchorTypesTestSecondPubKey(t),
				},
				SessionContext: []byte{21, 22},
			},
		},
	}

	return request
}

func customAnchorBurnScript() CustomAssetScriptPlan {
	return CustomAssetScriptPlan{
		Mode: CustomAssetScriptBurn,
		Burn: &CustomAssetBurnScriptPlan{},
	}
}

func customAnchorLossPolicy(ref AssetRef,
	maxAmount uint64) CustomAnchorLossPolicy {

	return CustomAnchorLossPolicy{
		Mode: CustomAnchorLossBurn,
		Allowances: []CustomAnchorLossAllowance{
			{
				AssetRef:  ref,
				MaxAmount: maxAmount,
			},
		},
		ConfirmIrreversibleLoss: true,
	}
}

func validCustomAnchorVerificationResult() *CustomAnchorVerificationResult {
	inputIndex := uint32(1)
	outputIndex := uint32(2)

	return &CustomAnchorVerificationResult{
		Checks: []CustomAnchorVerificationCheck{
			{
				Code:       "proof_valid",
				Scope:      CustomAnchorVerificationScopeInputProof,
				Origin:     CustomAnchorVerificationOriginLocal,
				Passed:     true,
				InputIndex: &inputIndex,
				Message:    "locally verified",
			},
		},
		Issues: []CustomAnchorVerificationIssue{
			{
				Code:        "backend_trusted",
				Scope:       CustomAnchorVerificationScopeBackendTrust,
				Origin:      CustomAnchorVerificationOriginBackend,
				Severity:    CustomAnchorVerificationSeverityWarning,
				OutputIndex: &outputIndex,
				Message:     "backend warning",
			},
		},
	}
}

func customAnchorTypesTestPubKey(t *testing.T) PubKey {
	t.Helper()

	key, err := ParsePubKeyHex(
		"0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959" +
			"f2815b16f81798",
	)
	require.NoError(t, err)

	return key
}

func customAnchorTypesTestOddPubKey(t *testing.T) PubKey {
	t.Helper()

	key, err := ParsePubKeyHex(
		"0379be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959" +
			"f2815b16f81798",
	)
	require.NoError(t, err)

	return key
}

func customAnchorTypesTestSecondPubKey(t *testing.T) PubKey {
	t.Helper()

	key, err := ParsePubKeyHex(
		"02c6047f9441ed7d6d3045406e95c07cd8" +
			"5c778e4b8cef3ca7abac09b95c709ee5",
	)
	require.NoError(t, err)

	return key
}
