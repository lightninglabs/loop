package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/address"
	staticaddrscript "github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"golang.org/x/crypto/nacl/secretbox"
	"gopkg.in/macaroon.v2"
)

const (
	backupVersion = 1

	backupBaseName = "L402_backup"

	backupFileExt = ".enc"

	paidTokenFileName = "l402.token"
)

// backupKeyLocator identifies the lnd key used only for deriving the local
// backup encryption key. The encrypted backup stays tied to the same lnd seed
// material without adding a separate user-managed password.
var backupKeyLocator = keychain.KeyLocator{
	Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
	Index:  0,
}

// backupMagic prefixes encrypted backup files so corrupt or unrelated files can
// be rejected before attempting to unmarshal JSON payloads.
var backupMagic = []byte("loopbak1")

// StaticAddressManager is the subset of static-address behavior required for
// creating backups.
type StaticAddressManager interface {
	// GetStaticAddressParameters returns the concrete legacy static address
	// row that is paired with the current paid L402 generation.
	GetStaticAddressParameters(context.Context) (
		*staticaddrscript.Parameters, error)

	// CurrentHeight returns the manager's current chain height, which is
	// stored as the future multi-address scan floor for this generation.
	CurrentHeight() int32
}

// Service creates encrypted local backups for Loop static-address and L402
// state.
type Service struct {
	dataDir              string
	network              string
	signer               lndclient.SignerClient
	staticAddressManager StaticAddressManager
}

type backupPayload struct {
	Version            uint32                `json:"version"`
	Network            string                `json:"network"`
	L402TokenID        string                `json:"l402_token_id"`
	L402TokenCreatedAt int64                 `json:"l402_token_created_at"`
	StaticAddress      *staticAddressBackup  `json:"static_address,omitempty"`
	TokenFiles         []*l402TokenFileEntry `json:"token_files,omitempty"`
}

// staticAddressBackup contains the legacy single-address data that can be
// restored directly by future recovery code, plus the stable per-L402
// multi-address/change branch fields future multi-address recovery will scan
// from.
type staticAddressBackup struct {
	ProtocolVersion         uint32 `json:"protocol_version"`
	ClientPubKey            []byte `json:"client_pubkey,omitempty"`
	ServerPubKey            []byte `json:"server_pubkey"`
	Expiry                  uint32 `json:"expiry"`
	LegacyClientKeyFamily   int32  `json:"legacy_client_key_family,omitempty"`
	MainKeyFamily           int32  `json:"main_key_family"`
	ChangeKeyFamily         int32  `json:"change_key_family"`
	LegacyFirstHeight       int32  `json:"legacy_first_height,omitempty"`
	MultiAddressFirstHeight int32  `json:"multi_address_first_height,omitempty"`
}

type l402TokenFileEntry struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

type currentTokenState struct {
	TokenID        string
	TokenCreatedAt int64
	TokenFiles     []*l402TokenFileEntry
}

type paidTokenMetadata struct {
	tokenID        string
	tokenCreatedAt int64
}

type backupFileDetails struct {
	tokenID        string
	titleTimestamp int64
}

// NewService constructs a backup service for a specific loop network data
// directory.
func NewService(dataDir, network string, signer lndclient.SignerClient,
	staticAddressManager StaticAddressManager) *Service {

	return &Service{
		dataDir:              dataDir,
		network:              network,
		signer:               signer,
		staticAddressManager: staticAddressManager,
	}
}

// WriteBackup writes an encrypted backup file for the current paid-L402 /
// static-address generation. It returns an empty path when there is no complete
// generation yet, or when the current L402 already has an immutable backup on
// disk.
func (s *Service) WriteBackup(ctx context.Context) (string, error) {
	// A backup is immutable and generation-based, so first collect enough
	// state to prove the current generation is complete: one paid L402 token
	// plus one concrete static address bound to that token.
	payload, hasState, err := s.buildPayload(ctx)
	if err != nil || !hasState {
		return "", err
	}

	// We need the derived key before checking for existing backups because a
	// filename match alone is not enough. A stale or corrupt file with the same
	// token ID must not suppress writing a valid backup.
	key, err := s.deriveEncryptionKey(ctx)
	if err != nil {
		return "", err
	}

	// If a valid backup for the exact token creation time already exists, the
	// generation is already protected and must not be rewritten.
	if backupFile, err := findValidBackupFileForToken(
		s.dataDir, key, s.network, payload.L402TokenID,
		payload.L402TokenCreatedAt,
	); err != nil {
		return "", err
	} else if backupFile != "" {
		return "", nil
	}

	fileName := backupFilePath(
		s.dataDir, payload.L402TokenID, payload.L402TokenCreatedAt,
	)

	// The plaintext is never written to disk. It is marshaled in memory,
	// encrypted with the lnd-derived key, then atomically installed.
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	encrypted, err := encryptBackupPayload(key, plaintext)
	if err != nil {
		return "", err
	}

	err = writeFileAtomically(fileName, encrypted)
	if err != nil {
		return "", err
	}

	return fileName, nil
}

