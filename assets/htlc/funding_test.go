package htlc

import (
	"bytes"
	"fmt"
	"net/url"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
	"github.com/stretchr/testify/require"
)

// TestHtlcFundingModes funds untouched default, address and interactive
// templates with exact-value and change inputs, for grouped and ungrouped
// assets. Default and address templates always derive the advertised key.
func TestHtlcFundingModes(t *testing.T) {
	t.Run("CSV", func(t *testing.T) {
		testHtlcFundingModes(t, AssetHTLCV0)
	})
	t.Run("CLTV", func(t *testing.T) {
		testHtlcFundingModes(t, AssetHTLCCLTVV1)
	})
}

func testHtlcFundingModes(t *testing.T, contractPolicy Policy) {
	t.Helper()

	for _, mode := range []struct {
		name        string
		interactive bool
		fromAddress bool
	}{
		{name: "default"},
		{name: "address", fromAddress: true},
		{name: "interactive", interactive: true},
	} {
		for _, grouped := range []bool{false, true} {
			for _, sourceAmount := range []uint64{1000, 1500} {
				name := fmt.Sprintf("%s/grouped=%t/input=%d",
					mode.name, grouped, sourceAmount)
				t.Run(name, func(t *testing.T) {
					f := newWitnessFixtureForPolicy(
						t, SuccessSequence, asset.V1,
						commitment.TapCommitmentV2, false, contractPolicy,
					)
					internalKey, err := f.kit.GetAggregateKey()
					require.NoError(t, err)
					sibling, err := f.kit.GetSiblingPreimage()
					require.NoError(t, err)
					scriptKey, _, _, _, err := CreateOpTrueLeaf()
					require.NoError(t, err)
					source, err := asset.New(
						f.proof.Asset.Genesis, sourceAmount, 0, 0,
						asset.NewScriptKey(scriptKey.PubKey), nil,
						asset.WithAssetVersion(asset.V1),
					)
					require.NoError(t, err)
					addr := &address.Tap{
						Version:          address.V1,
						AssetVersion:     asset.V1,
						ChainParams:      &address.RegressionNetTap,
						AssetID:          f.kit.AssetID(),
						Amount:           f.kit.Amount(),
						ScriptKey:        *scriptKey.PubKey,
						InternalKey:      *internalKey,
						TapscriptSibling: &sibling,
						ProofCourierAddr: url.URL{
							Scheme: "universerpc", Host: "localhost:10029",
						},
					}
					if grouped {
						signer, _ := scalarKey(t, 0x96)
						group, _ := asset.RandGroupKeyWithSigner(
							t, signer, source.Genesis, source,
						)
						source.GroupKey = group
						source.PrevWitnesses[0].TxWitness = group.Witness
						addr.GroupKey = &group.GroupPubKey
					}
					encoded, err := addr.EncodeAddress()
					require.NoError(t, err)
					decoded, err := address.DecodeAddress(
						encoded, &address.RegressionNetTap,
					)
					require.NoError(t, err)
					decoded.AttachGenesis(source.Genesis)
					addressKey, err := decoded.TaprootOutputKey()
					require.NoError(t, err)

					var template *tappsbt.VPacket
					switch {
					case mode.interactive:
						template, err = f.kit.CreateInteractiveHtlcVpkt()
					case mode.fromAddress:
						template, err = tappsbt.FromAddresses(
							[]*address.Tap{decoded}, 1,
						)
					default:
						template, err = f.kit.CreateHtlcVpkt()
					}
					require.NoError(t, err)
					var before bytes.Buffer
					require.NoError(t, template.Serialize(&before))
					packet, htlcProof := fundHtlcTemplate(t, template, source)
					var after bytes.Buffer
					require.NoError(t, template.Serialize(&after))
					require.Equal(t, before.Bytes(), after.Bytes())

					// Exact non-interactive funding retains the NUMS
					// tombstone; only the explicit interactive path removes it.
					change := sourceAmount - f.kit.Amount()
					if mode.interactive && change == 0 {
						require.Len(t, packet.Outputs, 1)
						require.False(t, packet.HasSplitRootOutput())
						require.True(t, htlcProof.Asset.IsTransferRoot())
						require.NotEmpty(t, htlcProof.AltLeaves)
					} else {
						require.Len(t, packet.Outputs, 2)
						root, err := packet.SplitRootOutput()
						require.NoError(t, err)
						require.Equal(t, change, root.Amount)
						if change == 0 {
							unspendable, err := root.ScriptKey.IsUnSpendable()
							require.NoError(t, err)
							require.True(t, unspendable)
						}
					}
					require.Equal(t, uint32(1), htlcProof.InclusionProof.OutputIndex)
					require.Equal(t, f.kit.Amount(), htlcProof.Asset.Amount)
					outputKey, err := proof.ExtractTaprootKey(
						&htlcProof.AnchorTx, htlcProof.InclusionProof.OutputIndex,
					)
					require.NoError(t, err)
					if mode.interactive && change == 0 {
						require.NotEqual(t, schnorr.SerializePubKey(addressKey),
							schnorr.SerializePubKey(outputKey))
					} else {
						require.Equal(t, schnorr.SerializePubKey(addressKey),
							schnorr.SerializePubKey(outputKey))
					}
					pkScript, err := f.kit.GetPkScriptFromProof(htlcProof)
					require.NoError(t, err)
					require.Equal(t, htlcProof.AnchorTx.TxOut[1].PkScript, pkScript)
				})
			}
		}
	}
}

