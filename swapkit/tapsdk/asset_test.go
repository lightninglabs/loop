package tapsdk

import (
	"encoding/hex"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// These vectors freeze the pre-SDK NUMS/single-OP_TRUE contract. A dependency
// upgrade or a fresh wallet locator must not change existing asset ownership.
func TestLegacyOPTruePolicy(t *testing.T) {
	key, spend, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	require.Equal(
		t,
		"027bbbb85ff774141ee36c2cc9f38b40be4a02c2a156c8a1def8781cf39177deca",
		hex.EncodeToString(key.PubKey[:]),
	)
	require.Equal(
		t,
		"a85b2107f791b26a84e7586c28cec7cb61202ed3d01944d832500f363782d675",
		hex.EncodeToString(key.TapTweak),
	)
	require.Equal(
		t,
		"c17c79b9b26e463895eef5679d8558942c86c4ad2233adef01bc3e6d540b3653fe",
		hex.EncodeToString(spend.ControlBlock),
	)
	require.Zero(t, key.KeyDesc.KeyLocator)
	require.NoError(t, spend.Validate(key.PubKey))
	external := sdk.CustomAnchorAssetOutputSummary{
		ScriptMode: sdk.CustomAssetScriptExternal,
		ScriptKey:  key.PubKey,
	}
	restored, err := OPTrueSpendInfo(external)
	require.NoError(t, err)
	require.Equal(t, spend, restored)
	// The policy changes require explicit selection; nil ordinary wallet
	// descriptors cannot be promoted into public asset witnesses.
	external.ScriptKey = testKey(t, 12)
	_, err = OPTrueSpendInfo(external)
	require.Error(t, err)
	external.ScriptKey = key.PubKey
	external.ScriptMode = sdk.CustomAssetScriptWallet
	_, err = OPTrueSpendInfo(external)
	require.Error(t, err)
	external.ScriptMode = sdk.CustomAssetScriptExternal
	external.OPTrueSpend = spend.Clone()
	external.OPTrueSpend.ControlBlock[0] ^= 1
	_, err = OPTrueSpendInfo(external)
	require.Error(t, err)
	// Returned descriptors never share mutable tweak or witness storage.
	key.TapTweak[0] ^= 1
	spend.ControlBlock[0] ^= 1
	fresh, freshSpend, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	require.NotEqual(t, key.TapTweak, fresh.TapTweak)
	require.NotEqual(t, spend.ControlBlock, freshSpend.ControlBlock)
}

func TestNativeLegacyAndExplicitSDKPolicies(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "legacy default"
		if explicit {
			name = "explicit SDK tree"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newCustomAnchorBuilderFixture(t)
			kit, err := swapkit.NewSwapKit(swapKitTestParams(t))
			require.NoError(t, err)
			if explicit {
				fixture.request.Outputs[0] = SDKOPTrueOutput(
					fixture.request.Outputs[0],
					sdk.KeyDescriptor{
						RawKeyBytes: testKey(t, 13),
					},
					kit.AnchorPlan(),
				)
			} else {
				fixture.request.Outputs[0], err = LegacyOPTrueOutput(fixture.request.Outputs[0], kit.AnchorPlan())
				require.NoError(t, err)
			}
			wallet := sdk.NewWallet(
				fixture.client,
				sdk.NetworkRegtest,
			)
			plan, err := wallet.NewCustomAnchorTxBuilder().
				Build(t.Context(), fixture.request)
			require.NoError(t, err)
			out := plan.Outputs()[0]
			summary := sdk.CustomAnchorAssetOutputSummary{
				ScriptKey:   out.ScriptKey,
				ScriptMode:  out.ScriptMode,
				OPTrueSpend: out.OPTrueSpend,
			}
			spend, err := OPTrueSpendInfo(summary)
			require.NoError(t, err)
			require.NoError(t, spend.Validate(out.ScriptKey))
			key, _, err := LegacyOPTrueDescriptor()
			require.NoError(t, err)
			if explicit {
				require.Equal(
					t,
					sdk.CustomAssetScriptOPTrue,
					out.ScriptMode,
				)
				require.NotEqual(t, key.PubKey, out.ScriptKey)
				require.NotNil(t, out.OPTrueSpend)
			} else {
				require.Equal(t, sdk.CustomAssetScriptExternal, out.ScriptMode)
				require.Equal(t, key.PubKey, out.ScriptKey)
				require.Nil(t, out.OPTrueSpend)
			}
		})
	}
}
