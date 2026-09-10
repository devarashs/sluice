//go:build unix

package process

import "syscall"

// readFileLimit reads RLIMIT_NOFILE. A failure to read is reported as
// unsupported rather than as an error, because there is nothing the process
// can do about it and startup must not depend on it.
func readFileLimit() FileLimit {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return FileLimit{}
	}
	return FileLimit{Supported: true, Soft: uint64(limit.Cur), Hard: uint64(limit.Max)}
}
