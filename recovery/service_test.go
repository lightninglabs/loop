package recovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/address"
	staticaddrscript "github.com/lightninglabs/loop/staticaddr/script"
	staticaddrversion "github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	testutils "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"gopkg.in/macaroon.v2"
)

// TestEncryptDecryptBackupPayload verifies that a recovery backup payload
// round-trips through the secretbox envelope and is not stored as plaintext.
func TestEncryptDecryptBackupPayload(t *testing.T) {
	t.Parallel()

	var key [32]byte
	copy(key[:], []byte("0123456789abcdefghijklmnopqrstuv"))

	plaintext := []byte("loop recovery backup payload")

	encrypted, err := encryptBackupPayload(key, plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, encrypted)

	decrypted, err := decryptBackupPayload(key, encrypted)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)
}

// TestBackupEncryptionUsesSignerDerivedKey verifies that backups are encrypted
// with the documented lnd-derived key and can only be restored with that same
// derived key.
func TestBackupEncryptionUsesSignerDerivedKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	restoreDir := t.TempDir()
	lnd := testutils.NewMockLnd()
	signer := &fixedKeySigner{
		key: testBackupKey(1),
	}
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(
		dir, "testnet", signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Len(t, signer.calls, 1)
	require.True(t, signer.calls[0].pubKey.IsEqual(lndclient.SharedKeyNUMS))
	require.Equal(t, backupKeyLocator, *signer.calls[0].locator)

	_, err = readBackupPayload(testBackupKey(2), backupFile)
	require.ErrorContains(t, err, "unable to decrypt backup file")

	payload, err := readBackupPayload(testBackupKey(1), backupFile)
	require.NoError(t, err)
	require.EqualValues(t, backupVersion, payload.Version)

	wrongKeySvc := NewService(
		restoreDir, "testnet", &fixedKeySigner{key: testBackupKey(2)},
		lnd.WalletKit,
		&mockStaticAddressManager{chainParams: lnd.ChainParams}, nil,
	)
	_, err = wrongKeySvc.Restore(context.Background(), backupFile)
	require.ErrorContains(t, err, "unable to decrypt backup file")

	rightKeySvc := NewService(
		restoreDir, "testnet", &fixedKeySigner{key: testBackupKey(1)},
		lnd.WalletKit,
		&mockStaticAddressManager{chainParams: lnd.ChainParams}, nil,
	)
	result, err := rightKeySvc.Restore(context.Background(), backupFile)
	require.NoError(t, err)
	require.True(t, result.RestoredL402)
	require.True(t, result.RestoredStaticAddress)
}

// TestWriteBackupReturnsEmptyWithoutState verifies that no backup is written
// before Loop has both paid L402 state and static-address state.
func TestWriteBackupReturnsEmptyWithoutState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
	)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, backupFile)
	require.Empty(t, listBackupFiles(t, dir))
}

// TestWriteBackupReturnsEmptyWithTokenOnly verifies that a paid L402 by itself
// does not define a complete static-address recovery generation.
func TestWriteBackupReturnsEmptyWithTokenOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
	)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, backupFile)
	require.Empty(t, listBackupFiles(t, dir))
}

// TestWriteBackupReturnsEmptyWithPendingToken verifies that pending L402 token
// material is not backed up as an immutable recovery generation.
func TestWriteBackupReturnsEmptyWithPendingToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)

	writePendingToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, backupFile)
	require.Empty(t, listBackupFiles(t, dir))
}

// TestWriteBackupIncludesStaticAddressAndPaidToken verifies that a complete
// generation backup contains the expected static-address parameters, exact paid
// L402 token bytes and private file permissions.
func TestWriteBackupIncludesStaticAddressAndPaidToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams:   lnd.ChainParams,
		params:        addrParams,
		currentHeight: 654,
	}

	tokenCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 123, time.UTC,
	)
	tokenID := writePaidToken(t, dir, 1, tokenCreatedAt)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Equal(
		t, backupFilePath(dir, tokenID, tokenCreatedAt.UnixNano()),
		backupFile,
	)

	key, err := svc.deriveEncryptionKey(context.Background())
	require.NoError(t, err)

	payload, err := readBackupPayload(key, backupFile)
	require.NoError(t, err)

	originalToken, err := os.ReadFile(filepath.Join(dir, paidTokenFileName))
	require.NoError(t, err)

	require.EqualValues(t, backupVersion, payload.Version)
	require.Equal(t, "testnet", payload.Network)
	require.Equal(t, tokenID, payload.L402TokenID)
	require.Equal(t, tokenCreatedAt.UnixNano(), payload.L402TokenCreatedAt)
	require.NotNil(t, payload.StaticAddress)
	require.EqualValues(
		t, addrParams.ProtocolVersion, payload.StaticAddress.ProtocolVersion,
	)
	require.Equal(
		t, addrParams.ClientPubkey.SerializeCompressed(),
		payload.StaticAddress.ClientPubKey,
	)
	require.Equal(
		t, addrParams.ServerPubkey.SerializeCompressed(),
		payload.StaticAddress.ServerPubKey,
	)
	require.Equal(t, addrParams.Expiry, payload.StaticAddress.Expiry)
	require.Equal(
		t, int32(addrParams.KeyLocator.Family),
		payload.StaticAddress.LegacyClientKeyFamily,
	)
	require.Equal(
		t, swap.StaticMultiAddressKeyFamily,
		payload.StaticAddress.MainKeyFamily,
	)
	require.Equal(
		t, swap.StaticAddressChangeKeyFamily,
		payload.StaticAddress.ChangeKeyFamily,
	)
	require.NotEqual(
		t, payload.StaticAddress.LegacyClientKeyFamily,
		payload.StaticAddress.MainKeyFamily,
	)
	require.NotEqual(
		t, payload.StaticAddress.LegacyClientKeyFamily,
		payload.StaticAddress.ChangeKeyFamily,
	)
	require.NotEqual(
		t, payload.StaticAddress.MainKeyFamily,
		payload.StaticAddress.ChangeKeyFamily,
	)
	require.Equal(
		t, addrParams.InitiationHeight,
		payload.StaticAddress.LegacyFirstHeight,
	)
	require.Equal(
		t, int32(654),
		payload.StaticAddress.MultiAddressFirstHeight,
	)
	require.Len(t, payload.TokenFiles, 1)
	require.Equal(t, paidTokenFileName, payload.TokenFiles[0].Name)
	require.Equal(t, originalToken, payload.TokenFiles[0].Data)

	info, err := os.Stat(backupFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// TestStaticAddressBackupReconstructsLegacyStaticAddress verifies that the
// backed-up legacy client key material reconstructs the original static address
// tapscript and taproot address.
func TestStaticAddressBackupReconstructsLegacyStaticAddress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 123, time.UTC),
	)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)

	backupFile, err := svc.WriteBackup(ctx)
	require.NoError(t, err)

	key, err := svc.deriveEncryptionKey(ctx)
	require.NoError(t, err)

	payload, err := readBackupPayload(key, backupFile)
	require.NoError(t, err)
	require.NotNil(t, payload.StaticAddress)

	serverPubKey, err := btcec.ParsePubKey(
		payload.StaticAddress.ServerPubKey,
	)
	require.NoError(t, err)

	clientPubKey, _, err := svc.resolveClientKey(ctx, payload.StaticAddress)
	require.NoError(t, err)

	reconstructed, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2,
		int64(payload.StaticAddress.Expiry), clientPubKey, serverPubKey,
	)
	require.NoError(t, err)

	pkScript, err := reconstructed.StaticAddressScript()
	require.NoError(t, err)
	require.Equal(t, addrParams.PkScript, pkScript)

	expectedAddr, err := taprootAddress(
		addrParams.ClientPubkey, addrParams.ServerPubkey,
		int64(addrParams.Expiry), lnd.ChainParams,
	)
	require.NoError(t, err)

	reconstructedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(reconstructed.TaprootKey), lnd.ChainParams,
	)
	require.NoError(t, err)
	require.Equal(t, expectedAddr.String(), reconstructedAddr.String())
}

