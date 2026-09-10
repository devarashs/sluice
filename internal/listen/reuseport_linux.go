//go:build linux

package listen

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePortSupported lets every acceptor bind its own socket on the same
// port, so the kernel spreads incoming connections across independent accept
// queues instead of serialising them on one.
//
// The trade: Linux lets any process with the same effective user id join the
// port group and take a share of connections. That is the same trade nginx
// makes with its reuseport option, and the answer is the same: run sluice as
// its own user.
const reusePortSupported = true

// reusePortControl sets SO_REUSEPORT before bind.
func reusePortControl(_, _ string, raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
