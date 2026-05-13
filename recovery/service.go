package recovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	staticaddrscript "github.com/lightninglabs/loop/staticaddr/script"
	staticaddrversion "github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lntypes"
	"golang.org/x/crypto/nacl/secretbox"
	"gopkg.in/macaroon.v2"
)

const (
	backupVersion = 1

	backupBaseName = "L402_backup"

	backupFileExt = ".enc"

	// backupKeyScanLimit is the highest legacy client-family child index
	// scanned when reconstructing the static-address client key.
	backupKeyScanLimit = 20

	paidTokenFileName = "l402.token"

	pendingTokenFileName = "l402.token.pending"
)

// DefaultMultiAddressScanGap is the default number of consecutive unused
// multi-address children to scan before restore stops. It is intentionally a
// package-level default so it can be wired to configuration later.
var DefaultMultiAddressScanGap = 20

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
// creating and restoring recovery backups.
type StaticAddressManager interface {
	// GetStaticAddressParameters returns the concrete legacy static address
	// row that is paired with the current paid L402 generation.
	GetStaticAddressParameters(context.Context) (*address.Parameters, error)

	// RestoreAddress recreates that concrete address row and imports its
	// tapscript into lnd. The bool reports whether local static-address state
	// changed, allowing restore responses to stay idempotent.
	RestoreAddress(context.Context,
		*address.Parameters) (*btcutil.AddressTaproot, bool, error)

	// CurrentHeight returns the manager's current chain height, which is
	// stored as the future multi-address scan floor for this generation.
	CurrentHeight() int32
}

// DepositManager is the subset of deposit-manager behavior required to
// reconcile deposits after restore.
type DepositManager interface {
	// ReconcileDeposits asks lnd for wallet-visible static-address UTXOs and
	// rebuilds deposit FSM state for anything not already tracked.
	ReconcileDeposits(context.Context) (int, error)

	// RecoverDeposit verifies one on-chain static-address output and restores
	// the corresponding local deposit/address state.
	RecoverDeposit(context.Context,
		*deposit.RecoveryRequest) (*deposit.RecoveryResult, error)
}

// RecoverResult describes the outcome of a restore attempt.
type RecoverResult struct {
	// BackupFile is the encrypted backup file that was selected and
	// restored.
	BackupFile string

	// StaticAddress is the taproot address reconstructed from the backup.
	StaticAddress string

	// RestoredStaticAddress reports whether restore created new local
	// static-address state. It is false for an idempotent restore where the
	// same address was already present.
	RestoredStaticAddress bool

	// RestoredL402 reports whether restore wrote paid L402 token material.
	// It is false when the same paid token already existed locally.
	RestoredL402 bool

	// NumDepositsFound is the number of wallet-visible static-address
	// deposits discovered by the post-restore reconciliation pass.
	NumDepositsFound int

	// DepositReconciliationError contains a non-fatal reconciliation error.
	// Token and address restore can still succeed even if deposit discovery
	// needs to be retried later.
	DepositReconciliationError string
}

// RecoverDepositRequest describes one static-address deposit to recover from
// on-chain data supplied by the caller.
type RecoverDepositRequest struct {
	TxID        string
	VOut        uint32
	HeightHint  int32
	PkScriptHex string
}

// RecoverDepositResult describes the recovered deposit and the static-address
// child that matched the supplied pkScript.
type RecoverDepositResult struct {
	OutPoint           string
	Value              btcutil.Amount
	ConfirmationHeight int64
	ClientKeyFamily    int32
	ClientKeyIndex     uint32
	StaticAddress      string
	RecoveredAddress   bool
	RecoveredDeposit   bool
	DepositID          []byte
}

