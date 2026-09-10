// Package version exposes the build identity stamped into the binary at link
// time, so a running process can say exactly which source it was built from.
package version

import (
	"fmt"
	"runtime"
)

// These are overwritten by the release pipeline with
// -ldflags "-X github.com/devarashs/sluice/internal/version.Version=v1.2.3"
// and the matching Commit and Date. The defaults identify a local build, so a
// stray development binary on a production host is recognisable in its logs.
var (
	// Version is the release tag, such as "v0.1.0".
	Version = "dev"
	// Commit is the short git hash the binary was built from.
	Commit = "unknown"
	// Date is the UTC build time in RFC 3339 form.
	Date = "unknown"
)

// String renders the build identity on one line for `sluice version` and for
// the first line every mode logs at startup.
func String() string {
	return fmt.Sprintf("sluice %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
