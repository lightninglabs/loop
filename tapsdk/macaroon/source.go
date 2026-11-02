package macaroon

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// Source describes where the SDK should obtain authentication
// macaroons. Exactly one source is held by the caller's Config, so
// conflicting inputs cannot be expressed — the compiler enforces
// what runtime validation used to. Instances are obtained from
// FromPath, FromDir, or FromHex.
type Source interface {
	// LoadPouch loads the full per-service Pouch represented by
	// this source.
	LoadPouch() (Pouch, error)
}

// FromPath returns a Source that loads a single macaroon file and
// reuses it for every service (the file is assumed to bear all
// required caveats, e.g. the admin macaroon).
func FromPath(path string) Source {
	return pathSource{path: path}
}

// FromDir returns a Source that loads one macaroon per service from
// the given directory, matching tapd's standard on-disk layout.
func FromDir(dir string) Source {
	return dirSource{dir: dir}
}

// FromHex returns a Source that uses a hex-encoded macaroon as the
// single credential for every service.
func FromHex(hex string) Source {
	return hexSource{hex: hex}
}

type pathSource struct {
	path string
}

// LoadPouch implements Source.
func (s pathSource) LoadPouch() (Pouch, error) {
	mac, err := newSerializedMacaroon(s.path)
	if err != nil {
		return nil, err
	}

	return pouchForAllServices(mac), nil
}

type dirSource struct {
	dir string
}

// LoadPouch implements Source.
func (s dirSource) LoadPouch() (Pouch, error) {
	p := make(Pouch)
	for _, macName := range macaroonServices {
		mac, err := newSerializedMacaroon(filepath.Join(
			s.dir, string(macName),
		))
		if err != nil {
			return nil, err
		}
		p[macName] = mac
	}

	return p, nil
}

type hexSource struct {
	hex string
}

// LoadPouch implements Source.
func (s hexSource) LoadPouch() (Pouch, error) {
	trimmed := strings.TrimSpace(s.hex)
	if trimmed == "" {
		return nil, fmt.Errorf("macaroon hex must not be empty")
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return nil, fmt.Errorf("invalid macaroon hex: %w", err)
	}

	return pouchForAllServices(SerializedMacaroon(trimmed)), nil
}

// pouchForAllServices returns a Pouch where every service entry
// points at the same macaroon — used by FromPath and FromHex, where
// a single credential authenticates all sub-servers.
func pouchForAllServices(mac SerializedMacaroon) Pouch {
	return Pouch{
		WalletKitServiceMac: mac,
		AdminServiceMac:     mac,
		ProofServiceMac:     mac,
		UniverseServiceMac:  mac,
		MintServiceMac:      mac,
		ReadOnlyServiceMac:  mac,
	}
}