// TestStaticAddressBackupReconstructsChangeStaticAddress verifies that the
// backed-up change key family can reconstruct the change static address and
// that it is distinct from the legacy main static address.
func TestStaticAddressBackupReconstructsChangeStaticAddress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}

	expectedChangeKey, err := lnd.WalletKit.DeriveKey(
		ctx, &keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressChangeKeyFamily),
			Index:  0,
		},
	)
	require.NoError(t, err)

	expectedChangeStaticAddr, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2,
		int64(addrParams.Expiry), expectedChangeKey.PubKey,
		addrParams.ServerPubkey,
	)
	require.NoError(t, err)

	expectedChangePkScript, err := expectedChangeStaticAddr.StaticAddressScript()
	require.NoError(t, err)

	expectedChangeAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(expectedChangeStaticAddr.TaprootKey),
		lnd.ChainParams,
	)
	require.NoError(t, err)

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 123, time.UTC),
	)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)

	backupFile, err := svc.WriteBackup(ctx)
	require.NoError(t, err)

	key, err := svc.deriveEncryptionKey(ctx)
	require.NoError(t, err)

	payload, err := readBackupPayload(key, backupFile)
	require.NoError(t, err)
	require.NotNil(t, payload.StaticAddress)

	serverPubKey, err := btcec.ParsePubKey(
		payload.StaticAddress.ServerPubKey,
	)
	require.NoError(t, err)

	changeKeyDesc, err := lnd.WalletKit.DeriveKey(
		ctx, &keychain.KeyLocator{
			Family: keychain.KeyFamily(
				payload.StaticAddress.ChangeKeyFamily,
			),
			Index: 0,
		},
	)
	require.NoError(t, err)

	reconstructed, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2,
		int64(payload.StaticAddress.Expiry), changeKeyDesc.PubKey,
		serverPubKey,
	)
	require.NoError(t, err)

	pkScript, err := reconstructed.StaticAddressScript()
	require.NoError(t, err)
	require.Equal(t, expectedChangePkScript, pkScript)

	reconstructedAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(reconstructed.TaprootKey),
		lnd.ChainParams,
	)
	require.NoError(t, err)
	require.Equal(t, expectedChangeAddr.String(), reconstructedAddr.String())

	legacyAddr, err := taprootAddress(
		addrParams.ClientPubkey, addrParams.ServerPubkey,
		int64(addrParams.Expiry), lnd.ChainParams,
	)
	require.NoError(t, err)
	require.NotEqual(t, legacyAddr.String(), reconstructedAddr.String())
}

// TestWriteBackupIsImmutablePerL402 verifies that an existing backup for the
// active L402 token prevents rewriting or creating another backup for the same
// generation.
func TestWriteBackupIsImmutablePerL402(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}

	tokenID := writePaidToken(
		t, dir, 2, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)

	firstBackup, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Equal(
		t,
		backupFilePath(
			dir, tokenID,
			time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC).
				UnixNano(),
		),
		firstBackup,
	)

	secondBackup, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, secondBackup)
	require.Equal(t, []string{firstBackup}, listBackupFiles(t, dir))
}

// TestWriteBackupIgnoresInvalidSameTokenBackup verifies that a corrupt file with
// the active token ID in its name does not suppress creation of a valid backup.
func TestWriteBackupIgnoresInvalidSameTokenBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}

	tokenCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 0, time.UTC,
	)
	tokenID := writePaidToken(t, dir, 3, tokenCreatedAt)

	backupPath := backupFilePath(dir, tokenID, tokenCreatedAt.UnixNano())
	err := os.WriteFile(backupPath, []byte("corrupt backup"), 0600)
	require.NoError(t, err)

	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)
	writtenBackup, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Equal(t, backupPath, writtenBackup)

	key, err := svc.deriveEncryptionKey(context.Background())
	require.NoError(t, err)

	payload, err := readBackupPayload(key, backupPath)
	require.NoError(t, err)
	require.Equal(t, tokenID, payload.L402TokenID)
	require.Equal(t, tokenCreatedAt.UnixNano(), payload.L402TokenCreatedAt)
}

// TestRestoreLatestBackupPrefersNewestGeneration verifies that restoring
// without an explicit path selects the newest valid backup generation.
func TestRestoreLatestBackupPrefersNewestGeneration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)

	writePaidToken(
		t, backupDir, 0x20,
		time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	firstBackup, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	writePaidToken(
		t, backupDir, 0x10,
		time.Date(2026, time.April, 14, 9, 31, 1, 0, time.UTC),
	)
	secondBackup, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	copyFile(t, firstBackup, filepath.Join(restoreDir, filepath.Base(firstBackup)))
	copyFile(
		t, secondBackup, filepath.Join(restoreDir, filepath.Base(secondBackup)),
	)

	destStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
	}
	destSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		destStaticMgr, nil,
	)

	result, err := destSvc.Restore(ctx, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(
		restoreDir, filepath.Base(secondBackup),
	), result.BackupFile)
	require.True(t, result.RestoredL402)
	require.True(t, result.RestoredStaticAddress)
}

// TestRestoreLatestOnFreshInstallUsesLatestTimestampInTitle verifies that
// startup recovery on a fresh install selects the newest timestamped backup
// filename.
func TestRestoreLatestOnFreshInstallUsesLatestTimestampInTitle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit, staticMgr, nil,
	)

	firstCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 0, time.UTC,
	)
	writePaidToken(t, backupDir, 0x20, firstCreatedAt)
	firstBackup, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	secondCreatedAt := time.Date(
		2026, time.April, 14, 9, 31, 1, 0, time.UTC,
	)
	writePaidToken(t, backupDir, 0x10, secondCreatedAt)
	secondBackup, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	err = os.Remove(filepath.Join(backupDir, paidTokenFileName))
	require.NoError(t, err)

	destStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
	}
	destSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		destStaticMgr, nil,
	)

	result, restored, err := destSvc.RestoreLatestOnFreshInstall(ctx)
	require.NoError(t, err)
	require.True(t, restored)
	require.Equal(t, secondBackup, result.BackupFile)

	// Keep both variables referenced to make the intended ordering explicit.
	require.NotEqual(t, firstBackup, secondBackup)
}