func (p *backupPayload) validateNetwork(currentNetwork string) error {
	switch {
	case p.Version != backupVersion:
		return fmt.Errorf("unsupported backup version %d", p.Version)

	case p.Network == "":
		return fmt.Errorf("backup file is missing a network")

	case p.L402TokenID == "":
		return fmt.Errorf("backup file is missing an L402 token ID")

	case p.Network != currentNetwork:
		return fmt.Errorf("backup file network %s does not match "+
			"daemon network %s", p.Network, currentNetwork)
	}

	return nil
}

func (p *backupPayload) validateCompleteGeneration(
	fileDetails *backupFileDetails) error {

	// When the caller knows the filename metadata, require it to match the
	// payload. This keeps the immutable filename and encrypted contents bound
	// to the same L402 generation.
	if fileDetails != nil {
		if p.L402TokenID != fileDetails.tokenID {
			return fmt.Errorf("backup file token ID %s does not match "+
				"payload token ID %s", fileDetails.tokenID,
				p.L402TokenID)
		}

		if p.L402TokenCreatedAt != fileDetails.titleTimestamp {
			return fmt.Errorf("backup file timestamp %d does not "+
				"match payload L402 creation time %d",
				fileDetails.titleTimestamp, p.L402TokenCreatedAt)
		}
	}

	if len(p.TokenFiles) == 0 {
		return fmt.Errorf("backup file is missing paid L402 token data")
	}

	if p.StaticAddress == nil {
		return fmt.Errorf("backup file is missing static address " +
			"parameters")
	}

	// The raw token file is the source of truth for the paid L402. Decode its
	// metadata and make sure it matches the generation named by the payload.
	metadata, err := validatePaidTokenFiles(p.TokenFiles)
	if err != nil {
		return err
	}

	if metadata.tokenID != p.L402TokenID {
		return fmt.Errorf("backup L402 token ID %s does not match "+
			"payload token ID %s", metadata.tokenID, p.L402TokenID)
	}

	if metadata.tokenCreatedAt != p.L402TokenCreatedAt {
		return fmt.Errorf("backup L402 token creation time %d does "+
			"not match payload creation time %d",
			metadata.tokenCreatedAt, p.L402TokenCreatedAt)
	}

	return nil
}

func (s *Service) buildPayload(ctx context.Context) (*backupPayload, bool,
	error) {

	// Backups are only meaningful after the token payment completed. Pending
	// L402 tokens can still change and do not define an immutable generation.
	tokenState, err := s.currentPaidToken()
	if err != nil {
		return nil, false, err
	}
	if tokenState == nil || s.staticAddressManager == nil {
		return nil, false, nil
	}

	payload := &backupPayload{
		Version:            backupVersion,
		Network:            s.network,
		L402TokenID:        tokenState.TokenID,
		L402TokenCreatedAt: tokenState.TokenCreatedAt,
		TokenFiles:         tokenState.TokenFiles,
	}

	// The current static-address row supplies the legacy concrete address. The
	// same payload also stores the deterministic families and scan floor future
	// multi-address recovery will use without rewriting this backup.
	addrParams, err := s.staticAddressManager.GetStaticAddressParameters(ctx)
	switch {
	case err == nil:
		multiAddressFirstHeight := s.staticAddressManager.CurrentHeight()
		if multiAddressFirstHeight <= 0 {
			return nil, false, fmt.Errorf(
				"invalid multi-address first height %d",
				multiAddressFirstHeight,
			)
		}

		payload.StaticAddress = &staticAddressBackup{
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
			MultiAddressFirstHeight: multiAddressFirstHeight,
		}

	case errors.Is(err, address.ErrNoStaticAddress):
		// The current L402 does not have a complete static-address generation
		// yet, so there is nothing immutable to back up.
		return nil, false, nil

	default:
		return nil, false, err
	}

	hasState := payload.StaticAddress != nil && len(payload.TokenFiles) > 0

	return payload, hasState, nil
}

