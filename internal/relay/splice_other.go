//go:build !linux

package relay

// spliceSupported is false here: on other platforms io.Copy between two
// *net.TCPConn falls back to a per-call 32KiB allocation inside the standard
// library, which the pooled-buffer path avoids.
const spliceSupported = false