// TestLatestBackupFilePathSelection verifies latest-backup selection across
// invalid candidates, empty valid sets and timestamp tie-breaks.
func TestLatestBackupFilePathSelection(t *testing.T) {
	t.Parallel()

	t.Run("skips invalid newer backups", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		lnd := testutils.NewMockLnd()
		addrParams := makeStaticAddressParams(
			t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
		)
		svc := NewService(
			dir, "testnet", lnd.Signer, lnd.WalletKit,
			&mockStaticAddressManager{
				chainParams: lnd.ChainParams,
				params:      addrParams,
			}, nil,
		)

		writePaidToken(
			t, dir, 0x01,
			time.Date(2026, time.April, 14, 9, 30, 1, 0,
				time.UTC),
		)
		validPath, err := svc.WriteBackup(ctx)
		require.NoError(t, err)

		key, err := svc.deriveEncryptionKey(ctx)
		require.NoError(t, err)

		newer := time.Date(
			2026, time.April, 14, 10, 30, 1, 0, time.UTC,
		).UnixNano()
		corruptID := testTokenID(0x02)
		err = os.WriteFile(
			backupFilePath(dir, corruptID, newer),
			[]byte("invalid"), 0600,
		)
		require.NoError(t, err)

		mismatchNameID := testTokenID(0x03)
		writeBackupPayload(
			t, svc, dir, mismatchNameID, newer+1,
			validBackupPayload(t, testTokenID(0x04), newer+1),
		)

		wrongNetworkID := testTokenID(0x05)
		wrongNetworkPayload := validBackupPayload(t, wrongNetworkID,
			newer+2)
		wrongNetworkPayload.Network = "mainnet"
		writeBackupPayload(
			t, svc, dir, wrongNetworkID, newer+2,
			wrongNetworkPayload,
		)

		incompleteID := testTokenID(0x06)
		incompletePayload := validBackupPayload(t, incompleteID,
			newer+3)
		incompletePayload.TokenFiles = nil
		writeBackupPayload(
			t, svc, dir, incompleteID, newer+3,
			incompletePayload,
		)

		latestFile, err := latestBackupFilePath(dir, key, "testnet")
		require.NoError(t, err)
		require.Equal(t, validPath, latestFile)
	})

	t.Run("returns error without valid backup", func(t *testing.T) {
		dir := t.TempDir()
		lnd := testutils.NewMockLnd()
		svc := NewService(
			dir, "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
		)
		key, err := svc.deriveEncryptionKey(context.Background())
		require.NoError(t, err)

		err = os.WriteFile(
			backupFilePath(dir, testTokenID(0x01), 1),
			[]byte("invalid"), 0600,
		)
		require.NoError(t, err)

		_, err = latestBackupFilePath(dir, key, "testnet")
		require.ErrorContains(t, err, "backup file is too short")
	})

	t.Run("tie breaks by token id", func(t *testing.T) {
		dir := t.TempDir()
		lnd := testutils.NewMockLnd()
		svc := NewService(
			dir, "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
		)

		timestamp := time.Date(
			2026, time.April, 14, 9, 30, 1, 0, time.UTC,
		).UnixNano()
		lowerID := testTokenID(0x01)
		higherID := testTokenID(0x02)

		lowerPath := writeBackupPayload(
			t, svc, dir, lowerID, timestamp,
			validBackupPayload(t, lowerID, timestamp),
		)
		higherPath := writeBackupPayload(
			t, svc, dir, higherID, timestamp,
			validBackupPayload(t, higherID, timestamp),
		)

		key, err := svc.deriveEncryptionKey(context.Background())
		require.NoError(t, err)

		latestFile, err := latestBackupFilePath(dir, key, "testnet")
		require.NoError(t, err)
		require.NotEqual(t, lowerPath, higherPath)
		require.Equal(t, higherPath, latestFile)
	})
}

// TestRestoreStaticAddressAndPaidToken documents the unit-level recovery story:
// a paid L402 generation with static-address parameters is written to an
// encrypted backup, then restored into an empty Loop data directory using the
// same lnd-derived encryption key. The restore must recreate the static address
// with the exact backed-up key material, expiry, protocol version and initiation
// height, write back the exact L402 token bytes, and run deposit reconciliation
// so a recovered daemon can discover funds sent to that static address. The
// live itest should extend this story with confirmed deposits, loop-in change,
// explicit generation switching via loop recover, withdrawals and swaps against
// a running Loop server.
func TestRestoreStaticAddressAndPaidToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		sourceStaticMgr, nil,
	)

	writePaidToken(
		t, backupDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	originalToken, err := os.ReadFile(filepath.Join(backupDir, paidTokenFileName))
	require.NoError(t, err)

	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	destStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
	}
	depositMgr := &mockDepositManager{
		depositsFound: 3,
	}
	destSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		destStaticMgr, depositMgr,
	)

	result, err := destSvc.Restore(ctx, backupFile)
	require.NoError(t, err)
	require.Equal(t, backupFile, result.BackupFile)
	require.True(t, result.RestoredStaticAddress)
	require.True(t, result.RestoredL402)
	require.Equal(t, 3, result.NumDepositsFound)
	require.Empty(t, result.DepositReconciliationError)

	require.Len(t, destStaticMgr.restoreCalls, 1)
	restoredParams := destStaticMgr.restoreCalls[0]
	require.True(t, restoredParams.ClientPubkey.IsEqual(addrParams.ClientPubkey))
	require.True(t, restoredParams.ServerPubkey.IsEqual(addrParams.ServerPubkey))
	require.Equal(t, addrParams.Expiry, restoredParams.Expiry)
	require.Equal(t, addrParams.PkScript, restoredParams.PkScript)
	require.Equal(t, addrParams.KeyLocator, restoredParams.KeyLocator)
	require.Equal(
		t, addrParams.ProtocolVersion, restoredParams.ProtocolVersion,
	)
	require.Equal(
		t, addrParams.InitiationHeight, restoredParams.InitiationHeight,
	)

	require.Equal(t, 1, depositMgr.calls)
	restoredToken, err := os.ReadFile(
		filepath.Join(restoreDir, paidTokenFileName),
	)
	require.NoError(t, err)
	require.Equal(t, originalToken, restoredToken)

	expectedAddr, err := destStaticMgr.GetTaprootAddress(
		addrParams.ClientPubkey, addrParams.ServerPubkey,
		int64(addrParams.Expiry),
	)
	require.NoError(t, err)
	require.Equal(t, expectedAddr.String(), result.StaticAddress)
}

// TestRestoreReturnsDepositReconciliationError verifies that restore succeeds
// even when deposit reconciliation fails, while reporting the reconciliation
// error to the caller.
func TestRestoreReturnsDepositReconciliationError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		sourceStaticMgr, nil,
	)
	writePaidToken(
		t, backupDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	depositErr := errors.New("reconcile failed")
	destSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{chainParams: lnd.ChainParams},
		&mockDepositManager{err: depositErr},
	)

	result, err := destSvc.Restore(ctx, backupFile)
	require.NoError(t, err)
	require.Equal(t, depositErr.Error(), result.DepositReconciliationError)
	require.Equal(t, 0, result.NumDepositsFound)
}