// Service coordinates creation and restoration of encrypted local recovery
// backups for Loop static-address and L402 state.
type Service struct {
	dataDir              string
	network              string
	signer               lndclient.SignerClient
	walletKit            lndclient.WalletKitClient
	staticAddressManager StaticAddressManager
	depositManager       DepositManager
	multiAddressScanGap  int
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
// restored directly today, plus the stable per-L402 multi-address/change branch
// fields future multi-address recovery will scan from.
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

type tokenRestoreResult struct {
	restored     bool
	writtenPaths []string
}

type paidTokenMetadata struct {
	tokenID        string
	tokenCreatedAt int64
}

type backupFileDetails struct {
	tokenID        string
	titleTimestamp int64
}

// NewService constructs a recovery service for a specific loop network data
// directory.
func NewService(dataDir, network string, signer lndclient.SignerClient,
	walletKit lndclient.WalletKitClient,
	staticAddressManager StaticAddressManager,
	depositManager DepositManager) *Service {

	return &Service{
		dataDir:              dataDir,
		network:              network,
		signer:               signer,
		walletKit:            walletKit,
		staticAddressManager: staticAddressManager,
		depositManager:       depositManager,
		multiAddressScanGap:  DefaultMultiAddressScanGap,
	}
}

// WriteBackup writes an encrypted backup file for the current paid-L402 /
// static-address generation. It returns an empty path when there is no
// complete recoverable generation yet, or when the current L402 already has an
// immutable backup on disk.
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

// RestoreLatestOnFreshInstall restores the most recent local backup only when
// loopd has no local token files or static-address state yet. It returns the
// restore result together with a boolean indicating whether a restore was
// actually performed.
func (s *Service) RestoreLatestOnFreshInstall(ctx context.Context) (
	*RecoverResult, bool, error) {

	// Automatic startup restore is intentionally limited to a truly fresh Loop
	// directory so it never overwrites an existing token or static address.
	freshInstall, err := s.isFreshInstall(ctx)
	if err != nil {
		return nil, false, err
	}
	if !freshInstall {
		return nil, false, nil
	}

	result, err := s.Restore(ctx, "")
	switch {
	case err == nil:
		return result, true, nil

	case errors.Is(err, os.ErrNotExist):
		// A fresh install without backup files should continue normal startup
		// initialization and create a new paid-L402/static-address generation.
		return nil, false, nil

	default:
		return nil, false, err
	}
}

// Restore restores the local static-address and L402 state from an encrypted
// backup file. If backupFile is empty, the most recent immutable generation
// backup in the active network directory is used.
func (s *Service) Restore(ctx context.Context, backupFile string) (
	*RecoverResult, error) {

	// Restores use the same lnd-derived key as backup creation. This makes the
	// backup useful only with the original lnd seed material.
	key, err := s.deriveEncryptionKey(ctx)
	if err != nil {
		return nil, err
	}

	// An explicit path is validated for backup filename shape. An empty path
	// means "scan the active network directory and pick the latest candidate".
	fileName, err := s.resolveBackupFile(key, backupFile)
	if err != nil {
		return nil, err
	}

	// Decrypt and validate the complete backup before touching local token or
	// static-address state.
	encrypted, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}

	plaintext, err := decryptBackupPayload(key, encrypted)
	if err != nil {
		return nil, err
	}

	var payload backupPayload
	err = json.Unmarshal(plaintext, &payload)
	if err != nil {
		return nil, err
	}

	err = payload.validateNetwork(s.network)
	if err != nil {
		return nil, err
	}

	fileDetails, _ := parseBackupFileName(filepath.Base(fileName))
	err = payload.validateRecoverableGeneration(fileDetails)
	if err != nil {
		return nil, err
	}

	result := &RecoverResult{
		BackupFile: fileName,
	}

	var restoreParams *address.Parameters
	if payload.StaticAddress != nil {
		// Reconstruct and validate the concrete static-address parameters up
		// front. If the backed-up key cannot be derived from lnd, no token file
		// is written.
		restoreParams, err = s.prepareStaticAddressRestore(
			ctx, payload.StaticAddress,
		)
		if err != nil {
			return nil, err
		}
	}

	tokenRestore, err := s.restoreTokenFiles(payload.TokenFiles)
	if err != nil {
		return nil, err
	}
	result.RestoredL402 = tokenRestore.restored

	if restoreParams != nil {
		addr, restored, err := s.restorePreparedStaticAddress(
			ctx, restoreParams,
		)
		if err != nil {
			// Token files are restored before the address so the local L402 is
			// available to code that validates the generation. If the address
			// restore fails, remove only files written by this restore attempt.
			rollbackErr := cleanupRestoredTokenFiles(
				tokenRestore.writtenPaths,
			)
			if rollbackErr != nil {
				return nil, fmt.Errorf("unable to restore static "+
					"address: %w (also failed to roll back "+
					"restored token files: %v)", err,
					rollbackErr)
			}

			return nil, err
		}

		result.StaticAddress = addr
		result.RestoredStaticAddress = restored

		restored, err = s.restoreMultiAddressBranches(
			ctx, payload.StaticAddress, restoreParams.ServerPubkey,
		)
		if err != nil {
			rollbackErr := cleanupRestoredTokenFiles(
				tokenRestore.writtenPaths,
			)
			if rollbackErr != nil {
				return nil, fmt.Errorf("unable to restore multi-"+
					"address static addresses: %w (also failed "+
					"to roll back restored token files: %v)",
					err, rollbackErr)
			}

			return nil, err
		}

		result.RestoredStaticAddress = result.RestoredStaticAddress ||
			restored
	}

	if payload.StaticAddress != nil && s.depositManager != nil {
		// Deposit history is not serialized in the backup. After the address
		// is restored, reconciliation asks lnd for current UTXOs and recreates
		// missing deposit FSMs best-effort.
		numDeposits, err := s.depositManager.ReconcileDeposits(ctx)
		if err != nil {
			result.DepositReconciliationError = err.Error()
		} else {
			result.NumDepositsFound = numDeposits
		}
	}

	return result, nil
}