func (s *Service) currentPaidToken() (*currentTokenState, error) {
	tokenStore, err := l402.NewFileStore(s.dataDir)
	if err != nil {
		return nil, err
	}

	token, err := tokenStore.CurrentToken()
	switch {
	case err == nil:

	case errors.Is(err, l402.ErrNoToken):
		return nil, nil

	default:
		return nil, err
	}

	// Only fully paid tokens define an immutable generation.
	if token.Preimage == (lntypes.Preimage{}) {
		return nil, nil
	}

	// Preserve the exact token file bytes instead of reserializing the token.
	// That keeps future restore code compatible with Aperture's token-store
	// format.
	tokenID, err := decodeTokenID(token)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(s.dataDir, paidTokenFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	return &currentTokenState{
		TokenID:        tokenID,
		TokenCreatedAt: token.TimeCreated.UnixNano(),
		TokenFiles: []*l402TokenFileEntry{{
			Name: paidTokenFileName,
			Data: data,
		}},
	}, nil
}

func decodeTokenID(token *l402.Token) (string, error) {
	identifier, err := l402.DecodeIdentifier(
		bytes.NewReader(token.BaseMacaroon().Id()),
	)
	if err != nil {
		return "", err
	}

	return identifier.TokenID.String(), nil
}

func backupFilePath(dataDir, tokenID string, tokenCreatedAt int64) string {
	return filepath.Join(dataDir, backupFileName(tokenID, tokenCreatedAt))
}

func backupFileName(tokenID string, tokenCreatedAt int64) string {
	return fmt.Sprintf(
		"%s_%019d_%s%s", backupBaseName, tokenCreatedAt, tokenID,
		backupFileExt,
	)
}

func backupFileTokenID(name string) (string, bool) {
	details, ok := parseBackupFileName(name)
	if !ok {
		return "", false
	}

	return details.tokenID, true
}

func parseBackupFileName(name string) (*backupFileDetails, bool) {
	if !strings.HasPrefix(name, backupBaseName+"_") ||
		!strings.HasSuffix(name, backupFileExt) {

		return nil, false
	}

	remainder := strings.TrimSuffix(
		strings.TrimPrefix(name, backupBaseName+"_"), backupFileExt,
	)

	parts := strings.SplitN(remainder, "_", 2)
	if len(parts) != 2 {
		return nil, false
	}

	titleTimestamp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, false
	}
	tokenID := parts[1]

	_, err = l402.MakeIDFromString(tokenID)
	if err != nil {
		return nil, false
	}

	return &backupFileDetails{
		tokenID:        tokenID,
		titleTimestamp: titleTimestamp,
	}, true
}

func findValidBackupFileForToken(dataDir string, key [32]byte, network,
	tokenID string, tokenCreatedAt int64) (string, error) {

	dirEntries, err := os.ReadDir(dataDir)
	if err != nil {
		return "", err
	}

	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}

		// Search by token ID first, then decrypt to verify the candidate is a
		// valid backup for this exact paid-token generation.
		details, ok := parseBackupFileName(entry.Name())
		if !ok || details.tokenID != tokenID {
			continue
		}

		path := filepath.Join(dataDir, entry.Name())
		payload, err := readBackupPayload(key, path)
		if err != nil {
			// Invalid same-token files are ignored so WriteBackup can replace a
			// corrupt placeholder with a real backup.
			continue
		}

		err = payload.validateNetwork(network)
		if err != nil {
			continue
		}

		err = payload.validateCompleteGeneration(details)
		if err != nil {
			continue
		}

		if payload.L402TokenCreatedAt != tokenCreatedAt {
			continue
		}

		return path, nil
	}

	return "", nil
}

func readBackupPayload(key [32]byte, path string) (*backupPayload, error) {
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	plaintext, err := decryptBackupPayload(key, ciphertext)
	if err != nil {
		return nil, err
	}

	var payload backupPayload
	err = json.Unmarshal(plaintext, &payload)
	if err != nil {
		return nil, err
	}

	return &payload, nil
}

func validatePaidTokenFiles(
	backupFiles []*l402TokenFileEntry) (*paidTokenMetadata, error) {

	var paidTokenData []byte
	for _, file := range backupFiles {
		if !isTokenFileName(file.Name) {
			return nil, fmt.Errorf("unexpected token file name %q",
				file.Name)
		}

		if paidTokenData != nil {
			return nil, fmt.Errorf("backup contains duplicate paid " +
				"L402 token data")
		}

		paidTokenData = file.Data
	}

	if paidTokenData == nil {
		return nil, fmt.Errorf("backup file is missing paid L402 token data")
	}

	return parsePaidTokenMetadata(paidTokenData)
}