// TestRestoreReportsNoStaticAddressChangeForIdempotentRestore verifies that
// restoring the already-present generation reports no L402 or static-address
// change while still returning the recovered address.
func TestRestoreReportsNoStaticAddressChangeForIdempotentRestore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
		params:      addrParams,
	}
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		sourceStaticMgr, nil,
	)
	writePaidToken(
		t, backupDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	writePaidToken(
		t, restoreDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	destStaticMgr := &mockStaticAddressManager{
		chainParams:       lnd.ChainParams,
		restoreChanged:    false,
		restoreChangedSet: true,
	}
	destSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		destStaticMgr, nil,
	)

	result, err := destSvc.Restore(ctx, backupFile)
	require.NoError(t, err)
	require.False(t, result.RestoredL402)
	require.False(t, result.RestoredStaticAddress)
	require.NotEmpty(t, result.StaticAddress)
	require.Len(t, destStaticMgr.restoreCalls, 1)
}

// TestRestoreLatestOnFreshInstallSkipsNonFreshInstall verifies that startup
// auto-restore is skipped whenever local token or static-address state already
// makes the data directory non-fresh, even if a valid backup is present.
func TestRestoreLatestOnFreshInstallSkipsNonFreshInstall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, string, *testutils.LndMockServices) StaticAddressManager
	}{
		{
			name: "local paid token",
			setup: func(t *testing.T, dir string,
				lnd *testutils.LndMockServices) StaticAddressManager {

				writePaidToken(
					t, dir, 1,
					time.Date(2026, time.April, 14, 9, 30,
						1, 0, time.UTC),
				)

				return &mockStaticAddressManager{
					chainParams: lnd.ChainParams,
				}
			},
		},
		{
			name: "local pending token",
			setup: func(t *testing.T, dir string,
				lnd *testutils.LndMockServices) StaticAddressManager {

				writePendingToken(
					t, dir, 1,
					time.Date(2026, time.April, 14, 9, 30,
						1, 0, time.UTC),
				)

				return &mockStaticAddressManager{
					chainParams: lnd.ChainParams,
				}
			},
		},
		{
			name: "local static address",
			setup: func(t *testing.T, _ string,
				lnd *testutils.LndMockServices) StaticAddressManager {

				addrParams := makeStaticAddressParams(
					t, lnd, 7, defaultRecoveryServerPubkey,
					144, 321,
				)

				return &mockStaticAddressManager{
					chainParams: lnd.ChainParams,
					params:      addrParams,
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			lnd := testutils.NewMockLnd()
			staticMgr := test.setup(t, dir, lnd)

			svc := NewService(
				dir, "testnet", lnd.Signer, lnd.WalletKit,
				staticMgr, nil,
			)
			backupTokenID := testTokenID(0x20)
			writeBackupPayload(
				t, svc, dir, backupTokenID, 2,
				validBackupPayload(t, backupTokenID, 2),
			)

			result, restored, err := svc.RestoreLatestOnFreshInstall(
				ctx,
			)
			require.NoError(t, err)
			require.False(t, restored)
			require.Nil(t, result)
		})
	}
}

// TestRestoreRejectsNetworkMismatch verifies that a backup from another Loop
// network cannot be restored into the current network data directory.
func TestRestoreRejectsNetworkMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	sourceSvc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params: makeStaticAddressParams(
				t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
			),
		}, nil,
	)
	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	restoreSvc := NewService(
		t.TempDir(), "mainnet", lnd.Signer, lnd.WalletKit, nil, nil,
	)

	_, err = restoreSvc.Restore(ctx, backupFile)
	require.ErrorContains(t, err, "does not match")
}

// TestRestoreRejectsInvalidPayloadMetadata verifies that unsupported or
// incomplete backup payload metadata is rejected before any local state is
// restored.
func TestRestoreRejectsInvalidPayloadMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*backupPayload)
		expectedErr string
	}{
		{
			name: "unsupported version",
			mutate: func(payload *backupPayload) {
				payload.Version = backupVersion + 1
			},
			expectedErr: "unsupported backup version",
		},
		{
			name: "missing network",
			mutate: func(payload *backupPayload) {
				payload.Network = ""
			},
			expectedErr: "backup file is missing a network",
		},
		{
			name: "missing token ID",
			mutate: func(payload *backupPayload) {
				payload.L402TokenID = ""
			},
			expectedErr: "backup file is missing an L402 token ID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			lnd := testutils.NewMockLnd()
			svc := NewService(
				dir, "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
			)

			tokenID := testTokenID(0x10)
			payload := validBackupPayload(t, tokenID, 1)
			test.mutate(payload)

			backupFile := writeBackupPayload(t, svc, dir, tokenID, 1, payload)

			_, err := svc.Restore(context.Background(), backupFile)
			require.ErrorContains(t, err, test.expectedErr)
		})
	}
}

// TestRestoreRejectsIncompleteRecoverableGeneration verifies that restore does
// not accept backups that lack either side of the documented paid-L402/static
// address generation.
func TestRestoreRejectsIncompleteRecoverableGeneration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*backupPayload)
		expectedErr string
	}{
		{
			name: "missing paid token data",
			mutate: func(payload *backupPayload) {
				payload.TokenFiles = nil
			},
			expectedErr: "missing paid L402 token data",
		},
		{
			name: "missing static address",
			mutate: func(payload *backupPayload) {
				payload.StaticAddress = nil
			},
			expectedErr: "missing static address parameters",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			lnd := testutils.NewMockLnd()
			backupDir := t.TempDir()
			restoreDir := t.TempDir()

			addrParams := makeStaticAddressParams(
				t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
			)
			sourceSvc := NewService(
				backupDir, "testnet", lnd.Signer, lnd.WalletKit,
				&mockStaticAddressManager{
					chainParams: lnd.ChainParams,
					params:      addrParams,
				}, nil,
			)

			writePaidToken(
				t, backupDir, 1,
				time.Date(
					2026, time.April, 14, 9, 30, 1, 0,
					time.UTC,
				),
			)

			backupFile, err := sourceSvc.WriteBackup(ctx)
			require.NoError(t, err)

			key, err := sourceSvc.deriveEncryptionKey(ctx)
			require.NoError(t, err)

			payload, err := readBackupPayload(key, backupFile)
			require.NoError(t, err)

			test.mutate(payload)
			mutatedBackup := writeBackupPayload(
				t, sourceSvc, backupDir, payload.L402TokenID,
				payload.L402TokenCreatedAt, payload,
			)

			destStaticMgr := &mockStaticAddressManager{
				chainParams: lnd.ChainParams,
			}
			restoreSvc := NewService(
				restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
				destStaticMgr, nil,
			)

			_, err = restoreSvc.Restore(ctx, mutatedBackup)
			require.ErrorContains(t, err, test.expectedErr)
			require.Empty(t, destStaticMgr.restoreCalls)
		})
	}
}