// RecoverDeposit restores one static-address deposit from caller-supplied
// on-chain coordinates. The recovery package only validates the RPC-shaped
// request and delegates chain/address/deposit work to the deposit manager.
func (s *Service) RecoverDeposit(ctx context.Context,
	req *RecoverDepositRequest) (*RecoverDepositResult, error) {

	if s.depositManager == nil {
		return nil, fmt.Errorf("deposit recovery is unavailable")
	}

	depositReq, err := parseRecoverDepositRequest(req)
	if err != nil {
		return nil, err
	}

	result, err := s.depositManager.RecoverDeposit(ctx, depositReq)
	if err != nil {
		return nil, err
	}

	if result.AddressParams == nil {
		return nil, fmt.Errorf("recovered deposit missing address " +
			"parameters")
	}

	return &RecoverDepositResult{
		OutPoint:           result.OutPoint.String(),
		Value:              result.Value,
		ConfirmationHeight: result.ConfirmationHeight,
		ClientKeyFamily: int32(
			result.AddressParams.KeyLocator.Family,
		),
		ClientKeyIndex:   result.AddressParams.KeyLocator.Index,
		StaticAddress:    result.StaticAddress,
		RecoveredAddress: result.RecoveredAddress,
		RecoveredDeposit: result.RecoveredDeposit,
		DepositID:        result.DepositID[:],
	}, nil
}

// parseRecoverDepositRequest validates the service-level recovery request and
// converts it into the deposit manager's strongly typed request.
func parseRecoverDepositRequest(req *RecoverDepositRequest) (
	*deposit.RecoveryRequest, error) {

	if req == nil {
		return nil, fmt.Errorf("missing recover deposit request")
	}
	if req.TxID == "" {
		return nil, fmt.Errorf("missing txid")
	}
	if req.HeightHint <= 0 {
		return nil, fmt.Errorf("height_hint must be positive")
	}
	if req.PkScriptHex == "" {
		return nil, fmt.Errorf("missing pkscript_hex")
	}

	txid, err := chainhash.NewHashFromStr(req.TxID)
	if err != nil {
		return nil, fmt.Errorf("invalid txid: %w", err)
	}

	pkScript, err := hex.DecodeString(
		strings.TrimPrefix(req.PkScriptHex, "0x"),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid pkscript_hex: %w", err)
	}
	if !isP2TRPkScript(pkScript) {
		return nil, fmt.Errorf("pkscript_hex must encode a P2TR " +
			"pkScript")
	}

	return &deposit.RecoveryRequest{
		TxID:       *txid,
		VOut:       req.VOut,
		HeightHint: req.HeightHint,
		PkScript:   pkScript,
	}, nil
}

// isP2TRPkScript reports whether the pkScript is a v1 witness program with a
// 32-byte Taproot output key.
func isP2TRPkScript(pkScript []byte) bool {
	return len(pkScript) == 34 &&
		pkScript[0] == txscript.OP_1 &&
		pkScript[1] == 32
}

