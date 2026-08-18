// Package version exposes the Taskpilot CLI version.
//
// The canonical version lives in the VERSION file in this package directory
// and is compiled into the binary via go:embed, so plain `go build`/`go run`
// and release builds all report the same value. Bump the version by editing
// internal/version/VERSION.
package version

import (
	_ "embed"
	"strings"

	"golang.org/x/mod/semver"
)

//go:embed VERSION
var versionFile string

// version is the CLI version, read from the embedded VERSION file. It is
// unexported so external packages cannot mutate it; read it via Version.
var version = strings.TrimSpace(versionFile)

// compatKey is derived from version at init so the two can never drift; read
// it via CompatKey.
var compatKey = computeCompatKey()

// Version returns the CLI version, read from the embedded VERSION file.
func Version() string { return version }

// CompatKey returns the compatibility key for Version, used to key caches and
// gate breaking changes per SemVer.
//
// While the major version is 0 (pre-1.0, where minor bumps are considered
// breaking) it is "major.minor"; otherwise it is "major".
func CompatKey() string { return compatKey }

// computeCompatKey derives the compatibility key used to separate cache entries
// across breaking engine changes.
func computeCompatKey() string {
	// golang.org/x/mod/semver requires a leading "v".
	sv := version
	if !strings.HasPrefix(sv, "v") {
		// Normalize semver inputs to the format expected by x/mod/semver.
		sv = "v" + sv
	}

	if !semver.IsValid(sv) {
		// Unparseable builds still vary the key by version text.
		return strings.TrimPrefix(sv, "v")
	}

	if semver.Major(sv) == "v0" {
		// Pre-1.0 releases treat minor bumps as breaking changes.
		return strings.TrimPrefix(semver.MajorMinor(sv), "v")
	}

	return strings.TrimPrefix(semver.Major(sv), "v")
}
