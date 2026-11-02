package tapsdk

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error represents an SDK-specific error that wraps underlying RPC errors
// with additional context.
type Error struct {
	// Op is the operation that failed (e.g., "DeriveScriptKey").
	Op string

	// Err is the underlying error.
	Err error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	}

	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error {
	return e.Err
}

// IsNotFound returns true if the error indicates a resource was not found.
func (e *Error) IsNotFound() bool {
	if e.Err == nil {
		return false
	}

	st, ok := status.FromError(e.Err)
	if !ok {
		return false
	}

	return st.Code() == codes.NotFound
}

// IsUnavailable returns true if the error indicates the service is unavailable.
func (e *Error) IsUnavailable() bool {
	if e.Err == nil {
		return false
	}

	st, ok := status.FromError(e.Err)
	if !ok {
		return false
	}

	return st.Code() == codes.Unavailable
}

// IsInvalidArgument returns true if the error indicates invalid input.
func (e *Error) IsInvalidArgument() bool {
	if e.Err == nil {
		return false
	}

	st, ok := status.FromError(e.Err)
	if !ok {
		return false
	}

	return st.Code() == codes.InvalidArgument
}

// GRPCCode returns the gRPC status code if the underlying error is a gRPC
// error, or codes.Unknown otherwise.
func (e *Error) GRPCCode() codes.Code {
	if e.Err == nil {
		return codes.OK
	}

	st, ok := status.FromError(e.Err)
	if !ok {
		return codes.Unknown
	}

	return st.Code()
}

// wrapErr wraps an error with operation context. If err is nil, returns nil.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}

	return &Error{
		Op:  op,
		Err: normalizeErr(op, err),
	}
}

func normalizeErr(op string, err error) error {
	if err == nil || wrapsKnownErr(err) {
		return err
	}

	if sentinel := matchErrorMessage(op, err.Error()); sentinel != nil {
		return fmt.Errorf("%w: %w", sentinel, err)
	}

	st, ok := status.FromError(err)
	if !ok {
		return err
	}

	var sentinel error
	switch st.Code() {
	case codes.Unauthenticated:
		sentinel = ErrUnauthenticated

	case codes.PermissionDenied:
		sentinel = ErrPermissionDenied

	case codes.NotFound:
		if isProofOp(op) {
			sentinel = ErrProofNotFound
		} else {
			sentinel = ErrAssetUnknown
		}

	case codes.InvalidArgument:
		msg := strings.ToLower(st.Message())
		if strings.Contains(msg, "asset ref") {
			sentinel = ErrInvalidAssetRef
		} else {
			sentinel = ErrTapdPrecondition
		}

	case codes.FailedPrecondition:
		sentinel = ErrTapdPrecondition

	case codes.Unimplemented:
		sentinel = ErrUnsupportedByTapd
	}

	if sentinel == nil {
		return err
	}

	return fmt.Errorf("%w: %w", sentinel, err)
}

func wrapsKnownErr(err error) bool {
	for _, sentinel := range knownSentinelErrors {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	return false
}

func matchErrorMessage(op, msg string) error {
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(
		lower, "all addrs must be of the same asset id or group key",
	):
		return ErrMixedAssetBatchUnsupported

	case strings.Contains(lower, "invalid asset ref"):
		return ErrInvalidAssetRef

	case strings.Contains(lower, "insufficient") &&
		strings.Contains(lower, "balance"):

		return ErrInsufficientBalance

	case isProofOp(op) && strings.Contains(lower, "proof") &&
		(strings.Contains(lower, "not found") ||
			strings.Contains(lower, "missing")):

		return ErrProofNotFound

	case strings.Contains(lower, "unable to find asset") ||
		strings.Contains(lower, "asset lookup failed") ||
		strings.Contains(lower, "unknown asset"):

		return ErrAssetUnknown

	case strings.Contains(lower, "permission denied"):
		return ErrPermissionDenied

	case strings.Contains(lower, "unauthenticated"):
		return ErrUnauthenticated

	case strings.Contains(lower, "unimplemented") ||
		strings.Contains(lower, "unsupported"):

		return ErrUnsupportedByTapd
	}

	return nil
}

func isProofOp(op string) bool {
	return strings.Contains(op, "Proof")
}