func (p *backupPayload) validateNetwork(currentNetwork string) error {
	// These checks validate the envelope-level metadata before any generation
	// contents are trusted.
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

func (p *backupPayload) validateRecoverableGeneration(
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

	// The current static-address row supplies the legacy concrete address that
	// this implementation can restore today. The same payload also stores the
	// deterministic families and scan floor future multi-address recovery will
	// use without rewriting this backup.
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
		// The current L402 does not have a complete static-address
		// generation yet, so there is nothing immutable to back up.
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

	// Only fully paid tokens define an immutable recoverable generation.
	if token.Preimage == (lntypes.Preimage{}) {
		return nil, nil
	}

	// Preserve the exact token file bytes instead of reserializing the token.
	// That keeps restore compatible with Aperture's token-store format.
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

func (s *Service) readTokenFiles() ([]*l402TokenFileEntry, error) {
	var files []*l402TokenFileEntry
	for _, name := range []string{paidTokenFileName, pendingTokenFileName} {
		path := filepath.Join(s.dataDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, err
		}

		files = append(files, &l402TokenFileEntry{
			Name: name,
			Data: data,
		})
	}

	return files, nil
}

func (s *Service) resolveBackupFile(key [32]byte, backupFile string) (string,
	error) {

	if backupFile != "" {
		// Explicit restore still requires the immutable backup filename format
		// so payload validation can compare encrypted contents to the title.
		return validateBackupFilePath(backupFile)
	}

	return latestBackupFilePath(s.dataDir, key, s.network)
}

func validateBackupFilePath(path string) (string, error) {
	cleanPath := lncfg.CleanAndExpandPath(path)
	if _, ok := parseBackupFileName(filepath.Base(cleanPath)); !ok {
		return "", fmt.Errorf("invalid backup file path %q", path)
	}

	return cleanPath, nil
}

type backupSelection struct {
	fileName      string
	tokenID       string
	sortTimestamp int64
}

func latestBackupFilePath(dataDir string, key [32]byte,
	network string) (string, error) {

	dirEntries, err := os.ReadDir(dataDir)
	if err != nil {
		return "", err
	}

	var (
		latestSelection *backupSelection
		firstErr        error
	)

	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}

		// Only files using the immutable backup name shape participate in
		// automatic selection; everything else in the data dir is ignored.
		details, ok := parseBackupFileName(entry.Name())
		if !ok {
			continue
		}

		path := filepath.Join(dataDir, entry.Name())
		payload, err := readBackupPayload(key, path)
		if err != nil {
			// Keep scanning so one corrupt or wrong-key backup does not hide a
			// valid older backup.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		err = payload.validateNetwork(network)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		err = payload.validateRecoverableGeneration(details)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		selection := &backupSelection{
			fileName:      path,
			tokenID:       details.tokenID,
			sortTimestamp: details.titleTimestamp,
		}

		if latestSelection == nil ||
			selection.sortTimestamp > latestSelection.sortTimestamp ||
			(selection.sortTimestamp == latestSelection.sortTimestamp &&
				selection.tokenID > latestSelection.tokenID) {

			latestSelection = selection
		}
	}

	if latestSelection != nil {
		return latestSelection.fileName, nil
	}
	if firstErr != nil {
		return "", firstErr
	}

	return "", os.ErrNotExist
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

		err = payload.validateRecoverableGeneration(details)
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

func readBackupPlaintext(key [32]byte, path string) ([]byte, error) {
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return decryptBackupPayload(key, ciphertext)
}

func readBackupPayload(key [32]byte, path string) (*backupPayload, error) {
	plaintext, err := readBackupPlaintext(key, path)
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

func (s *Service) prepareStaticAddressRestore(ctx context.Context,
	backup *staticAddressBackup) (*address.Parameters, error) {

	// This phase only validates and reconstructs parameters. The token store
	// and static-address DB are not modified until all backup fields prove
	// internally consistent and derivable from lnd.
	if s.staticAddressManager == nil {
		return nil, fmt.Errorf("static address restore is unavailable")
	}

	if !staticaddrversion.AddressProtocolVersion(
		backup.ProtocolVersion,
	).Valid() {

		return nil, fmt.Errorf("invalid static address protocol version %d",
			backup.ProtocolVersion)
	}

	serverPubKey, err := btcec.ParsePubKey(backup.ServerPubKey)
	if err != nil {
		return nil, err
	}

	clientPubKey, locator, err := s.resolveClientKey(ctx, backup)
	if err != nil {
		return nil, err
	}

	// Build the full concrete row, including pkScript, through the same helper
	// used by multi-address recovery so both paths reconstruct scripts in one
	// place. RestoreAddress still derives and verifies the script before it
	// writes or imports anything.
	params, err := newRecoveredStaticAddress(
		backup, serverPubKey, &keychain.KeyDescriptor{
			KeyLocator: locator,
			PubKey:     clientPubKey,
		}, backup.LegacyFirstHeight,
	)
	if err != nil {
		return nil, err
	}

	return params, nil
}

func (s *Service) restoreTokenFiles(
	backupFiles []*l402TokenFileEntry) (*tokenRestoreResult, error) {

	if len(backupFiles) == 0 {
		return &tokenRestoreResult{}, nil
	}

	existingFiles, err := s.readTokenFiles()
	if err != nil {
		return nil, err
	}

	existingByName := make(map[string][]byte, len(existingFiles))
	for _, file := range existingFiles {
		existingByName[file.Name] = file.Data
	}

	// Accept only the expected token-store file. The backup format must not be
	// able to write arbitrary names into the Loop data directory.
	backupByName := make(map[string][]byte, len(backupFiles))
	for _, file := range backupFiles {
		if !isTokenFileName(file.Name) {
			return nil, fmt.Errorf("unexpected token file name %q",
				file.Name)
		}

		backupByName[file.Name] = file.Data
	}

	for name := range existingByName {
		if _, ok := backupByName[name]; ok {
			continue
		}

		return nil, fmt.Errorf("token store already contains "+
			"unexpected file %q", name)
	}

	result := &tokenRestoreResult{}
	for name, data := range backupByName {
		path := filepath.Join(s.dataDir, name)
		if current, ok := existingByName[name]; ok {
			// Restoring the same generation is idempotent, but conflicting
			// local token bytes are never overwritten.
			if !bytes.Equal(current, data) {
				return nil, fmt.Errorf("token file %q already exists "+
					"with different contents", name)
			}

			continue
		}

		err := writeFileAtomically(path, data)
		if err != nil {
			return nil, err
		}
		result.restored = true
		result.writtenPaths = append(result.writtenPaths, path)
	}

	return result, nil
}

func validatePaidTokenFiles(
	backupFiles []*l402TokenFileEntry) (*paidTokenMetadata, error) {

	// Decode the backed-up token file enough to prove it is a paid L402 and to
	// bind its ID/creation time to the rest of the payload.
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
		return nil, fmt.Errorf("unable to read L402 token amount: %w",
			err)
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

func (s *Service) restorePreparedStaticAddress(ctx context.Context,
	params *address.Parameters) (string, bool, error) {

	// The address manager owns persistence and lnd tapscript import ordering.
	// Recovery only supplies already-validated parameters.
	addr, restored, err := s.staticAddressManager.RestoreAddress(
		ctx, params,
	)
	if err != nil {
		return "", false, err
	}

	return addr.String(), restored, nil
}

// restoreMultiAddressBranches scans all backed-up multi-address key families
// against a single cached ListUnspent view. The scan is branch-local and uses a
// rolling gap: every matched child resets the unused-child counter, so sparse
// deposits remain recoverable as long as the distance between hits is below the
// configured scan gap.
func (s *Service) restoreMultiAddressBranches(ctx context.Context,
	backup *staticAddressBackup, serverPubKey *btcec.PublicKey) (bool, error) {

	if backup == nil {
		return false, nil
	}
	if serverPubKey == nil {
		return false, fmt.Errorf("missing static address server pubkey")
	}
	if s.multiAddressScanGap <= 0 {
		return false, fmt.Errorf("invalid multi-address scan gap %d",
			s.multiAddressScanGap)
	}

	families := multiAddressFamilies(backup)
	if len(families) == 0 {
		return false, nil
	}

	walletScripts, err := s.walletUnspentScriptSet(ctx)
	if err != nil {
		return false, err
	}
	if len(walletScripts) == 0 {
		return false, nil
	}

	var restored bool
	for _, family := range families {
		branchRestored, err := s.restoreMultiAddressBranch(
			ctx, backup, serverPubKey, family, walletScripts,
		)
		if err != nil {
			return false, err
		}

		restored = restored || branchRestored
	}

	return restored, nil
}

// multiAddressFamilies returns the non-zero multi-address branch families from
// the backup while preserving their order and suppressing duplicates.
func multiAddressFamilies(backup *staticAddressBackup) []int32 {
	var families []int32
	seen := make(map[int32]struct{}, 2)
	for _, family := range []int32{
		backup.MainKeyFamily,
		backup.ChangeKeyFamily,
	} {
		if family == 0 {
			continue
		}
		if _, ok := seen[family]; ok {
			continue
		}

		seen[family] = struct{}{}
		families = append(families, family)
	}

	return families
}

// walletUnspentScriptSet returns the pkScripts currently visible in lnd's
// wallet. This is intentionally called once per restore before branch scanning;
// scanning then derives candidates in memory and checks them against this set.
// Deposit reconciliation runs afterwards and performs its own ListUnspent pass
// because it needs the active address-manager view and confirmation metadata.
func (s *Service) walletUnspentScriptSet(ctx context.Context) (
	map[string]struct{}, error) {

	// List all wallet-visible UTXOs. This follows the legacy recovery model:
	// recovery reconstructs scripts and matches them against lnd's current
	// wallet view.
	utxos, err := s.walletKit.ListUnspent(ctx, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("unable to list unspent outputs for "+
			"multi-address recovery: %w", err)
	}

	scripts := make(map[string]struct{}, len(utxos))
	for _, utxo := range utxos {
		scripts[string(utxo.PkScript)] = struct{}{}
	}

	return scripts, nil
}

// restoreMultiAddressBranch derives concrete children for one key family until
// it observes multiAddressScanGap consecutive misses. Each hit is restored
// through the address manager so the concrete row is persisted and imported
// before deposit reconciliation discovers matching UTXOs.
func (s *Service) restoreMultiAddressBranch(ctx context.Context,
	backup *staticAddressBackup, serverPubKey *btcec.PublicKey,
	keyFamily int32, walletScripts map[string]struct{}) (bool, error) {

	var restored bool
	unused := 0
	for idx := uint32(0); unused < s.multiAddressScanGap; idx++ {
		params, err := s.deriveMultiAddress(
			ctx, backup, serverPubKey, keyFamily, idx,
		)
		if err != nil {
			return false, err
		}

		if _, ok := walletScripts[string(params.PkScript)]; !ok {
			unused++
			continue
		}

		_, changed, err := s.staticAddressManager.RestoreAddress(
			ctx, params,
		)
		if err != nil {
			return false, err
		}

		restored = restored || changed
		unused = 0
	}

	return restored, nil
}

// deriveMultiAddress reconstructs one concrete receive/change child from the
// backed-up generation root and a child locator.
func (s *Service) deriveMultiAddress(ctx context.Context,
	backup *staticAddressBackup, serverPubKey *btcec.PublicKey,
	keyFamily int32, keyIndex uint32) (*address.Parameters, error) {

	locator := keychain.KeyLocator{
		Family: keychain.KeyFamily(keyFamily),
		Index:  keyIndex,
	}

	clientKey, err := s.walletKit.DeriveKey(ctx, &locator)
	if err != nil {
		return nil, fmt.Errorf("unable to derive multi-address child "+
			"%d:%d: %w", keyFamily, keyIndex, err)
	}

	return newRecoveredStaticAddress(
		backup, serverPubKey, clientKey, backup.MultiAddressFirstHeight,
	)
}

// newRecoveredStaticAddress builds the concrete static-address parameters and
// pkScript for a recovered child key. Legacy recovery uses the same helper as
// multi-address scanning so both paths reconstruct scripts identically.
func newRecoveredStaticAddress(backup *staticAddressBackup,
	serverPubKey *btcec.PublicKey, clientKey *keychain.KeyDescriptor,
	initiationHeight int32) (*address.Parameters, error) {

	staticAddress, err := staticaddrscript.NewStaticAddress(
		input.MuSig2Version100RC2, int64(backup.Expiry),
		clientKey.PubKey, serverPubKey,
	)
	if err != nil {
		return nil, err
	}

	pkScript, err := staticAddress.StaticAddressScript()
	if err != nil {
		return nil, err
	}

	return &address.Parameters{
		ClientPubkey: clientKey.PubKey,
		ServerPubkey: serverPubKey,
		Expiry:       backup.Expiry,
		PkScript:     pkScript,
		KeyLocator:   clientKey.KeyLocator,
		ProtocolVersion: staticaddrversion.AddressProtocolVersion(
			backup.ProtocolVersion,
		),
		InitiationHeight: initiationHeight,
	}, nil
}

func cleanupRestoredTokenFiles(paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	// Only remove files created by this restore attempt. Pre-existing matching
	// files are never included in paths and are therefore left untouched.
	var cleanupErrs []error
	for _, path := range paths {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrs = append(
				cleanupErrs, fmt.Errorf("remove %s: %w", path, err),
			)
		}
	}

	return errors.Join(cleanupErrs...)
}

func (s *Service) resolveClientKey(ctx context.Context,
	backup *staticAddressBackup) (
	*btcec.PublicKey, keychain.KeyLocator, error) {

	if len(backup.ClientPubKey) == 0 {
		return nil, keychain.KeyLocator{}, fmt.Errorf(
			"backup file is missing the static address client pubkey",
		)
	}

	if backup.LegacyClientKeyFamily == 0 {
		return nil, keychain.KeyLocator{}, fmt.Errorf(
			"backup file is missing the legacy static address " +
				"client key family",
		)
	}

	expectedClientPubKey, err := btcec.ParsePubKey(backup.ClientPubKey)
	if err != nil {
		return nil, keychain.KeyLocator{}, err
	}

	// Older backups do not persist the key index. Scan the legacy static
	// address family and accept the child whose pubkey matches the backup.
	for idx := 0; idx <= backupKeyScanLimit; idx++ {
		candidateLocator := keychain.KeyLocator{
			Family: keychain.KeyFamily(backup.LegacyClientKeyFamily),
			Index:  uint32(idx),
		}

		candidateKey, err := s.walletKit.DeriveKey(
			ctx, &candidateLocator,
		)
		if err != nil {
			continue
		}

		if candidateKey.PubKey.IsEqual(expectedClientPubKey) {
			return candidateKey.PubKey, candidateLocator, nil
		}
	}

	return nil, keychain.KeyLocator{}, fmt.Errorf("unable to derive " +
		"static address client key from backup")
}

func (s *Service) deriveEncryptionKey(ctx context.Context) ([32]byte, error) {
	// DeriveSharedKey gives both backup and restore the same symmetric key on
	// any lnd instance restored from the same seed/key material.
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
	// partially written backup or token at the final name.
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

func (s *Service) isFreshInstall(ctx context.Context) (bool, error) {
	// Any local token material means this is not safe for automatic restore:
	// a paid token would be overwritten, and a pending token may still be in
	// the middle of the L402 acquisition flow.
	hasTokenFiles, err := hasAnyLocalTokenFiles(s.dataDir)
	if err != nil || hasTokenFiles {
		return !hasTokenFiles && err == nil, err
	}

	if s.staticAddressManager == nil {
		return true, nil
	}

	// Static-address state also makes the install non-fresh, even if no token
	// file exists, because it may represent a partial local generation.
	_, err = s.staticAddressManager.GetStaticAddressParameters(ctx)
	switch {
	case err == nil:
		return false, nil

	case errors.Is(err, address.ErrNoStaticAddress):
		return true, nil

	default:
		return false, err
	}
}

func hasAnyLocalTokenFiles(dataDir string) (bool, error) {
	for _, name := range []string{paidTokenFileName, pendingTokenFileName} {
		path := filepath.Join(dataDir, name)
		_, err := os.Stat(path)
		switch {
		case err == nil:
			return true, nil

		case errors.Is(err, os.ErrNotExist):
			continue

		default:
			return false, err
		}
	}

	return false, nil
}
