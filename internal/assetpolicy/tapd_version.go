package policy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightninglabs/taproot-assets/taprpc"
	"google.golang.org/grpc"
)

// MinOnchainSwapTapdVersion is the oldest tapd release that may create new
// on-chain asset swap addresses or verify an untrusted HTLC/deposit history.
// Local transaction and witness builders remain available for recovery of
// existing contracts; this does not implement an application recovery manager.
//
// Taproot Assets v0.8.8 fixes a regression introduced in v0.8.6 that
// could prevent asset invoices from settling. New on-chain swaps rely on
// that fix as well as tapd's activation and Taproot proof validation.
const MinOnchainSwapTapdVersion = "0.8.8"

// ErrTapdVersionUnsupported is returned when tapd is older than
// MinOnchainSwapTapdVersion or reports a version that cannot be parsed.
var ErrTapdVersionUnsupported = errors.New("unsupported tapd version")

// tapdVersion is the release number of a tapd version.
type tapdVersion struct {
	major     uint64
	minor     uint64
	patch     uint64
	qualifier string
}

// String returns the release number in its vMAJOR.MINOR.PATCH form.
func (v tapdVersion) String() string {
	release := fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch)
	if v.qualifier != "" {
		return release + "-" + v.qualifier
	}
	return release
}

// atLeast returns true if the release number is not older than other.
func (v tapdVersion) atLeast(other tapdVersion) bool {
	if v.major != other.major {
		return v.major > other.major
	}
	if v.minor != other.minor {
		return v.minor > other.minor
	}

	if v.patch != other.patch {
		return v.patch > other.patch
	}
	// The v0.8.8 release identifies itself as 0.8.8-alpha.
	return v.qualifier == "" || v.qualifier == "alpha"
}

// parseTapdVersion parses the release number of a version string as tapd
// reports it in GetInfo, such as "0.8.8-alpha commit=v0.8.8".
func parseTapdVersion(version string) (tapdVersion, bool) {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return tapdVersion{}, false
	}

	release, qualifier, _ := strings.Cut(strings.TrimPrefix(fields[0], "v"), "-")
	parts := strings.Split(release, ".")
	if len(parts) != 3 {
		return tapdVersion{}, false
	}

	var numbers [3]uint64
	for idx, part := range parts {
		number, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return tapdVersion{}, false
		}
		numbers[idx] = number
	}

	return tapdVersion{
		major:     numbers[0],
		minor:     numbers[1],
		patch:     numbers[2],
		qualifier: qualifier,
	}, true
}

// CheckOnchainSwapTapdVersion returns an error wrapping
// ErrTapdVersionUnsupported unless the version that tapd reports in GetInfo is
// at least MinOnchainSwapTapdVersion.
func CheckOnchainSwapTapdVersion(version string) error {
	floor, ok := parseTapdVersion(MinOnchainSwapTapdVersion)
	if !ok {
		return fmt.Errorf("invalid minimum tapd version %q",
			MinOnchainSwapTapdVersion)
	}

	actual, ok := parseTapdVersion(version)
	if !ok {
		return fmt.Errorf("%w: cannot parse %q", ErrTapdVersionUnsupported,
			version)
	}
	if !actual.atLeast(floor) {
		return fmt.Errorf("%w: %v is older than %v",
			ErrTapdVersionUnsupported, actual, floor)
	}

	return nil
}

// TapdInfoClient is the tapd boundary needed to check the running verifier and
// address backend, independently of any module version used to build a caller.
type TapdInfoClient interface {
	GetInfo(context.Context, *taprpc.GetInfoRequest,
		...grpc.CallOption) (*taprpc.GetInfoResponse, error)
}

// RequireOnchainSwapTapd checks the running backend immediately before an
// address-creation or untrusted-history acceptance operation. It is not a
// daemon-wide startup gate: ordinary asset payments and local recovery
// signing do not use this boundary.
func RequireOnchainSwapTapd(ctx context.Context, client TapdInfoClient) error {
	if client == nil {
		return fmt.Errorf("%w: tapd client is required",
			ErrTapdVersionUnsupported)
	}

	info, err := client.GetInfo(ctx, &taprpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("unable to get tapd version: %w", err)
	}
	if info == nil {
		return fmt.Errorf("%w: tapd returned no version",
			ErrTapdVersionUnsupported)
	}

	return CheckOnchainSwapTapdVersion(info.Version)
}
