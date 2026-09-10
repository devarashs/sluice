//go:build !linux

package listen

import "syscall"

// reusePortSupported is false here: one socket is bound and shared by every
// acceptor. Windows has no SO_REUSEPORT and the BSD variants differ in
// semantics; Linux is the production target.
const reusePortSupported = false

func reusePortControl(_, _ string, _ syscall.RawConn) error {
	return nil
}
