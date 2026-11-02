package tapsdk

import "fmt"

const unknownString = "unknown"

// CustomAnchorBackendMode identifies the kind of backend that reported a
// custom-anchor capability set.
type CustomAnchorBackendMode string

const (
	// CustomAnchorBackendUnknown means the SDK cannot classify the backend.
	CustomAnchorBackendUnknown CustomAnchorBackendMode = unknownString

	// CustomAnchorBackendLocalTapd means the backend is local tapd.
	CustomAnchorBackendLocalTapd = CustomAnchorBackendMode(
		"local-tapd",
	)

	// CustomAnchorBackendRemoteTapd means the backend is remote tapd.
	CustomAnchorBackendRemoteTapd = CustomAnchorBackendMode(
		"remote-tapd",
	)

	// CustomAnchorBackendHosted means the backend is a hosted service.
	CustomAnchorBackendHosted CustomAnchorBackendMode = "hosted"

	// CustomAnchorBackendLightweight means the backend is a smaller service
	// that exposes only the SDK-required wallet, proof, or builder calls.
	CustomAnchorBackendLightweight = CustomAnchorBackendMode(
		"lightweight",
	)
)

// CustomAnchorCapability names a backend feature used by the advanced
// custom-anchor transaction flow.
type CustomAnchorCapability string

const (
	// CustomAnchorCapabilityCommit commits asset changes into a
	// caller-visible anchor PSBT.
	CustomAnchorCapabilityCommit = CustomAnchorCapability(
		"custom_anchor_commit",
	)

	// CustomAnchorCapabilityCallerAnchorPSBT accepts a caller-supplied BTC
	// anchor PSBT template.
	CustomAnchorCapabilityCallerAnchorPSBT = CustomAnchorCapability(
		"caller_anchor_psbt",
	)

	// CustomAnchorCapabilitySkipFunding skips backend BTC wallet funding.
	CustomAnchorCapabilitySkipFunding = CustomAnchorCapability(
		"skip_funding",
	)

	// CustomAnchorCapabilityNoChangeOutput commits without adding
	// or selecting a BTC change output.
	CustomAnchorCapabilityNoChangeOutput = CustomAnchorCapability(
		"no_change_output",
	)

	// CustomAnchorCapabilityWalletFunding funds the BTC anchor from the
	// backend wallet.
	CustomAnchorCapabilityWalletFunding = CustomAnchorCapability(
		"wallet_funding",
	)

	// CustomAnchorCapabilitySkipBroadcast logs a transfer while leaving BTC
	// anchor broadcast to the caller.
	CustomAnchorCapabilitySkipBroadcast = CustomAnchorCapability(
		"skip_broadcast",
	)

	// CustomAnchorCapabilityP2AAnchorOutput supports pay-to-anchor or a
	// semantically equivalent external fee-bump policy.
	CustomAnchorCapabilityP2AAnchorOutput = CustomAnchorCapability(
		"p2a_anchor_output",
	)

	// CustomAnchorCapabilityExternalFeeBump supports caller fee bumps.
	CustomAnchorCapabilityExternalFeeBump = CustomAnchorCapability(
		"external_fee_bump",
	)

	// CustomAnchorCapabilityProofRegistration registers proof state
	// after the custom transfer is logged.
	CustomAnchorCapabilityProofRegistration = CustomAnchorCapability(
		"proof_registration",
	)

	// CustomAnchorCapabilityLocalVerification verifies the plan or
	// package in SDK/library code without trusting the backend.
	CustomAnchorCapabilityLocalVerification = CustomAnchorCapability(
		"local_verification",
	)

	// CustomAnchorCapabilityBackendVerification verifies the plan or
	// package through the backend.
	CustomAnchorCapabilityBackendVerification = CustomAnchorCapability(
		"backend_verification",
	)
)

// CustomAnchorCapabilityStatus describes whether a capability is available.
type CustomAnchorCapabilityStatus uint8

const (
	// CustomAnchorCapabilityUnknown means the backend did not report
	// a usable answer for the capability.
	CustomAnchorCapabilityUnknown CustomAnchorCapabilityStatus = iota

	// CustomAnchorCapabilityUnsupported means the backend reports that the
	// capability is not available.
	CustomAnchorCapabilityUnsupported

	// CustomAnchorCapabilitySupported means the backend reports that the
	// capability is available.
	CustomAnchorCapabilitySupported
)

// String returns the stable text form of the support status.
func (s CustomAnchorCapabilityStatus) String() string {
	switch s {
	case CustomAnchorCapabilitySupported:
		return "supported"

	case CustomAnchorCapabilityUnsupported:
		return "unsupported"

	default:
		return unknownString
	}
}

// Supported returns true when the capability can be used safely.
func (s CustomAnchorCapabilityStatus) Supported() bool {
	return s == CustomAnchorCapabilitySupported
}