func parsePaidTokenMetadata(data []byte) (*paidTokenMetadata, error) {
	r := bytes.NewReader(data)

	var macLen uint32
	err := binary.Read(r, binary.BigEndian, &macLen)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token macaroon "+
			"length: %w", err)
	}

	if uint64(macLen) > uint64(r.Len()) {
		return nil, fmt.Errorf("invalid L402 token macaroon length")
	}

	macBytes := make([]byte, macLen)
	err = binary.Read(r, binary.BigEndian, &macBytes)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token macaroon: %w",
			err)
	}

	var paymentHash lntypes.Hash
	err = binary.Read(r, binary.BigEndian, &paymentHash)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token payment hash: %w",
			err)
	}

	var preimage lntypes.Preimage
	err = binary.Read(r, binary.BigEndian, &preimage)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token preimage: %w",
			err)
	}

	if preimage == (lntypes.Preimage{}) {
		return nil, fmt.Errorf("backup L402 token is not paid")
	}

	var amountPaid uint64
	err = binary.Read(r, binary.BigEndian, &amountPaid)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token amount: %w", err)
	}

	var routingFeePaid uint64
	err = binary.Read(r, binary.BigEndian, &routingFeePaid)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token routing fee: %w",
			err)
	}

	var tokenCreatedAt int64
	err = binary.Read(r, binary.BigEndian, &tokenCreatedAt)
	if err != nil {
		return nil, fmt.Errorf("unable to read L402 token creation time: %w",
			err)
	}

	mac := &macaroon.Macaroon{}
	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal L402 token "+
			"macaroon: %w", err)
	}

	identifier, err := l402.DecodeIdentifier(bytes.NewReader(mac.Id()))
	if err != nil {
		return nil, fmt.Errorf("unable to decode L402 token ID: %w", err)
	}

	return &paidTokenMetadata{
		tokenID:        identifier.TokenID.String(),
		tokenCreatedAt: tokenCreatedAt,
	}, nil
}

func (s *Service) deriveEncryptionKey(ctx context.Context) ([32]byte, error) {
	return s.signer.DeriveSharedKey(
		ctx, lndclient.SharedKeyNUMS, &backupKeyLocator,
	)
}

func encryptBackupPayload(key [32]byte, plaintext []byte) ([]byte, error) {
	var nonce [24]byte
	_, err := rand.Read(nonce[:])
	if err != nil {
		return nil, err
	}

	cipherText := secretbox.Seal(nil, plaintext, &nonce, &key)
	encoded := make([]byte, 0, len(backupMagic)+len(nonce)+len(cipherText))
	encoded = append(encoded, backupMagic...)
	encoded = append(encoded, nonce[:]...)
	encoded = append(encoded, cipherText...)

	return encoded, nil
}

func decryptBackupPayload(key [32]byte, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < len(backupMagic)+24 {
		return nil, fmt.Errorf("backup file is too short")
	}
	if !bytes.Equal(ciphertext[:len(backupMagic)], backupMagic) {
		return nil, fmt.Errorf("backup file has an unknown format")
	}

	var nonce [24]byte
	copy(nonce[:], ciphertext[len(backupMagic):len(backupMagic)+24])

	plaintext, ok := secretbox.Open(
		nil, ciphertext[len(backupMagic)+24:], &nonce, &key,
	)
	if !ok {
		return nil, fmt.Errorf("unable to decrypt backup file")
	}

	return plaintext, nil
}

func writeFileAtomically(path string, data []byte) error {
	tempPath := path + ".tmp"

	// Write private files through a temp path so a crash cannot leave a
	// partially written backup at the final name.
	file, err := os.OpenFile(
		tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600,
	)
	if err != nil {
		return err
	}

	_, err = file.Write(data)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)

		return err
	}

	err = file.Sync()
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)

		return err
	}

	err = file.Close()
	if err != nil {
		_ = os.Remove(tempPath)

		return err
	}

	err = os.Rename(tempPath, path)
	if err != nil {
		_ = os.Remove(tempPath)
	}

	return err
}

func isTokenFileName(name string) bool {
	return filepath.Base(name) == name && name == paidTokenFileName
}
