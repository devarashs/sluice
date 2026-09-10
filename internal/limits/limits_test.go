package limits

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/config"
)

func TestConfigValidateDefaults(t *testing.T) {
	cfg := Config{PerClient: PerClientConfig{ConnectionsPerSecond: 2.5}}
	if err := cfg.Validate("limits"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.PerClient.Burst != 3 {
		t.Errorf("burst = %d, want 3 (2.5 rounded up)", cfg.PerClient.Burst)
	}
	if cfg.PerClient.MaxTrackedClients != DefaultMaxTrackedClients {
		t.Errorf("maxTrackedClients = %d, want default", cfg.PerClient.MaxTrackedClients)
	}

	disabled := Config{}
	if err := disabled.Validate("limits"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if disabled.PerClient.Burst != 0 || disabled.PerClient.MaxTrackedClients != 0 {
		t.Errorf("disabled limiter should get no defaults, got %+v", disabled.PerClient)
	}

	tiny := Config{PerClient: PerClientConfig{ConnectionsPerSecond: 0.1}}
	if err := tiny.Validate("limits"); err != nil {
		t.Fatal(err)
	}
	if tiny.PerClient.Burst != 1 {
		t.Errorf("burst for 0.1/s = %d, want 1", tiny.PerClient.Burst)
	}
}

func TestConfigValidateRejectsImpossibleValues(t *testing.T) {
	cases := map[string]struct {
		cfg   Config
		field string
	}{
		"negative cap":     {Config{MaxConnections: -1}, "limits.maxConnections"},
		"negative rate":    {Config{PerClient: PerClientConfig{ConnectionsPerSecond: -1}}, "limits.perClient.connectionsPerSecond"},
		"negative burst":   {Config{PerClient: PerClientConfig{ConnectionsPerSecond: 1, Burst: -1}}, "limits.perClient.burst"},
		"negative tracked": {Config{PerClient: PerClientConfig{ConnectionsPerSecond: 1, MaxTrackedClients: -1}}, "limits.perClient.maxTrackedClients"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := c.cfg.Validate("limits")
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != c.field {
				t.Fatalf("err = %v, want FieldError on %s", err, c.field)
			}
		})
	}
}

func TestConcurrencyCapAndRelease(t *testing.T) {
	cap := NewConcurrency(2)
	if !cap.TryAcquire() || !cap.TryAcquire() {
		t.Fatal("first two acquisitions should succeed")
	}
	if cap.TryAcquire() {
		t.Fatal("third acquisition should fail at a cap of 2")
	}
	if cap.Held() != 2 || cap.Limit() != 2 {
		t.Fatalf("held/limit = %d/%d", cap.Held(), cap.Limit())
	}
	cap.Release()
	if !cap.TryAcquire() {
		t.Fatal("acquisition after release should succeed")
	}
}

func TestConcurrencyAcquireWaitsForASlot(t *testing.T) {
	cap := NewConcurrency(1)
	if err := cap.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() { acquired <- cap.Acquire(context.Background()) }()
	select {
	case <-acquired:
		t.Fatal("Acquire returned while the only slot was held")
	case <-time.After(50 * time.Millisecond):
	}
	cap.Release()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not proceed after Release")
	}
}

