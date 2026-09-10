//go:build !unix

package process

// readFileLimit reports that this platform has no per-process descriptor
// limit to read. Windows has no RLIMIT_NOFILE; its handle limits are per
// session and not something a process adjusts.
func readFileLimit() FileLimit {
	return FileLimit{}
}
