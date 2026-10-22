package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestHtlcAddressFunding verifies the address returned by NewHtlcAddr predicts
// its untouched funding template's output for grouped and ungrouped assets,
// with both exact-value and change inputs.
func TestHtlcAddressFunding(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		name := "ungrouped"
		if grouped {
			name = testGroupedAsset
		}
		t.Run(name, func(t *testing.T) {
			for _, change := range []uint64{0, 500} {
				name := "exact"
				if change > 0 {
					name = "change"
				}
				t.Run(name, func(t *testing.T) {
					f := newWitnessFixture(t)
					f.proof.Asset.Amount += change
					var groupKey *btcec.PublicKey
					if grouped {
						signer, _ := scalarKey(t, 42)
						group, _ := asset.RandGroupKeyWithSigner(
							t, signer, f.proof.Asset.Genesis,
							&f.proof.Asset,
						)
						f.proof.Asset.GroupKey = group
						f.proof.Asset.PrevWitnesses[0].TxWitness =
							group.Witness
						groupKey = &group.GroupPubKey
					}
					genesis := f.proof.Asset.Genesis
					f.kit = newAddressKit(t, &genesis, groupKey)
					reanchorDepositProof(t, f.kit, f.proof)
					client := &addressClient{
						genesis: genesis, groupKey: groupKey,
					}
					hash := lntypes.Hash{1}
					rpcAddr, kit, err := f.kit.NewHtlcAddr(
						t.Context(), client, 1000, hash, 144,
					)
					require.NoError(t, err)
					decoded, err := f.kit.ValidateHtlcAddr(
						rpcAddr.Encoded, 1000, hash, 144,
					)
					require.NoError(t, err)
					addressKey, err := decoded.TaprootOutputKey()
					require.NoError(t, err)
					require.Equal(t, rpcAddr.TaprootOutputKey,
						schnorr.SerializePubKey(addressKey))
					template, err := kit.CreateHtlcVpkt()
					require.NoError(t, err)
					allocations, interactive, err := tapsend.AllocationsFromTemplate(
						template, f.proof.Asset.Amount,
					)
					require.NoError(t, err)
					require.False(t, interactive)
					packets, err := tapsend.DistributeCoins(
						[]*proof.Proof{f.proof}, allocations,
						&address.RegressionNetTap, interactive,
						template.Version,
					)
					require.NoError(t, err)
					require.Len(t, packets, 1)
					packet := packets[0]
					root, err := packet.SplitRootOutput()
					require.NoError(t, err)
					require.Equal(t, change, root.Amount)
					_, changeKey := scalarKey(t, 43)
					_, anchorKey := scalarKey(t, 44)
					if change > 0 {
						root.ScriptKey = asset.NewScriptKey(changeKey)
					}
					root.AnchorOutputInternalKey = anchorKey
					err = tapsend.PrepareOutputAssets(t.Context(), packet)
					require.NoError(t, err)
					var splits []*commitment.SplitAsset
					for _, output := range packet.Outputs {
						err = output.Asset.UpdateTxWitness(
							0, opTrueWitness(t),
						)
						require.NoError(t, err)
						splitAsset := output.Asset
						if output.Type.IsSplitRoot() {
							splitAsset = output.SplitAsset
							err = splitAsset.UpdateTxWitness(
								0, opTrueWitness(t),
							)
							require.NoError(t, err)
						}
						splits = append(splits, &commitment.SplitAsset{
							Asset:       *splitAsset,
							OutputIndex: output.AnchorOutputIndex,
						})
					}
					previous := commitment.InputSet{
						packet.Inputs[0].PrevID: packet.Inputs[0].Asset(),
					}
					err = vm.ValidateWitnesses(root.Asset, splits, previous)
					require.NoError(t, err)
					commitments, err := tapsend.CreateOutputCommitments(
						packets, tapsend.WithSpenderLeaves(),
					)
					require.NoError(t, err)
					anchor, err := tapsend.PrepareAnchoringTemplate(packets)
					require.NoError(t, err)
					err = tapsend.UpdateTaprootOutputKeys(
						anchor, packet, commitments,
					)
					require.NoError(t, err)
					suffix, err := tapsend.CreateProofSuffix(
						anchor.UnsignedTx, anchor.Outputs, packet,
						commitments, 1, packets,
					)
					require.NoError(t, err)
					_, err = suffix.VerifyProofs()
					require.NoError(t, err)
					expectedScript, err := txscript.PayToTaprootScript(addressKey)
					require.NoError(t, err)
					actualScript, err := kit.GetPkScriptFromProof(suffix)
					require.NoError(t, err)
					require.Equal(t, expectedScript, actualScript)
					index := suffix.InclusionProof.OutputIndex
					require.Equal(t, expectedScript,
						suffix.AnchorTx.TxOut[index].PkScript)
				})
			}
		})
	}
}