func TestConcurrencyAcquireHonoursContext(t *testing.T) {
	cap := NewConcurrency(1)
	if !cap.TryAcquire() {
		t.Fatal("first acquisition should succeed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := cap.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if cap.Held() != 1 {
		t.Fatalf("active = %d after a failed acquire, want 1", cap.Held())
	}
}

func TestConcurrencyUnlimited(t *testing.T) {
	cap := NewConcurrency(0)
	for i := 0; i < 10000; i++ {
		if !cap.TryAcquire() {
			t.Fatalf("unlimited cap refused acquisition %d", i)
		}
	}
	if cap.Held() != 10000 || cap.Limit() != 0 {
		t.Fatalf("active/limit = %d/%d", cap.Held(), cap.Limit())
	}
}

func TestConcurrencyLargeCapCostsNoMemoryPerSlot(t *testing.T) {
	// A channel of zero-size elements has no buffer allocation, which is
	// what makes a million-slot cap free. This only checks it constructs
	// instantly and works; the memory claim is documented on the type.
	cap := NewConcurrency(1 << 20)
	if !cap.TryAcquire() {
		t.Fatal("acquisition failed")
	}
	cap.Release()
}

func newLimiter(t *testing.T, perSecond float64, burst, tracked int) *PerClient {
	t.Helper()
	cfg := PerClientConfig{ConnectionsPerSecond: perSecond, Burst: burst, MaxTrackedClients: tracked}
	if err := cfg.Validate("limits.perClient"); err != nil {
		t.Fatal(err)
	}
	limiter, err := NewPerClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func TestPerClientAllowsBurstThenRefillsAtRate(t *testing.T) {
	limiter := newLimiter(t, 2, 3, 100)
	client := netip.MustParseAddr("203.0.113.7")
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		if !limiter.allowAt(client, now) {
			t.Fatalf("burst connection %d refused", i+1)
		}
	}
	if limiter.allowAt(client, now) {
		t.Fatal("fourth connection within the burst should be refused")
	}
	// Two per second: half a second later exactly one token is back.
	if !limiter.allowAt(client, now.Add(500*time.Millisecond)) {
		t.Fatal("connection after refill refused")
	}
	if limiter.allowAt(client, now.Add(500*time.Millisecond)) {
		t.Fatal("second connection at the same instant should be refused")
	}

	other := netip.MustParseAddr("203.0.113.8")
	if !limiter.allowAt(other, now) {
		t.Fatal("a different client must have its own bucket")
	}
}

func TestPerClientGroupsIPv6ByPrefixAndUnmapsIPv4(t *testing.T) {
	limiter := newLimiter(t, 1, 1, 100)
	now := time.Unix(1_700_000_000, 0)

	first := netip.MustParseAddr("2001:db8:1:2::1")
	sameHost := netip.MustParseAddr("2001:db8:1:2:ffff::9")
	otherHost := netip.MustParseAddr("2001:db8:1:3::1")
	if !limiter.allowAt(first, now) {
		t.Fatal("first IPv6 connection refused")
	}
	if limiter.allowAt(sameHost, now) {
		t.Fatal("address in the same /64 must share the bucket")
	}
	if !limiter.allowAt(otherHost, now) {
		t.Fatal("address in a different /64 must have its own bucket")
	}

	v4 := netip.MustParseAddr("198.51.100.4")
	mapped := netip.MustParseAddr("::ffff:198.51.100.4")
	if !limiter.allowAt(v4, now) {
		t.Fatal("IPv4 connection refused")
	}
	if limiter.allowAt(mapped, now) {
		t.Fatal("IPv4-mapped form must share the IPv4 bucket")
	}
	if ClientKey(v4) != ClientKey(mapped) {
		t.Fatalf("keys differ: %v vs %v", ClientKey(v4), ClientKey(mapped))
	}
}

func TestPerClientMemoryIsBounded(t *testing.T) {
	const tracked = 1000
	limiter := newLimiter(t, 1, 1, tracked)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5*tracked; i++ {
		addr := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		limiter.allowAt(addr, now)
	}
	if got := limiter.Tracked(); got != tracked {
		t.Fatalf("tracked = %d, want exactly the bound %d", got, tracked)
	}
}

func TestPerClientDisabledIsNilAndAllowsEverything(t *testing.T) {
	limiter, err := NewPerClient(PerClientConfig{})
	if err != nil || limiter != nil {
		t.Fatalf("disabled config should give a nil limiter, got %v, %v", limiter, err)
	}
	client := netip.MustParseAddr("203.0.113.7")
	for i := 0; i < 1000; i++ {
		if !limiter.Allow(client) {
			t.Fatal("nil limiter refused a connection")
		}
	}
	if limiter.Tracked() != 0 {
		t.Fatal("nil limiter tracks nothing")
	}
}

func TestPerClientRejectsUnvalidatedConfig(t *testing.T) {
	if _, err := NewPerClient(PerClientConfig{ConnectionsPerSecond: 5}); err == nil {
		t.Fatal("config without defaults applied should be refused")
	}
}

func TestPerClientIsSafeUnderConcurrentUse(t *testing.T) {
	limiter := newLimiter(t, 1000, 1000, 64)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				addr := netip.AddrFrom4([4]byte{10, 0, byte(worker), byte(i)})
				limiter.Allow(addr)
			}
		}(worker)
	}
	wg.Wait()
	if limiter.Tracked() > 64 {
		t.Fatalf("tracked = %d exceeds the bound under concurrency", limiter.Tracked())
	}
}

func TestClientAddr(t *testing.T) {
	tcp := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 4444}
	if addr, ok := ClientAddr(tcp); !ok || addr != netip.MustParseAddr("203.0.113.7") {
		t.Errorf("tcp addr = %v, %v", addr, ok)
	}
	tcp6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4444}
	if addr, ok := ClientAddr(tcp6); !ok || addr != netip.MustParseAddr("2001:db8::1") {
		t.Errorf("tcp6 addr = %v, %v", addr, ok)
	}
	if _, ok := ClientAddr(pipeAddr{}); ok {
		t.Error("pipe address has no IP and must report false")
	}
	if addr, ok := ClientAddr(stringAddr("198.51.100.9:80")); !ok || addr != netip.MustParseAddr("198.51.100.9") {
		t.Errorf("string addr = %v, %v", addr, ok)
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

type stringAddr string

func (s stringAddr) Network() string { return "tcp" }
func (s stringAddr) String() string  { return string(s) }

func ExampleClientKey() {
	fmt.Println(ClientKey(netip.MustParseAddr("203.0.113.7")))
	fmt.Println(ClientKey(netip.MustParseAddr("2001:db8:1:2:3:4:5:6")))
	// Output:
	// 203.0.113.7/32
	// 2001:db8:1:2::/64
}
