package limits

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// PerClient rate-limits new connections per client with one token bucket
// per client held in a bounded LRU. A nil *PerClient is a disabled limiter
// that allows everything, so callers need not branch on configuration.
type PerClient struct {
	limit   rate.Limit
	burst   int
	buckets *lru.Cache[netip.Prefix, *rate.Limiter]
}

// NewPerClient builds a limiter from validated configuration, or returns nil
// when the configuration disables it.
func NewPerClient(cfg PerClientConfig) (*PerClient, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if cfg.Burst <= 0 || cfg.MaxTrackedClients <= 0 {
		return nil, fmt.Errorf("limits: per-client config must be validated first: burst %d, maxTrackedClients %d", cfg.Burst, cfg.MaxTrackedClients)
	}
	buckets, err := lru.New[netip.Prefix, *rate.Limiter](cfg.MaxTrackedClients)
	if err != nil {
		return nil, fmt.Errorf("limits: %w", err)
	}
	return &PerClient{limit: rate.Limit(cfg.ConnectionsPerSecond), burst: cfg.Burst, buckets: buckets}, nil
}

// Allow reports whether a new connection from addr may proceed now, and
// consumes one token if so.
func (p *PerClient) Allow(addr netip.Addr) bool {
	return p.allowAt(addr, time.Now())
}

// allowAt is Allow at a chosen instant, so tests can move time.
func (p *PerClient) allowAt(addr netip.Addr, now time.Time) bool {
	if p == nil {
		return true
	}
	key := ClientKey(addr)
	bucket, found := p.buckets.Get(key)
	if !found {
		// PeekOrAdd is atomic under the cache's lock, so two goroutines
		// racing to create a client's bucket agree on one of them.
		fresh := rate.NewLimiter(p.limit, p.burst)
		if previous, existed, _ := p.buckets.PeekOrAdd(key, fresh); existed {
			bucket = previous
		} else {
			bucket = fresh
		}
	}
	return bucket.AllowN(now, 1)
}

// Tracked is the number of clients currently holding a bucket. It never
// exceeds the configured maximum.
func (p *PerClient) Tracked() int {
	if p == nil {
		return 0
	}
	return p.buckets.Len()
}

// ClientKey folds an address into the unit one client controls: the single
// address for IPv4, and the /64 for IPv6. IPv4-mapped IPv6 addresses are
// unmapped first so a client is the same client whichever way it connected.
func ClientKey(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap()
	if addr.Is4() {
		return netip.PrefixFrom(addr, 32)
	}
	return netip.PrefixFrom(addr, 64).Masked()
}

// ClientAddr extracts the client's IP from a connection's remote address.
// A dual-stack listener reports IPv4 clients as IPv4-mapped IPv6 addresses;
// those are unmapped so logs and keys show the address the client actually
// has. The second result is false for addresses that carry no IP, such as a
// net.Pipe in tests.
func ClientAddr(remote net.Addr) (netip.Addr, bool) {
	if tcpAddr, ok := remote.(*net.TCPAddr); ok {
		addr := tcpAddr.AddrPort().Addr().Unmap()
		return addr, addr.IsValid()
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
