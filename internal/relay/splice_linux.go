//go:build linux

package relay

// spliceSupported is true where io.Copy between two *net.TCPConn moves data
// inside the kernel with splice(2). The standard library does the splice; this
// constant only decides whether to hand it raw TCP connections or wrap them
// and copy through a pooled buffer.
const spliceSupported = true
