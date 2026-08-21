package backup

import (
	"bytes"
	"context"
	"encoding/binary"
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

// TestEncryptDecryptBackupPayload verifies that a backup payload round-trips
// through the secretbox envelope and is not stored as plaintext.
func TestEncryptDecryptBackupPayload(t *testing.T) {
	t.Parallel()

	var key [32]byte
	copy(key[:], []byte("0123456789abcdefghijklmnopqrstuv"))

	plaintext := []byte("loop backup payload")

	encrypted, err := encryptBackupPayload(key, plaintext)
	require.NoError(t, err)
	require.NotEqual(t, plaintext, encrypted)

	decrypted, err := decryptBackupPayload(key, encrypted)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)
}

// TestBackupEncryptionUsesSignerDerivedKey verifies that backups are encrypted
// with the documented lnd-derived key.
func TestBackupEncryptionUsesSignerDerivedKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()
	signer := &fixedKeySigner{
		key: testBackupKey(1),
	}
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(
		dir, "testnet", signer,
		&mockStaticAddressManager{
			params: addrParams,
		},
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
}

// TestWriteBackupReturnsEmptyWithoutState verifies that no backup is written
// before Loop has both paid L402 state and static-address state.
func TestWriteBackupReturnsEmptyWithoutState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	svc := NewService(dir, "testnet", lnd.Signer, nil)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, backupFile)
	require.Empty(t, listBackupFiles(t, dir))
}

// TestWriteBackupReturnsEmptyWithTokenOnly verifies that a paid L402 by itself
// does not define a complete static-address generation backup.
func TestWriteBackupReturnsEmptyWithTokenOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(dir, "testnet", lnd.Signer, nil)

	backupFile, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, backupFile)
	require.Empty(t, listBackupFiles(t, dir))
}

// TestWriteBackupReturnsEmptyWithPendingToken verifies that pending L402 token
// material is not backed up as an immutable generation.
func TestWriteBackupReturnsEmptyWithPendingToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)

	writePendingToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 0, time.UTC),
	)

	svc := NewService(
		dir, "testnet", lnd.Signer,
		&mockStaticAddressManager{
			params: addrParams,
		},
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
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		params:        addrParams,
		currentHeight: 654,
	}

	tokenCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 123, time.UTC,
	)
	tokenID := writePaidToken(t, dir, 1, tokenCreatedAt)

	svc := NewService(dir, "testnet", lnd.Signer, staticMgr)

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
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		params: addrParams,
	}

	writePaidToken(
		t, dir, 1, time.Date(2026, time.April, 14, 9, 30, 1, 123, time.UTC),
	)

	svc := NewService(dir, "testnet", lnd.Signer, staticMgr)

	backupFile, err := svc.WriteBackup(ctx)
	require.NoError(t, err)

	key, err := svc.deriveEncryptionKey(ctx)
	require.NoError(t, err)

	payload, err := readBackupPayload(key, backupFile)
	require.NoError(t, err)
	require.NotNil(t, payload.StaticAddress)

	clientPubKey, err := btcec.ParsePubKey(
		payload.StaticAddress.ClientPubKey,
	)
	require.NoError(t, err)
	serverPubKey, err := btcec.ParsePubKey(
		payload.StaticAddress.ServerPubKey,
	)
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
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		params: addrParams,
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

	svc := NewService(dir, "testnet", lnd.Signer, staticMgr)

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
		schnorr.SerializePubKey(reconstructed.TaprootKey), lnd.ChainParams,
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
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		params: addrParams,
	}

	tokenCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 0, time.UTC,
	)
	tokenID := writePaidToken(t, dir, 2, tokenCreatedAt)

	svc := NewService(dir, "testnet", lnd.Signer, staticMgr)

	firstBackup, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Equal(
		t, backupFilePath(dir, tokenID, tokenCreatedAt.UnixNano()),
		firstBackup,
	)

	secondBackup, err := svc.WriteBackup(context.Background())
	require.NoError(t, err)
	require.Empty(t, secondBackup)
	require.Equal(t, []string{firstBackup}, listBackupFiles(t, dir))
}

// TestWriteBackupIgnoresInvalidSameTokenBackup verifies that a corrupt file
// with the active token ID in its name does not suppress creation of a valid
// backup.
func TestWriteBackupIgnoresInvalidSameTokenBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lnd := testutils.NewMockLnd()
	addrParams := makeStaticAddressParams(
		t, lnd, 7, defaultBackupServerPubkey, 144, 321,
	)
	staticMgr := &mockStaticAddressManager{
		params: addrParams,
	}

	tokenCreatedAt := time.Date(
		2026, time.April, 14, 9, 30, 1, 0, time.UTC,
	)
	tokenID := writePaidToken(t, dir, 3, tokenCreatedAt)

	backupPath := backupFilePath(dir, tokenID, tokenCreatedAt.UnixNano())
	err := os.WriteFile(backupPath, []byte("corrupt backup"), 0600)
	require.NoError(t, err)

	svc := NewService(dir, "testnet", lnd.Signer, staticMgr)
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

var defaultBackupServerPubkey = func() *btcec.PublicKey {
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

func testBackupKey(seed byte) [32]byte {
	var key [32]byte
	for idx := range key {
		key[idx] = seed
	}

	return key
}

type mockStaticAddressManager struct {
	params        *staticaddrscript.Parameters
	currentHeight int32
	getParamsErr  error
}

func (m *mockStaticAddressManager) GetStaticAddressParameters(
	context.Context) (*staticaddrscript.Parameters, error) {

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

func makeStaticAddressParams(t *testing.T, lnd *testutils.LndMockServices,
	index uint32, serverPubKey *btcec.PublicKey, expiry uint32,
	initiationHeight int32) *staticaddrscript.Parameters {

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

	return &staticaddrscript.Parameters{
		ClientPubkey:     keyDesc.PubKey,
		ServerPubkey:     serverPubKey,
		Expiry:           expiry,
		PkScript:         pkScript,
		KeyLocator:       keyDesc.KeyLocator,
		ProtocolVersion:  staticaddrversion.ProtocolVersion_V0,
		InitiationHeight: initiationHeight,
	}
}

func cloneAddressParameters(
	params *staticaddrscript.Parameters) *staticaddrscript.Parameters {

	if params == nil {
		return nil
	}

	return &staticaddrscript.Parameters{
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
		[]byte("loop-backup-test-root-key"),
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
