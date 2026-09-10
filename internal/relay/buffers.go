package relay

import "sync"

// pools holds one sync.Pool per buffer size, keyed by int. A relay holds its
// buffer for its whole life, so pooling only saves the allocation churn of
// short connections, but at high connection rates that churn is what keeps
// the collector busy.
var pools sync.Map

// getBuffer returns a buffer of exactly size bytes from the pool for that
// size.
func getBuffer(size int) []byte {
	pool, _ := pools.LoadOrStore(size, &sync.Pool{
		New: func() any {
			buffer := make([]byte, size)
			return &buffer
		},
	})
	return *pool.(*sync.Pool).Get().(*[]byte)
}

// putBuffer returns a buffer obtained from getBuffer. The pool is chosen by
// capacity, so a buffer that was resliced still goes back where it came from.
func putBuffer(buffer []byte) {
	if pool, ok := pools.Load(cap(buffer)); ok {
		full := buffer[:cap(buffer)]
		pool.(*sync.Pool).Put(&full)
	}
}