// CustomAnchorCapabilities reports backend support for advanced custom-anchor
// transaction operations. Unknown capabilities are treated as unavailable by
// Require so callers fail before signing.
type CustomAnchorCapabilities struct {
	BackendMode CustomAnchorBackendMode

	CustomAnchorCommit       CustomAnchorCapabilityStatus
	CallerSuppliedAnchorPSBT CustomAnchorCapabilityStatus
	SkipFunding              CustomAnchorCapabilityStatus
	NoChangeOutput           CustomAnchorCapabilityStatus
	WalletFunding            CustomAnchorCapabilityStatus
	SkipBroadcast            CustomAnchorCapabilityStatus
	P2AAnchorOutput          CustomAnchorCapabilityStatus
	ExternalFeeBump          CustomAnchorCapabilityStatus
	ProofRegistration        CustomAnchorCapabilityStatus
	LocalVerification        CustomAnchorCapabilityStatus
	BackendVerification      CustomAnchorCapabilityStatus
}

// DefaultTapdCustomAnchorCapabilities returns an SDK-version-pinned assumption
// profile for tapd-compatible transports. This is not runtime discovery: tapd
// does not currently expose a capability RPC. Features not proven by the
// pinned API are deliberately left unknown so callers fail closed.
func DefaultTapdCustomAnchorCapabilities() CustomAnchorCapabilities {
	return CustomAnchorCapabilities{
		BackendMode:              CustomAnchorBackendUnknown,
		CustomAnchorCommit:       CustomAnchorCapabilitySupported,
		CallerSuppliedAnchorPSBT: CustomAnchorCapabilitySupported,
		SkipFunding:              CustomAnchorCapabilitySupported,
		NoChangeOutput:           CustomAnchorCapabilityUnknown,
		WalletFunding:            CustomAnchorCapabilitySupported,
		SkipBroadcast:            CustomAnchorCapabilitySupported,
		P2AAnchorOutput:          CustomAnchorCapabilityUnknown,
		ExternalFeeBump:          CustomAnchorCapabilityUnknown,
		ProofRegistration:        CustomAnchorCapabilityUnknown,
		LocalVerification:        CustomAnchorCapabilityUnsupported,
		BackendVerification:      CustomAnchorCapabilityUnknown,
	}
}

// Support returns the reported status for a capability. Unknown capability
// names are treated as unknown rather than supported.
func (c CustomAnchorCapabilities) Support(
	capability CustomAnchorCapability) CustomAnchorCapabilityStatus {

	switch capability {
	case CustomAnchorCapabilityCommit:
		return c.CustomAnchorCommit

	case CustomAnchorCapabilityCallerAnchorPSBT:
		return c.CallerSuppliedAnchorPSBT

	case CustomAnchorCapabilitySkipFunding:
		return c.SkipFunding

	case CustomAnchorCapabilityNoChangeOutput:
		return c.NoChangeOutput

	case CustomAnchorCapabilityWalletFunding:
		return c.WalletFunding

	case CustomAnchorCapabilitySkipBroadcast:
		return c.SkipBroadcast

	case CustomAnchorCapabilityP2AAnchorOutput:
		return c.P2AAnchorOutput

	case CustomAnchorCapabilityExternalFeeBump:
		return c.ExternalFeeBump

	case CustomAnchorCapabilityProofRegistration:
		return c.ProofRegistration

	case CustomAnchorCapabilityLocalVerification:
		return c.LocalVerification

	case CustomAnchorCapabilityBackendVerification:
		return c.BackendVerification

	default:
		return CustomAnchorCapabilityUnknown
	}
}

// Require returns an unsupported-capability error unless every requested
// capability is explicitly supported.
func (c CustomAnchorCapabilities) Require(
	capabilities ...CustomAnchorCapability) error {

	for _, capability := range capabilities {
		status := c.Support(capability)
		if !status.Supported() {
			return &UnsupportedCustomAnchorCapabilityError{
				BackendMode: c.BackendMode,
				Capability:  capability,
				Status:      status,
			}
		}
	}

	return nil
}

// UnsupportedCustomAnchorCapabilityError identifies a missing capability and
// the backend mode that reported it.
type UnsupportedCustomAnchorCapabilityError struct {
	BackendMode CustomAnchorBackendMode
	Capability  CustomAnchorCapability
	Status      CustomAnchorCapabilityStatus
}

// Error returns a human-readable unsupported capability message.
func (e *UnsupportedCustomAnchorCapabilityError) Error() string {
	backendMode := e.BackendMode
	if backendMode == "" {
		backendMode = CustomAnchorBackendUnknown
	}

	capability := e.Capability
	if capability == "" {
		capability = CustomAnchorCapability(unknownString)
	}

	return fmt.Sprintf(
		"custom-anchor capability %q is %s for backend mode %q",
		capability, e.Status, backendMode,
	)
}

// Unwrap returns the sentinel error for errors.Is checks.
func (e *UnsupportedCustomAnchorCapabilityError) Unwrap() error {
	return ErrUnsupportedCustomAnchorCapability
}