// TestRestoreRejectsTokenMetadataMismatch verifies that the raw token bytes in
// the backup must match the payload's generation metadata before restore writes
// any state.
func TestRestoreRejectsTokenMetadataMismatch(t *testing.T) {
	t.Parallel()

	originalCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 0, time.UTC,
	)

	tests := []struct {
		name           string
		tokenSeed      byte
		tokenCreatedAt time.Time
		expectedErr    string
	}{
		{
			name:           "token ID mismatch",
			tokenSeed:      2,
			tokenCreatedAt: originalCreatedAt,
			expectedErr:    "does not match payload token ID",
		},
		{
			name:      "creation time mismatch",
			tokenSeed: 1,
			tokenCreatedAt: time.Date(
				2026, time.April, 14, 10, 30, 1, 0,
				time.UTC,
			),
			expectedErr: "does not match payload creation time",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			lnd := testutils.NewMockLnd()
			backupDir := t.TempDir()
			restoreDir := t.TempDir()

			addrParams := makeStaticAddressParams(
				t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
			)
			sourceSvc := NewService(
				backupDir, "testnet", lnd.Signer, lnd.WalletKit,
				&mockStaticAddressManager{
					chainParams: lnd.ChainParams,
					params:      addrParams,
				}, nil,
			)

			writePaidToken(t, backupDir, 1, originalCreatedAt)
			backupFile, err := sourceSvc.WriteBackup(ctx)
			require.NoError(t, err)

			key, err := sourceSvc.deriveEncryptionKey(ctx)
			require.NoError(t, err)

			payload, err := readBackupPayload(key, backupFile)
			require.NoError(t, err)

			otherTokenDir := t.TempDir()
			writePaidToken(
				t, otherTokenDir, test.tokenSeed,
				test.tokenCreatedAt,
			)
			otherToken, err := os.ReadFile(
				filepath.Join(otherTokenDir, paidTokenFileName),
			)
			require.NoError(t, err)

			payload.TokenFiles = []*l402TokenFileEntry{{
				Name: paidTokenFileName,
				Data: otherToken,
			}}
			mutatedBackup := writeBackupPayload(
				t, sourceSvc, backupDir, payload.L402TokenID,
				payload.L402TokenCreatedAt, payload,
			)

			destStaticMgr := &mockStaticAddressManager{
				chainParams: lnd.ChainParams,
			}
			restoreSvc := NewService(
				restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
				destStaticMgr, nil,
			)

			_, err = restoreSvc.Restore(ctx, mutatedBackup)
			require.ErrorContains(t, err, test.expectedErr)
			require.Empty(t, destStaticMgr.restoreCalls)

			_, err = os.Stat(filepath.Join(restoreDir, paidTokenFileName))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

// TestRestoreRejectsExplicitFilenamePayloadTokenIDMismatch verifies that an
// explicit path is held to the same filename/payload token-ID consistency check
// used during latest-backup discovery.
func TestRestoreRejectsExplicitFilenamePayloadTokenIDMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)

	writePaidToken(
		t, backupDir, 1,
		time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	key, err := sourceSvc.deriveEncryptionKey(ctx)
	require.NoError(t, err)

	payload, err := readBackupPayload(key, backupFile)
	require.NoError(t, err)

	mismatchedBackup := writeBackupPayload(
		t, sourceSvc, backupDir, testTokenID(2),
		payload.L402TokenCreatedAt, payload,
	)

	restoreSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{chainParams: lnd.ChainParams}, nil,
	)

	_, err = restoreSvc.Restore(ctx, mismatchedBackup)
	require.ErrorContains(t, err, "backup file token ID")
}

// TestPrepareStaticAddressRestoreRejectsInvalidBackup verifies that malformed
// static-address backup fields are rejected before RestoreAddress is called.
func TestPrepareStaticAddressRestoreRejectsInvalidBackup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)

	baseBackup := func() *staticAddressBackup {
		return &staticAddressBackup{
			ProtocolVersion: uint32(addrParams.ProtocolVersion),
			ClientPubKey: addrParams.ClientPubkey.
				SerializeCompressed(),
			ServerPubKey: addrParams.ServerPubkey.
				SerializeCompressed(),
			Expiry: addrParams.Expiry,
			LegacyClientKeyFamily: int32(
				addrParams.KeyLocator.Family,
			),
			MainKeyFamily:           swap.StaticMultiAddressKeyFamily,
			ChangeKeyFamily:         swap.StaticAddressChangeKeyFamily,
			LegacyFirstHeight:       addrParams.InitiationHeight,
			MultiAddressFirstHeight: addrParams.InitiationHeight,
		}
	}

	_, unrelatedPubKey := testutils.CreateKey(99)
	tests := []struct {
		name        string
		mutate      func(*staticAddressBackup)
		expectedErr string
	}{
		{
			name: "invalid protocol version",
			mutate: func(backup *staticAddressBackup) {
				backup.ProtocolVersion = 99
			},
			expectedErr: "invalid static address protocol version",
		},
		{
			name: "missing client pubkey",
			mutate: func(backup *staticAddressBackup) {
				backup.ClientPubKey = nil
			},
			expectedErr: "missing the static address client pubkey",
		},
		{
			name: "missing legacy client key family",
			mutate: func(backup *staticAddressBackup) {
				backup.LegacyClientKeyFamily = 0
			},
			expectedErr: "missing the legacy static address " +
				"client key family",
		},
		{
			name: "malformed client pubkey",
			mutate: func(backup *staticAddressBackup) {
				backup.ClientPubKey = []byte{0x01, 0x02}
			},
			expectedErr: "public key",
		},
		{
			name: "malformed server pubkey",
			mutate: func(backup *staticAddressBackup) {
				backup.ServerPubKey = []byte{0x01, 0x02}
			},
			expectedErr: "public key",
		},
		{
			name: "client key not found",
			mutate: func(backup *staticAddressBackup) {
				backup.ClientPubKey = unrelatedPubKey.
					SerializeCompressed()
			},
			expectedErr: "unable to derive static address client key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			svc := NewService(
				t.TempDir(), "testnet", lnd.Signer, lnd.WalletKit,
				&mockStaticAddressManager{
					chainParams: lnd.ChainParams,
				}, nil,
			)
			backup := baseBackup()
			test.mutate(backup)

			_, err := svc.prepareStaticAddressRestore(ctx, backup)
			require.ErrorContains(t, err, test.expectedErr)
		})
	}
}

// TestRestoreRejectsExplicitPathWithInvalidName verifies that explicit restore
// paths must use the documented backup filename format.
func TestRestoreRejectsExplicitPathWithInvalidName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()

	restoreSvc := NewService(
		t.TempDir(), "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
	)

	invalidPath := filepath.Join(t.TempDir(), "backup.enc")
	err := os.WriteFile(invalidPath, []byte("invalid"), 0600)
	require.NoError(t, err)

	_, err = restoreSvc.Restore(ctx, invalidPath)
	require.ErrorContains(t, err, "invalid backup file path")
}

// TestRestoreFailsWithoutStaticAddressManager verifies that backups containing
// static-address state cannot be restored when the daemon has no static-address
// manager configured.
func TestRestoreFailsWithoutStaticAddressManager(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 3, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)
	writePaidToken(
		t, backupDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	restoreSvc := NewService(
		t.TempDir(), "testnet", lnd.Signer, lnd.WalletKit, nil, nil,
	)

	_, err = restoreSvc.Restore(ctx, backupFile)
	require.ErrorContains(t, err, "static address restore is unavailable")
}

// TestRestoreRejectsDifferentExistingTokenBeforeStaticAddress verifies that a
// conflicting local L402 token blocks restore before static-address state is
// modified.
func TestRestoreRejectsDifferentExistingTokenBeforeStaticAddress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 3, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)
	writePaidToken(
		t, backupDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	err = os.WriteFile(
		filepath.Join(restoreDir, paidTokenFileName),
		[]byte("conflicting-token"), 0600,
	)
	require.NoError(t, err)

	destStaticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
	}
	restoreSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		destStaticMgr, nil,
	)

	_, err = restoreSvc.Restore(ctx, backupFile)
	require.ErrorContains(t, err, "different contents")
	require.Empty(t, destStaticMgr.restoreCalls)
}