// Sentinel errors for common SDK error conditions.
var (
	// ErrBuilderFinished is returned when attempting to use a TxBuilder
	// that has already completed its transaction.
	ErrBuilderFinished = errors.New("builder already finished")

	// ErrNotFunded is returned when attempting to sign a transaction
	// that hasn't been funded yet.
	ErrNotFunded = errors.New("transaction not funded")

	// ErrNotSigned is returned when attempting to complete a transaction
	// that hasn't been signed yet.
	ErrNotSigned = errors.New("transaction not signed")

	// ErrNotCommitted is returned when attempting to finish a transaction
	// that hasn't been committed yet.
	ErrNotCommitted = errors.New("transaction not committed")

	// ErrNoRecipients is returned when attempting to fund a transaction
	// with no recipients configured.
	ErrNoRecipients = errors.New("no recipients configured")

	// ErrNoReceiverKeys is returned when attempting to execute an
	// interactive transfer without setting receiver keys.
	ErrNoReceiverKeys = errors.New("receiver keys not set")

	// ErrNoAssetRef is returned when attempting to execute a transfer
	// without specifying a usable asset reference.
	ErrNoAssetRef = errors.New("asset ref not set")

	// ErrZeroAmount is returned when attempting to execute a transfer
	// with zero amount.
	ErrZeroAmount = errors.New("amount must be greater than zero")

	// ErrGroupKeyNotSupported is returned when a group-key AssetRef is
	// used in a context that cannot resolve it to a concrete issuance.
	ErrGroupKeyNotSupported = errors.New(
		"group-key AssetRef requires a wallet-backed asset resolver",
	)

	// ErrInsufficientBalance is returned when the wallet knows the asset
	// ref but cannot select enough spendable units for the requested
	// operation.
	ErrInsufficientBalance = errors.New("insufficient asset balance")

	// ErrInvalidAssetRef is returned when an AssetRef is malformed or is
	// rejected by tapd as an invalid asset identifier.
	ErrInvalidAssetRef = errors.New("invalid asset ref")

	// ErrPermissionDenied is returned when tapd rejects an operation because
	// the macaroon or user credentials do not have enough permissions.
	ErrPermissionDenied = errors.New("permission denied")

	// ErrUnauthenticated is returned when tapd rejects an operation because
	// authentication is missing or invalid.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrProofNotFound is returned when tapd cannot locate a requested asset
	// proof.
	ErrProofNotFound = errors.New("proof not found")

	// ErrTapdPrecondition is returned when tapd rejects a request because a
	// daemon-side precondition is not satisfied.
	ErrTapdPrecondition = errors.New("tapd precondition failed")

	// ErrUnsupportedByTapd is returned when the connected tapd version does
	// not support the requested operation.
	ErrUnsupportedByTapd = errors.New("operation unsupported by tapd")

	// ErrUnsupportedCustomAnchorCapability is returned when an advanced
	// custom-anchor flow requires a backend capability that is unsupported or
	// unknown.
	ErrUnsupportedCustomAnchorCapability = errors.New(
		"custom-anchor capability unsupported",
	)

	// ErrUnconfirmedCustomAnchorPublish is returned when a package contains
	// compact unconfirmed proof-path steps. Current tapd publish/log requires
	// chain-confirmed input proofs in its local archive.
	ErrUnconfirmedCustomAnchorPublish = errors.New(
		"unconfirmed custom-anchor package cannot be published through tapd",
	)

	// ErrMixedAssetBatchUnsupported is returned when one high-level send
	// request contains recipients for multiple logical assets. Wallet.SendMulti
	// sends one logical asset in one tapd request.
	ErrMixedAssetBatchUnsupported = errors.New(
		"mixed-asset send batch unsupported",
	)

	// ErrAmountRequired is returned when attempting to send to a V2
	// address that does not embed an amount without specifying one.
	ErrAmountRequired = errors.New(
		"amount required for V2 address without an embedded amount",
	)

	// ErrAmountMismatch is returned when the caller provides an
	// explicit amount that does not match the amount embedded in the
	// destination address.
	ErrAmountMismatch = errors.New(
		"amount does not match the address-embedded amount",
	)

	// ErrAssetUnknown is returned by Wallet.GetBalance (and related
	// high-level helpers) when the wallet has no record of the asset
	// ref: no balance entry and no issuance or transfer root in the
	// local universe. Callers should detect it with errors.Is. A known
	// asset with zero confirmed units produces (0, nil) instead.
	ErrAssetUnknown = errors.New("asset ref is unknown to the wallet")

	// ErrNoProofs is returned when a proof bundle export/import path cannot
	// find any proof entries for a known asset ref.
	ErrNoProofs = errors.New("no proofs found for asset ref")

	// ErrIncompleteProofBundle is returned when a proof bundle is missing
	// entries or entry proof bytes.
	ErrIncompleteProofBundle = errors.New("proof bundle is incomplete")

	// ErrOwnershipProofRequired is returned when ownership verification is
	// attempted without proof bytes.
	ErrOwnershipProofRequired = errors.New("ownership proof is required")

	// ErrInvalidChallenge is returned when an ownership proof challenge is
	// explicitly set but is not a non-zero 32-byte value.
	ErrInvalidChallenge = errors.New("invalid ownership proof challenge")

	// ErrOwnershipAmountRequired is returned when proving fungible asset
	// ownership without an explicit amount.
	ErrOwnershipAmountRequired = errors.New(
		"ownership proof amount is required",
	)

	// ErrOwnershipProofInvalid is returned when ownership verification
	// completes but tapd reports the proof is invalid.
	ErrOwnershipProofInvalid = errors.New("ownership proof is invalid")

	// ErrAssetNameRequired is returned when an issuer call is missing the
	// asset or NFT name required by tapd.
	ErrAssetNameRequired = errors.New("asset name is required")

	// ErrMintBatchActive is returned by high-level issuer calls when tapd
	// already has a mint batch that has not reached a terminal state.
	ErrMintBatchActive = errors.New("mint batch is already active")

	// ErrWrongAssetType is returned when an AssetRef resolves successfully
	// but not to the kind of entity required by the operation.
	ErrWrongAssetType = errors.New("asset ref has the wrong asset type")

	// ErrAssetNotIssuable is returned when an operation requires a grouped
	// asset but the AssetRef resolves to a standalone issuance.
	ErrAssetNotIssuable = errors.New("asset ref is not an issuable grouped asset")

	// ErrMintResolveTimeout is returned when tapd accepted a high-level
	// issuer mint request but the SDK timed out before the wallet projection
	// exposed the resulting entity. This does not mean the mint failed. Callers
	// must inspect wallet assets, issuances, collections, or mint batches
	// before retrying to avoid duplicate issuance.
	ErrMintResolveTimeout = errors.New("mint result resolution timed out")

	// ErrMintResultNotFound is returned when the SDK resolved a wallet row for
	// an accepted mint but could not map it into the requested high-level
	// entity. This does not mean tapd rejected the mint; callers should inspect
	// wallet state before retrying.
	ErrMintResultNotFound = errors.New("mint result could not be mapped")

	// ErrExternalIssuanceKeyRequired is returned when an issuer call needs an
	// external issuance signature but no external key descriptor was provided.
	ErrExternalIssuanceKeyRequired = errors.New(
		"external issuance key is required",
	)

	// ErrExternalIssuanceSignerRequired is returned when an external issuance
	// key is configured but no signer was provided to complete the mint.
	ErrExternalIssuanceSignerRequired = errors.New(
		"external issuance signer is required",
	)

	// ErrExternalIssuanceRequestNotFound is returned when tapd does not return
	// an issuance signing payload after an external signer was configured.
	ErrExternalIssuanceRequestNotFound = errors.New(
		"external issuance signing request not found",
	)

	// ErrExternalIssuanceSignatureRequired is returned when an external signer
	// did not return a signed issuance PSBT.
	ErrExternalIssuanceSignatureRequired = errors.New(
		"external issuance signature is required",
	)

	// ErrUniverseHostRequired is returned when a universe sync request does
	// not specify the remote universe host.
	ErrUniverseHostRequired = errors.New("universe host is required")

	// ErrInvalidUniverseHost is returned when a universe sync host is not a
	// host:port address accepted by the high-level universe facade.
	ErrInvalidUniverseHost = errors.New("invalid universe host")

	// ErrDuplicateAssetRef is returned when a batch call receives the same
	// asset ref more than once.
	ErrDuplicateAssetRef = errors.New("duplicate asset ref")

	// ErrUniverseProofTypeRequired is returned when a universe proof query
	// requires an explicit issuance or transfer proof type.
	ErrUniverseProofTypeRequired = errors.New(
		"universe proof type is required",
	)

	// ErrInvalidPagination is returned when pagination options are malformed.
	ErrInvalidPagination = errors.New("invalid pagination")
)