// fundHtlcTemplate runs tapd's allocation, coin distribution and output
// preparation stages without changing the supplied funding template. It
// supplies wallet change keys and completes unused Bitcoin output slots.
func fundHtlcTemplate(t *testing.T, template *tappsbt.VPacket,
	source *asset.Asset) (*tappsbt.VPacket, *proof.Proof) {

	t.Helper()

	allocations, interactive, err := tapsend.AllocationsFromTemplate(
		template, source.Amount,
	)
	require.NoError(t, err)
	packets, err := tapsend.DistributeCoins(
		[]*proof.Proof{sourceProof(t, source)}, allocations,
		&address.RegressionNetTap, interactive, template.Version,
	)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	packet := packets[0]
	_, changeKey := scalarKey(t, 0x93)
	_, changeAnchorKey := scalarKey(t, 0x94)
	for _, output := range packet.Outputs {
		if !output.Type.IsSplitRoot() {
			continue
		}
		if output.Amount > 0 {
			output.ScriptKey = asset.NewScriptKey(changeKey)
		}
		output.AnchorOutputInternalKey = changeAnchorKey
	}
	require.NoError(t, tapsend.PrepareOutputAssets(t.Context(), packet))
	for _, output := range packet.Outputs {
		require.NoError(t, output.Asset.UpdateTxWitness(0, opTrueWitness(t)))
		if output.SplitAsset != nil {
			require.NoError(t, output.SplitAsset.UpdateTxWitness(
				0, opTrueWitness(t),
			))
		}
	}

	prevAssets := make(commitment.InputSet, len(packet.Inputs))
	for _, input := range packet.Inputs {
		prevAssets[input.PrevID] = input.Asset()
	}
	rootAsset := packet.Outputs[0].Asset
	var splits []*commitment.SplitAsset
	if packet.HasSplitRootOutput() {
		root, err := packet.SplitRootOutput()
		require.NoError(t, err)
		rootAsset = root.Asset
		for _, output := range packet.Outputs {
			splitAsset := output.Asset
			if output.Type.IsSplitRoot() {
				splitAsset = output.SplitAsset
			}
			require.NotNil(t, splitAsset)
			splits = append(splits, &commitment.SplitAsset{
				Asset: *splitAsset, OutputIndex: output.AnchorOutputIndex,
			})
		}
	}
	require.NoError(t, vm.ValidateWitnesses(rootAsset, splits, prevAssets))
	commitments, err := tapsend.CreateOutputCommitments(
		packets, tapsend.WithSpenderLeaves(),
	)
	require.NoError(t, err)
	anchor, err := tapsend.PrepareAnchoringTemplate(packets)
	require.NoError(t, err)
	require.NoError(t, tapsend.UpdateTaprootOutputKeys(anchor, packet, commitments))

	// Exact interactive funding removes the asset allocation at index zero.
	// A Bitcoin wallet funds that unused slot as an ordinary BIP86 output.
	_, walletKey := scalarKey(t, 0x95)
	walletScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(walletKey),
	)
	require.NoError(t, err)
	for idx, output := range anchor.UnsignedTx.TxOut {
		if bytes.Equal(output.PkScript, tapsend.GenesisDummyScript) {
			output.PkScript = bytes.Clone(walletScript)
			anchor.Outputs[idx].TaprootInternalKey =
				schnorr.SerializePubKey(walletKey)
		}
	}
	for idx, output := range packet.Outputs {
		if output.Type.IsSplitRoot() {
			continue
		}
		htlcProof, err := tapsend.CreateProofSuffix(
			anchor.UnsignedTx, anchor.Outputs, packet, commitments,
			idx, packets,
		)
		require.NoError(t, err)
		_, err = htlcProof.VerifyProofs()
		require.NoError(t, err)
		return packet, htlcProof
	}
	require.FailNow(t, "funding transfer has no HTLC output")
	return nil, nil
}