// TestRestoreRollsBackTokenFilesOnStaticAddressFailure verifies that token
// files written during restore are removed again if static-address restore
// fails.
func TestRestoreRollsBackTokenFilesOnStaticAddressFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	backupDir := t.TempDir()
	restoreDir := t.TempDir()

	addrParams := makeStaticAddressParams(
		t, lnd, 3, defaultRecoveryServerPubkey, 144, 321,
	)
	sourceSvc := NewService(
		backupDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)
	writePaidToken(
		t, backupDir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	backupFile, err := sourceSvc.WriteBackup(ctx)
	require.NoError(t, err)

	restoreSvc := NewService(
		restoreDir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			restoreErr:  errors.New("restore address failed"),
		}, nil,
	)

	_, err = restoreSvc.Restore(ctx, backupFile)
	require.ErrorContains(t, err, "restore address failed")

	_, err = os.Stat(filepath.Join(restoreDir, paidTokenFileName))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestResolveClientKeyScansLegacyClientFamilyReadOnly verifies that locating the
// backed-up static-address client key scans existing keys without advancing the
// wallet's next-key index.
func TestResolveClientKeyScansLegacyClientFamilyReadOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	targetIndex := uint32(5)
	addrParams := makeStaticAddressParams(
		t, lnd, targetIndex, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
	}
	svc := NewService(
		t.TempDir(), "testnet", lnd.Signer, lnd.WalletKit,
		staticMgr, nil,
	)

	backup := &staticAddressBackup{
		ProtocolVersion:         uint32(addrParams.ProtocolVersion),
		ClientPubKey:            addrParams.ClientPubkey.SerializeCompressed(),
		ServerPubKey:            addrParams.ServerPubkey.SerializeCompressed(),
		Expiry:                  addrParams.Expiry,
		LegacyClientKeyFamily:   swap.StaticAddressKeyFamily,
		MainKeyFamily:           swap.StaticMultiAddressKeyFamily,
		ChangeKeyFamily:         swap.StaticAddressChangeKeyFamily,
		LegacyFirstHeight:       addrParams.InitiationHeight,
		MultiAddressFirstHeight: addrParams.InitiationHeight,
	}

	clientKey, locator, err := svc.resolveClientKey(ctx, backup)
	require.NoError(t, err)
	require.Equal(t, addrParams.KeyLocator, locator)
	require.True(t, clientKey.IsEqual(addrParams.ClientPubkey))

	nextKey, err := lnd.WalletKit.DeriveNextKey(
		ctx, swap.StaticAddressKeyFamily,
	)
	require.NoError(t, err)
	require.EqualValues(t, 0, nextKey.Index)
}

// TestResolveClientKeyScansLegacyClientFamily verifies that restore can find a
// backed-up static-address client key within the configured legacy client
// family scan range.
func TestResolveClientKeyScansLegacyClientFamily(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lnd := testutils.NewMockLnd()
	targetIndex := uint32(7)
	addrParams := makeStaticAddressParams(
		t, lnd, targetIndex, defaultRecoveryServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		chainParams: lnd.ChainParams,
	}
	svc := NewService(
		t.TempDir(), "testnet", lnd.Signer, lnd.WalletKit,
		staticMgr, nil,
	)

	backup := &staticAddressBackup{
		ProtocolVersion:         uint32(addrParams.ProtocolVersion),
		ClientPubKey:            addrParams.ClientPubkey.SerializeCompressed(),
		ServerPubKey:            addrParams.ServerPubkey.SerializeCompressed(),
		Expiry:                  addrParams.Expiry,
		LegacyClientKeyFamily:   swap.StaticAddressKeyFamily,
		MainKeyFamily:           swap.StaticMultiAddressKeyFamily,
		ChangeKeyFamily:         swap.StaticAddressChangeKeyFamily,
		LegacyFirstHeight:       addrParams.InitiationHeight,
		MultiAddressFirstHeight: addrParams.InitiationHeight,
	}

	clientKey, locator, err := svc.resolveClientKey(ctx, backup)
	require.NoError(t, err)
	require.Equal(t, addrParams.KeyLocator, locator)
	require.True(t, clientKey.IsEqual(addrParams.ClientPubkey))
}

// TestResolveClientKeyFamilySelection verifies the V0 restore path scans the
// concrete legacy client-key family, while keeping the future multi-address
// receive/change families out of the lookup.
func TestResolveClientKeyFamilySelection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	targetIndex := uint32(6)
	_, expectedPubKey := testutils.CreateKey(200)

	tests := []struct {
		name                 string
		backup               *staticAddressBackup
		expectedLookupFamily int32
	}{
		{
			name: "explicit legacy client family wins",
			backup: &staticAddressBackup{
				ClientPubKey:          expectedPubKey.SerializeCompressed(),
				LegacyClientKeyFamily: swap.StaticAddressKeyFamily,
				MainKeyFamily:         swap.StaticMultiAddressKeyFamily,
				ChangeKeyFamily:       swap.StaticAddressChangeKeyFamily,
			},
			expectedLookupFamily: swap.StaticAddressKeyFamily,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			walletKit := &familyScopedWalletKit{
				expectedFamily: test.expectedLookupFamily,
				expectedIndex:  targetIndex,
				expectedPubKey: expectedPubKey,
			}
			svc := NewService(
				t.TempDir(), "testnet", nil, walletKit, nil, nil,
			)

			clientKey, locator, err := svc.resolveClientKey(
				ctx, test.backup,
			)
			require.NoError(t, err)
			require.True(t, clientKey.IsEqual(expectedPubKey))
			require.Equal(
				t, keychain.KeyFamily(test.expectedLookupFamily),
				locator.Family,
			)
			require.Equal(t, targetIndex, locator.Index)
			require.NotEmpty(t, walletKit.calls)

			for _, call := range walletKit.calls {
				require.Equal(
					t,
					keychain.KeyFamily(test.expectedLookupFamily),
					call.Family,
				)
			}
		})
	}
}

// TestRestoreTokenFiles verifies that paid token material is restored exactly
// once and that restoring identical token bytes is idempotent.
func TestRestoreTokenFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	svc := &Service{
		dataDir: dir,
	}

	restoreResult, err := svc.restoreTokenFiles([]*l402TokenFileEntry{{
		Name: "l402.token",
		Data: []byte("paid-token"),
	}})
	require.NoError(t, err)
	require.True(t, restoreResult.restored)

	paidToken, err := os.ReadFile(filepath.Join(dir, "l402.token"))
	require.NoError(t, err)
	require.Equal(t, []byte("paid-token"), paidToken)

	restoreResult, err = svc.restoreTokenFiles([]*l402TokenFileEntry{{
		Name: "l402.token",
		Data: []byte("paid-token"),
	}})
	require.NoError(t, err)
	require.False(t, restoreResult.restored)
}

