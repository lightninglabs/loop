//go:build itest

package itest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

const (
	defaultMineBlocks                = 6
	defaultSyncTimeout               = 30 * time.Second
	defaultGroupBalanceTimeout       = 3 * time.Minute
	defaultCollectibleBalanceTimeout = time.Minute
	customAnchorITestVersion         = int32(3)
	customAnchorITestSequence        = uint32(0xffff_fffc)
)

func customAnchorFinalizeKeyPath(t testing.TB, rawPsbt []byte) []byte {
	t.Helper()
	packet := decodeCustomAnchorITestPSBT(t, rawPsbt)
	ok, err := psbt.MaybeFinalize(packet, 0)
	require.NoError(t, err)
	require.True(t, ok)

	var buffer bytes.Buffer
	require.NoError(t, packet.Serialize(&buffer))

	return buffer.Bytes()
}

func customAnchorMuSig2Sign(t testing.TB, signers []*btcec.PrivateKey,
	participants []*btcec.PublicKey, sighash tapsdk.Hash,
	scriptRoot []byte) []byte {

	t.Helper()
	var message [32]byte
	copy(message[:], sighash[:])

	nonces := make([]*musig2.Nonces, len(signers))
	pubNonces := make([][musig2.PubNonceSize]byte, len(signers))
	for i, signer := range signers {
		nonce, err := musig2.GenNonces(
			musig2.WithPublicKey(signer.PubKey()),
		)
		require.NoError(t, err)
		nonces[i] = nonce
		pubNonces[i] = nonce.PubNonce
	}

	combinedNonce, err := musig2.AggregateNonces(pubNonces)
	require.NoError(t, err)

	partials := make([]*musig2.PartialSignature, len(signers))
	for i, signer := range signers {
		partial, err := musig2.Sign(
			nonces[i].SecNonce, signer, combinedNonce, participants,
			message, musig2.WithTaprootSignTweak(scriptRoot),
		)
		require.NoError(t, err)
		partials[i] = partial
	}

	finalSig := musig2.CombineSigs(
		partials[0].R, partials,
		musig2.WithTaprootTweakedCombine(
			message, participants, scriptRoot, false,
		),
	)

	return finalSig.Serialize()
}

func decodeCustomAnchorITestPSBT(t testing.TB,
	rawPsbt []byte) *psbt.Packet {

	t.Helper()
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(rawPsbt), false)
	require.NoError(t, err)

	return packet
}

func mustPayToTaproot(t testing.TB, key *btcec.PublicKey) []byte {
	t.Helper()
	script, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)

	return script
}

func mustCompressedPubKey(t testing.TB, key *btcec.PublicKey) tapsdk.PubKey {
	t.Helper()
	parsed, err := tapsdk.ParsePubKey(key.SerializeCompressed())
	require.NoError(t, err)

	return parsed
}

func mustXOnlyPubKey(t testing.TB, key *btcec.PublicKey) tapsdk.XOnlyPubKey {
	t.Helper()
	parsed, err := tapsdk.ParseXOnlyPubKey(schnorr.SerializePubKey(key))
	require.NoError(t, err)

	return parsed
}

func uniqueEventLabel(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func extractDockerFile(t testing.TB, container,
	containerPath string) string {

	t.Helper()

	tmpDir := t.TempDir()
	localPath := tmpDir + "/" + containerPath[strings.LastIndex(
		containerPath, "/")+1:]

	var lastErr error
	for attempt := range 5 {
		if attempt > 0 {
			t.Logf("Retry %d: docker cp %s:%s",
				attempt, container, containerPath)
			time.Sleep(3 * time.Second)
		}

		cmd := exec.Command("docker", "cp",
			container+":"+containerPath, localPath)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return localPath
		}

		lastErr = fmt.Errorf("docker cp %s:%s failed: %s",
			container, containerPath, string(out))
	}

	require.NoError(t, lastErr,
		"docker cp %s:%s failed after retries",
		container, containerPath)

	return localPath
}

func balanceTimeoutFor(ref tapsdk.AssetRef) time.Duration {
	if ref.IsGroupRef() {
		return defaultGroupBalanceTimeout
	}

	return defaultCollectibleBalanceTimeout
}