var knownSentinelErrors = []error{
	ErrBuilderFinished,
	ErrNotFunded,
	ErrNotSigned,
	ErrNotCommitted,
	ErrNoRecipients,
	ErrNoReceiverKeys,
	ErrNoAssetRef,
	ErrZeroAmount,
	ErrGroupKeyNotSupported,
	ErrInsufficientBalance,
	ErrInvalidAssetRef,
	ErrPermissionDenied,
	ErrUnauthenticated,
	ErrProofNotFound,
	ErrTapdPrecondition,
	ErrUnsupportedByTapd,
	ErrUnsupportedCustomAnchorCapability,
	ErrUnconfirmedCustomAnchorPublish,
	ErrMixedAssetBatchUnsupported,
	ErrAmountRequired,
	ErrAmountMismatch,
	ErrAssetUnknown,
	ErrNoProofs,
	ErrIncompleteProofBundle,
	ErrOwnershipProofRequired,
	ErrInvalidChallenge,
	ErrOwnershipAmountRequired,
	ErrOwnershipProofInvalid,
	ErrAssetNameRequired,
	ErrMintBatchActive,
	ErrWrongAssetType,
	ErrAssetNotIssuable,
	ErrMintResolveTimeout,
	ErrMintResultNotFound,
	ErrExternalIssuanceKeyRequired,
	ErrExternalIssuanceSignerRequired,
	ErrExternalIssuanceRequestNotFound,
	ErrExternalIssuanceSignatureRequired,
	ErrUniverseHostRequired,
	ErrInvalidUniverseHost,
	ErrDuplicateAssetRef,
	ErrUniverseProofTypeRequired,
	ErrInvalidPagination,
}