// TestRestoreTokenFilesRejectsDifferentExistingToken verifies that restore
// refuses to overwrite existing paid token material with different bytes.
func TestRestoreTokenFilesRejectsDifferentExistingToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := os.WriteFile(
		filepath.Join(dir, "l402.token"), []byte("current-token"), 0600,
	)
	require.NoError(t, err)

	svc := &Service{
		dataDir: dir,
	}

	_, err = svc.restoreTokenFiles([]*l402TokenFileEntry{{
		Name: "l402.token",
		Data: []byte("backup-token"),
	}})
	require.ErrorContains(t, err, "different contents")
}

// TestRestoreTokenFilesRejectsExistingPendingToken verifies that explicit
// restore refuses to mix backed-up paid token material with local pending L402
// acquisition state.
func TestRestoreTokenFilesRejectsExistingPendingToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	err := os.WriteFile(
		filepath.Join(dir, pendingTokenFileName),
		[]byte("pending-token"), 0600,
	)
	require.NoError(t, err)

	svc := &Service{
		dataDir: dir,
	}

	_, err = svc.restoreTokenFiles([]*l402TokenFileEntry{{
		Name: paidTokenFileName,
		Data: []byte("paid-token"),
	}})
	require.ErrorContains(t, err, "unexpected file")
	require.ErrorContains(t, err, pendingTokenFileName)

	_, err = os.Stat(filepath.Join(dir, paidTokenFileName))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestRestoreTokenFilesRejectsInvalidName verifies that backup payloads cannot
// write arbitrary or pending-token filenames into the token store.
func TestRestoreTokenFilesRejectsInvalidName(t *testing.T) {
	t.Parallel()

	svc := &Service{
		dataDir: t.TempDir(),
	}

	_, err := svc.restoreTokenFiles([]*l402TokenFileEntry{{
		Name: "l402.token.pending",
		Data: []byte("pending-token"),
	}})
	require.ErrorContains(t, err, "unexpected token file name")
}

// TestLatestBackupFilePathIgnoresMalformedNames verifies that files which do
// not match the backup filename grammar are ignored during latest-backup
// selection.
func TestLatestBackupFilePathIgnoresMalformedNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultRecoveryServerPubkey, 144, 321,
	)
	svc := NewService(
		dir, "testnet", lnd.Signer, lnd.WalletKit,
		&mockStaticAddressManager{
			chainParams: lnd.ChainParams,
			params:      addrParams,
		}, nil,
	)

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)
	validPath, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)

	err = os.WriteFile(
		filepath.Join(dir, "L402_backup_not-an-id.enc"),
		[]byte("invalid"), 0600,
	)
	require.NoError(t, err)
	err = os.WriteFile(
		filepath.Join(dir, backupFileName(stringsOfLength(64), 1)),
		[]byte("invalid"), 0600,
	)
	require.NoError(t, err)

	key, err := svc.deriveEncryptionKey(context.Background())
	require.NoError(t, err)

	latestFile, err := latestBackupFilePath(dir, key, "testnet")
	require.NoError(t, err)
	require.Equal(t, validPath, latestFile)
}

// TestWriteFileAtomically verifies that backup files are written with private
// permissions and that failed atomic writes clean up their temporary files.
func TestWriteFileAtomically(t *testing.T) {
	t.Parallel()

	t.Run("uses private permissions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "backup.enc")
		err := writeFileAtomically(path, []byte("backup"))
		require.NoError(t, err)

		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	})

	t.Run("cleans temp file on rename error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "backup-target")
		err := os.Mkdir(path, 0700)
		require.NoError(t, err)

		err = writeFileAtomically(path, []byte("backup"))
		require.Error(t, err)

		_, err = os.Stat(path + ".tmp")
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

var defaultRecoveryServerPubkey = func() *btcec.PublicKey {
	_, pubKey := testutils.CreateKey(42)
	return pubKey
}()

type deriveSharedKeyCall struct {
	pubKey  *btcec.PublicKey
	locator *keychain.KeyLocator
}

type fixedKeySigner struct {
	lndclient.SignerClient

	key   [32]byte
	calls []deriveSharedKeyCall
}

func (s *fixedKeySigner) DeriveSharedKey(_ context.Context,
	pubKey *btcec.PublicKey, locator *keychain.KeyLocator) ([32]byte,
	error) {

	call := deriveSharedKeyCall{
		pubKey: pubKey,
	}
	if locator != nil {
		locatorCopy := *locator
		call.locator = &locatorCopy
	}
	s.calls = append(s.calls, call)

	return s.key, nil
}

type familyScopedWalletKit struct {
	lndclient.WalletKitClient

	expectedFamily int32
	expectedIndex  uint32
	expectedPubKey *btcec.PublicKey
	calls          []keychain.KeyLocator
}

func (w *familyScopedWalletKit) DeriveKey(_ context.Context,
	locator *keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	if locator == nil {
		return nil, fmt.Errorf("missing key locator")
	}

	w.calls = append(w.calls, *locator)

	if int32(locator.Family) == w.expectedFamily &&
		locator.Index == w.expectedIndex {

		return &keychain.KeyDescriptor{
			KeyLocator: *locator,
			PubKey:     w.expectedPubKey,
		}, nil
	}

	_, pubKey := testutils.CreateKey(
		10_000 + int32(locator.Index) + int32(locator.Family),
	)

	return &keychain.KeyDescriptor{
		KeyLocator: *locator,
		PubKey:     pubKey,
	}, nil
}

func testBackupKey(seed byte) [32]byte {
	var key [32]byte
	for idx := range key {
		key[idx] = seed
	}

	return key
}

func testTokenID(seed byte) string {
	var tokenID l402.TokenID
	tokenID[0] = seed

	return tokenID.String()
}

func validBackupPayload(t *testing.T, tokenID string,
	tokenCreatedAt int64) *backupPayload {

	t.Helper()

	parsedTokenID, err := l402.MakeIDFromString(tokenID)
	require.NoError(t, err)

	var (
		paymentHash lntypes.Hash
		preimage    lntypes.Preimage
	)
	paymentHash[0] = parsedTokenID[0]
	preimage[0] = parsedTokenID[0]

	return &backupPayload{
		Version:            backupVersion,
		Network:            "testnet",
		L402TokenID:        tokenID,
		L402TokenCreatedAt: tokenCreatedAt,
		StaticAddress:      &staticAddressBackup{},
		TokenFiles: []*l402TokenFileEntry{{
			Name: paidTokenFileName,
			Data: tokenFileData(
				t, parsedTokenID, paymentHash, preimage,
				parsedTokenID[0], time.Unix(0, tokenCreatedAt),
			),
		}},
	}
}

func writeBackupPayload(t *testing.T, svc *Service, dir, tokenID string,
	titleTimestamp int64, payload *backupPayload) string {

	t.Helper()

	plaintext, err := json.Marshal(payload)
	require.NoError(t, err)

	key, err := svc.deriveEncryptionKey(context.Background())
	require.NoError(t, err)

	encrypted, err := encryptBackupPayload(key, plaintext)
	require.NoError(t, err)

	path := backupFilePath(dir, tokenID, titleTimestamp)
	err = os.WriteFile(path, encrypted, 0600)
	require.NoError(t, err)

	return path
}

type mockStaticAddressManager struct {
	chainParams       *chaincfg.Params
	params            *address.Parameters
	currentHeight     int32
	getParamsErr      error
	restoreErr        error
	restoreCalls      []*address.Parameters
	restoreChanged    bool
	restoreChangedSet bool
}

func (m *mockStaticAddressManager) GetStaticAddressParameters(
	context.Context) (*address.Parameters, error) {

	switch {
	case m.getParamsErr != nil:
		return nil, m.getParamsErr

	case m.params == nil:
		return nil, address.ErrNoStaticAddress

	default:
		return cloneAddressParameters(m.params), nil
	}
}

func (m *mockStaticAddressManager) CurrentHeight() int32 {
	if m.currentHeight > 0 {
		return m.currentHeight
	}
	if m.params != nil {
		return m.params.InitiationHeight
	}

	return 0
}

func (m *mockStaticAddressManager) GetTaprootAddress(clientPubkey,
	serverPubkey *btcec.PublicKey, expiry int64) (*btcutil.AddressTaproot,
	error) {

	return taprootAddress(
		clientPubkey, serverPubkey, expiry, m.chainParams,
	)
}

func (m *mockStaticAddressManager) RestoreAddress(_ context.Context,
	params *address.Parameters) (*btcutil.AddressTaproot, bool, error) {

	if m.restoreErr != nil {
		return nil, false, m.restoreErr
	}

	staticAddress, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2, int64(params.Expiry),
		params.ClientPubkey, params.ServerPubkey,
	)
	if err != nil {
		return nil, false, err
	}
	pkScript, err := staticAddress.StaticAddressScript()
	if err != nil {
		return nil, false, err
	}
	if len(params.PkScript) != 0 &&
		!bytes.Equal(params.PkScript, pkScript) {

		return nil, false, fmt.Errorf("static address pk script mismatch")
	}

	params.PkScript = pkScript
	m.restoreCalls = append(m.restoreCalls, cloneAddressParameters(params))

	changed := true
	if m.restoreChangedSet {
		changed = m.restoreChanged
	}

	addr, err := m.GetTaprootAddress(
		params.ClientPubkey, params.ServerPubkey, int64(params.Expiry),
	)
	if err != nil {
		return nil, false, err
	}

	return addr, changed, nil
}

