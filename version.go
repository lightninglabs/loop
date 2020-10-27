package loop

// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/version.go
// Copyright (C) 2015-2020 The Lightning Network Developers

import (
	"bytes"
	"fmt"
	"strings"
)

// Commit stores the current commit hash of this build, this should be set
// using the -ldflags during compilation.
var Commit string

// semanticAlphabet
const semanticAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"

// These constants define the application version and follow the semantic
// versioning 2.0.0 spec (http://semver.org/).
const (
	// Note: please update release_notes.md when you change these values.
	appMajor uint = 0
	appMinor uint = 11
	appPatch uint = 0

	// appPreRelease MUST only contain characters from semanticAlphabet per
	// the semantic versioning spec.
	appPreRelease = "beta"

	// defaultAgentName is the default name of the software that is added as
	// the first part of the user agent string.
	defaultAgentName = "loopd"
)

// AgentName stores the name of the software that is added as the first part of
// the user agent string. This defaults to the value "loopd" when being run as
// a standalone component but can be overwritten by LiT for example when loopd
// is integrated into the UI.
var AgentName = defaultAgentName

// Version returns the application version as a properly formed string per the
// semantic versioning 2.0.0 spec (http://semver.org/) and the commit it was
// built on.
func Version() string {
	// Append commit hash of current build to version.
	return fmt.Sprintf("%s commit=%s", semanticVersion(), Commit)
}

// UserAgent returns the full user agent string that identifies the software
// that is submitting swaps to the loop server.
func UserAgent() string {
	// Assemble full string, including the commit hash of current build.
	return fmt.Sprintf(
		"%s/v%s/commit=%s", AgentName, semanticVersion(), Commit,
	)
}

// semanticVersion returns the SemVer part of the version.
func semanticVersion() string {
	// Start with the major, minor, and patch versions.
	version := fmt.Sprintf("%d.%d.%d", appMajor, appMinor, appPatch)

	// Append pre-release version if there is one.  The hyphen called for
	// by the semantic versioning spec is automatically appended and should
	// not be contained in the pre-release string.  The pre-release version
	// is not appended if it contains invalid characters.
	preRelease := normalizeVerString(appPreRelease)
	if preRelease != "" {
		version = fmt.Sprintf("%s-%s", version, preRelease)
	}

	return version
}

// normalizeVerString returns the passed string stripped of all characters
// which are not valid according to the semantic versioning guidelines for
// pre-release version and build metadata strings.  In particular they MUST
// only contain characters in semanticAlphabet.
func normalizeVerString(str string) string {
	var result bytes.Buffer
	for _, r := range str {
		if strings.ContainsRune(semanticAlphabet, r) {
			result.WriteRune(r)
		}
	}
	return result.String()
}