type mockDepositManager struct {
	depositsFound int
	err           error
	calls         int
}

func (m *mockDepositManager) ReconcileDeposits(context.Context) (int, error) {
	m.calls++
	return m.depositsFound, m.err
}

func makeStaticAddressParams(t *testing.T, lnd *testutils.LndMockServices,
	index uint32, serverPubKey *btcec.PublicKey, expiry uint32,
	initiationHeight int32) *address.Parameters {

	t.Helper()

	keyDesc, err := lnd.WalletKit.DeriveKey(
		context.Background(), &keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  index,
		},
	)
	require.NoError(t, err)

	staticAddress, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2, int64(expiry), keyDesc.PubKey,
		serverPubKey,
	)
	require.NoError(t, err)

	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	return &address.Parameters{
		ClientPubkey:     keyDesc.PubKey,
		ServerPubkey:     serverPubKey,
		Expiry:           expiry,
		PkScript:         pkScript,
		KeyLocator:       keyDesc.KeyLocator,
		ProtocolVersion:  staticaddrversion.ProtocolVersion_V0,
		InitiationHeight: initiationHeight,
	}
}

func cloneAddressParameters(params *address.Parameters) *address.Parameters {
	if params == nil {
		return nil
	}

	return &address.Parameters{
		ClientPubkey:     params.ClientPubkey,
		ServerPubkey:     params.ServerPubkey,
		Expiry:           params.Expiry,
		PkScript:         slices.Clone(params.PkScript),
		KeyLocator:       params.KeyLocator,
		ProtocolVersion:  params.ProtocolVersion,
		InitiationHeight: params.InitiationHeight,
	}
}

func taprootAddress(clientPubkey, serverPubkey *btcec.PublicKey, expiry int64,
	chainParams *chaincfg.Params) (*btcutil.AddressTaproot, error) {

	staticAddress, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2, expiry, clientPubkey, serverPubkey,
	)
	if err != nil {
		return nil, err
	}

	return btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(staticAddress.TaprootKey), chainParams,
	)
}

func writePaidToken(t *testing.T, dir string, seed byte,
	createdAt time.Time) string {

	t.Helper()

	return writeTokenFile(
		t, filepath.Join(dir, paidTokenFileName), seed, createdAt, true,
	)
}

func writePendingToken(t *testing.T, dir string, seed byte,
	createdAt time.Time) string {

	t.Helper()

	return writeTokenFile(
		t, filepath.Join(dir, "l402.token.pending"), seed, createdAt, false,
	)
}

func writeTokenFile(t *testing.T, path string, seed byte, createdAt time.Time,
	paid bool) string {

	t.Helper()

	var (
		paymentHash lntypes.Hash
		tokenID     l402.TokenID
		preimage    lntypes.Preimage
	)
	paymentHash[0] = seed
	tokenID[0] = seed
	if paid {
		preimage[0] = seed
	}

	data := tokenFileData(
		t, tokenID, paymentHash, preimage, seed, createdAt,
	)
	err := os.WriteFile(path, data, 0600)
	require.NoError(t, err)

	return tokenID.String()
}

func tokenFileData(t *testing.T, tokenID l402.TokenID,
	paymentHash lntypes.Hash, preimage lntypes.Preimage, seed byte,
	createdAt time.Time) []byte {

	t.Helper()

	var idBytes bytes.Buffer
	err := l402.EncodeIdentifier(&idBytes, &l402.Identifier{
		Version:     l402.LatestVersion,
		PaymentHash: paymentHash,
		TokenID:     tokenID,
	})
	require.NoError(t, err)

	mac, err := macaroon.New(
		[]byte("loop-recovery-test-root-key"),
		idBytes.Bytes(), "loop.test", macaroon.LatestVersion,
	)
	require.NoError(t, err)

	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)

	var serialized bytes.Buffer
	err = binary.Write(&serialized, binary.BigEndian, uint32(len(macBytes)))
	require.NoError(t, err)
	err = binary.Write(&serialized, binary.BigEndian, macBytes)
	require.NoError(t, err)
	err = binary.Write(&serialized, binary.BigEndian, paymentHash)
	require.NoError(t, err)
	err = binary.Write(&serialized, binary.BigEndian, preimage)
	require.NoError(t, err)
	err = binary.Write(
		&serialized, binary.BigEndian, lnwire.MilliSatoshi(seed)*1000,
	)
	require.NoError(t, err)
	err = binary.Write(
		&serialized, binary.BigEndian, lnwire.MilliSatoshi(seed)*10,
	)
	require.NoError(t, err)
	err = binary.Write(&serialized, binary.BigEndian, createdAt.UnixNano())
	require.NoError(t, err)

	return serialized.Bytes()
}

func listBackupFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var files []string
	for _, entry := range entries {
		if _, ok := backupFileTokenID(entry.Name()); ok {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}

	slices.Sort(files)
	return files
}

func copyFile(t *testing.T, src, dest string) {
	t.Helper()

	data, err := os.ReadFile(src)
	require.NoError(t, err)

	err = os.WriteFile(dest, data, 0600)
	require.NoError(t, err)
}

func stringsOfLength(length int) string {
	return string(bytes.Repeat([]byte("a"), length))
}
