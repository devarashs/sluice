package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// pairMaker returns both ends of one connection. Tests run against real
// loopback TCP, which can splice on Linux, and against net.Pipe, which is
// never a *net.TCPConn and so always takes the buffered path.
type pairMaker func(t *testing.T) (near, far net.Conn)

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptedCh := make(chan accepted, 1)
	go func() {
		conn, err := listener.Accept()
		acceptedCh <- accepted{conn, err}
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-acceptedCh
	if server.err != nil {
		t.Fatal(server.err)
	}
	t.Cleanup(func() {
		client.Close()
		server.conn.Close()
	})
	return client, server.conn
}

func pipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	near, far := net.Pipe()
	t.Cleanup(func() {
		near.Close()
		far.Close()
	})
	return near, far
}

var pairKinds = map[string]pairMaker{"tcp": tcpPair, "pipe": pipePair}

// relayed wires left <-> relayA ==Pipe== relayB <-> right and starts the relay.
// The test talks to left and right; the relay owns the middle two.
func relayed(t *testing.T, newPair pairMaker, opts Options) (left, right net.Conn, results <-chan Result, cancel context.CancelFunc) {
	t.Helper()
	left, relayA := newPair(t)
	relayB, right := newPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan Result, 1)
	go func() { resultCh <- Pipe(ctx, relayA, relayB, opts) }()
	t.Cleanup(cancel)
	return left, right, resultCh, cancel
}

func awaitResult(t *testing.T, results <-chan Result, within time.Duration) Result {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(within):
		t.Fatalf("relay did not finish within %v", within)
		return Result{}
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// readAll reads from conn until EOF or error and returns what arrived.
func readAll(conn net.Conn) ([]byte, error) {
	var collected bytes.Buffer
	_, err := io.Copy(&collected, conn)
	return collected.Bytes(), err
}

type readOutcome struct {
	data []byte
	err  error
}

// readInBackground runs read on its own goroutine and delivers the outcome.
func readInBackground(read func() ([]byte, error)) <-chan readOutcome {
	outcome := make(chan readOutcome, 1)
	go func() {
		data, err := read()
		outcome <- readOutcome{data, err}
	}()
	return outcome
}

// closeForWriting ends the test's outgoing stream on conn: half-close where
// supported, full close otherwise.
func closeForWriting(t *testing.T, conn net.Conn) {
	t.Helper()
	if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := halfCloser.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// expectUnreadable asserts that conn no longer delivers data, because the
// relay closed its side.
func expectUnreadable(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still readable after the relay closed its side")
	}
}

// trickle writes one byte on conn every interval until stop is closed, and
// reports how many it wrote.
func trickle(conn net.Conn, interval time.Duration, stop <-chan struct{}) <-chan int {
	count := make(chan int, 1)
	go func() {
		written := 0
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				count <- written
				return
			case <-ticker.C:
				if _, err := conn.Write([]byte{'x'}); err != nil {
					count <- written
					return
				}
				written++
			}
		}
	}()
	return count
}

// drain reads and discards everything from conn until it errors.
func drain(conn net.Conn) {
	go io.Copy(io.Discard, conn)
}

// readExactly reads len(want) bytes from conn.
func readExactly(conn net.Conn, want []byte) ([]byte, error) {
	data := make([]byte, len(want))
	_, err := io.ReadFull(conn, data)
	return data, err
}

func TestDataFlowsBothWaysAndCountsMatch(t *testing.T) {
	for kind, newPair := range pairKinds {
		t.Run(kind, func(t *testing.T) {
			left, right, results, _ := relayed(t, newPair, Options{})
			toRight := randomBytes(t, 1<<20)
			toLeft := randomBytes(t, 300*1024)

			// Read exact lengths: a pipe cannot half-close, so EOF would only
			// arrive once the test closed its own end.
			rightOutcome := readInBackground(func() ([]byte, error) { return readExactly(right, toRight) })
			leftOutcome := readInBackground(func() ([]byte, error) { return readExactly(left, toLeft) })

			if _, err := left.Write(toRight); err != nil {
				t.Fatal(err)
			}
			if _, err := right.Write(toLeft); err != nil {
				t.Fatal(err)
			}

			received := <-rightOutcome
			if received.err != nil || !bytes.Equal(received.data, toRight) {
				t.Fatalf("right received %d bytes (err %v), want %d matching", len(received.data), received.err, len(toRight))
			}
			received = <-leftOutcome
			if received.err != nil || !bytes.Equal(received.data, toLeft) {
				t.Fatalf("left received %d bytes (err %v), want %d matching", len(received.data), received.err, len(toLeft))
			}

			closeForWriting(t, left)
			closeForWriting(t, right)
			if kind == "tcp" {
				// Half-closes propagate: both test ends now read EOF.
				if rest, err := readAll(right); err != nil || len(rest) != 0 {
					t.Fatalf("right after half-close: %d bytes, err %v; want EOF", len(rest), err)
				}
				if rest, err := readAll(left); err != nil || len(rest) != 0 {
					t.Fatalf("left after half-close: %d bytes, err %v; want EOF", len(rest), err)
				}
			}

			result := awaitResult(t, results, 5*time.Second)
			if result.Err != nil {
				t.Fatalf("Err = %v, want nil after a clean finish", result.Err)
			}
			if result.BytesAToB != int64(len(toRight)) || result.BytesBToA != int64(len(toLeft)) {
				t.Fatalf("counts = %d/%d, want %d/%d", result.BytesAToB, result.BytesBToA, len(toRight), len(toLeft))
			}
		})
	}
}

func TestHalfCloseLeavesOtherDirectionOpen(t *testing.T) {
	left, right, results, _ := relayed(t, tcpPair, Options{})

	if _, err := left.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := left.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// Right sees the request and then EOF, because the relay half-closed its
	// side after left did.
	received, err := readAll(right)
	if err != nil || string(received) != "request" {
		t.Fatalf("right read %q (err %v), want \"request\" then EOF", received, err)
	}

	// The reply direction must still work after the request direction ended.
	if _, err := right.Write([]byte("reply")); err != nil {
		t.Fatalf("write on the open direction failed: %v", err)
	}
	if err := right.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	received, err = readAll(left)
	if err != nil || string(received) != "reply" {
		t.Fatalf("left read %q (err %v), want \"reply\" then EOF", received, err)
	}

	result := awaitResult(t, results, 5*time.Second)
	if result.Err != nil || result.BytesAToB != 7 || result.BytesBToA != 5 {
		t.Fatalf("result = %+v, want clean with 7/5 bytes", result)
	}
}

func TestIdleTimeoutClosesQuietRelay(t *testing.T) {
	for kind, newPair := range pairKinds {
		t.Run(kind, func(t *testing.T) {
			const idle = 150 * time.Millisecond
			left, _, results, _ := relayed(t, newPair, Options{IdleTimeout: idle})

			started := time.Now()
			result := awaitResult(t, results, 5*time.Second)
			if !errors.Is(result.Err, ErrIdle) {
				t.Fatalf("Err = %v, want ErrIdle", result.Err)
			}
			if elapsed := time.Since(started); elapsed < idle {
				t.Fatalf("relay ended after %v, before the %v idle timeout", elapsed, idle)
			}
			expectUnreadable(t, left)
		})
	}
}

func TestIdleTimeoutSparesActiveRelay(t *testing.T) {
	for kind, newPair := range pairKinds {
		t.Run(kind, func(t *testing.T) {
			const idle = 120 * time.Millisecond
			left, right, results, _ := relayed(t, newPair, Options{IdleTimeout: idle})
			drain(right)

			stop := make(chan struct{})
			written := trickle(left, idle/4, stop)

			select {
			case result := <-results:
				t.Fatalf("relay ended with %v while traffic was flowing", result.Err)
			case <-time.After(4 * idle):
			}
			close(stop)
			sent := <-written

			closeForWriting(t, left)
			if kind == "tcp" {
				right.Close()
			}
			result := awaitResult(t, results, 5*time.Second)
			if result.BytesAToB != int64(sent) {
				t.Fatalf("relayed %d bytes, trickle sent %d", result.BytesAToB, sent)
			}
		})
	}
}

func TestIdleTimeoutJudgesBothDirectionsTogether(t *testing.T) {
	// Only right -> left carries traffic. The left -> right direction's read
	// keeps timing out, and must not end the relay while the other direction
	// is busy.
	const idle = 120 * time.Millisecond
	left, right, results, _ := relayed(t, tcpPair, Options{IdleTimeout: idle})
	drain(left)

	stop := make(chan struct{})
	written := trickle(right, idle/4, stop)
	select {
	case result := <-results:
		t.Fatalf("relay ended with %v while the reverse direction was flowing", result.Err)
	case <-time.After(4 * idle):
	}
	close(stop)
	<-written
}

func TestCancelEndsRelayWithContextError(t *testing.T) {
	for kind, newPair := range pairKinds {
		t.Run(kind, func(t *testing.T) {
			left, _, results, cancel := relayed(t, newPair, Options{})
			cancel()
			result := awaitResult(t, results, 5*time.Second)
			if !errors.Is(result.Err, context.Canceled) {
				t.Fatalf("Err = %v, want context.Canceled", result.Err)
			}
			expectUnreadable(t, left)
		})
	}
}

func TestPeerResetIsReportedAsTheCause(t *testing.T) {
	left, right, results, _ := relayed(t, tcpPair, Options{})
	drain(right)
	if _, err := left.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Linger zero turns Close into a reset instead of an orderly FIN.
	if err := left.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	left.Close()

	result := awaitResult(t, results, 5*time.Second)
	if result.Err == nil || errors.Is(result.Err, ErrIdle) || errors.Is(result.Err, context.Canceled) {
		t.Fatalf("Err = %v, want the reset reported as an I/O error", result.Err)
	}
}

// slowReader reads from conn in small pieces with a pause between them, so a
// transfer spans many watchdog pokes, and returns everything it read.
func slowReader(conn net.Conn, want int, piece int, pause time.Duration) ([]byte, error) {
	data := make([]byte, 0, want)
	chunk := make([]byte, piece)
	for len(data) < want {
		n, err := conn.Read(chunk)
		data = append(data, chunk[:n]...)
		if err != nil {
			return data, err
		}
		time.Sleep(pause)
	}
	return data, nil
}

func TestPokesDuringSlowTransferKeepDataIntact(t *testing.T) {
	for kind, newPair := range pairKinds {
		t.Run(kind, func(t *testing.T) {
			const idle = 40 * time.Millisecond
			left, right, results, _ := relayed(t, newPair, Options{IdleTimeout: idle})
			payload := randomBytes(t, 4<<20)

			// The consumer is slower than the idle interval per piece would
			// allow if progress were not counted, so the relay is poked many
			// times mid-transfer and must neither lose bytes nor give up.
			outcome := readInBackground(func() ([]byte, error) { return slowReader(right, len(payload), 32*1024, 3*time.Millisecond) })
			if _, err := left.Write(payload); err != nil {
				t.Fatal(err)
			}
			received := <-outcome
			if received.err != nil {
				t.Fatalf("consumer stopped early after %d bytes: %v", len(received.data), received.err)
			}
			if !bytes.Equal(received.data, payload) {
				t.Fatalf("payload corrupted in transit (%d bytes)", len(received.data))
			}

			closeForWriting(t, left)
			closeForWriting(t, right)
			result := awaitResult(t, results, 5*time.Second)
			if result.BytesAToB != int64(len(payload)) {
				t.Fatalf("BytesAToB = %d, want %d", result.BytesAToB, len(payload))
			}
			// Over TCP the relay's writes complete as soon as the kernel socket
			// buffer accepts them, so a consumer slower than the idle interval
			// can leave the relay genuinely idle while the kernel still drains
			// to the peer. That is the standard proxy-timeout semantic and not
			// a defect, so ErrIdle is acceptable here; anything else is not.
			if result.Err != nil && !errors.Is(result.Err, ErrIdle) {
				t.Fatalf("Err = %v, want nil or ErrIdle", result.Err)
			}
		})
	}
}

func TestStalledConsumerIsClosedAsIdle(t *testing.T) {
	// Right never reads. Left keeps writing until the socket buffers fill and
	// the relay's write to right blocks; from then on no byte moves, and the
	// stall must be caught even though left is still trying.
	const idle = 100 * time.Millisecond
	left, _, results, _ := relayed(t, tcpPair, Options{IdleTimeout: idle})
	go func() {
		filler := make([]byte, 64*1024)
		for {
			if _, err := left.Write(filler); err != nil {
				return
			}
		}
	}()
	result := awaitResult(t, results, 10*time.Second)
	if !errors.Is(result.Err, ErrIdle) {
		t.Fatalf("Err = %v, want ErrIdle for a consumer that stopped draining", result.Err)
	}
}

func TestCanSpliceOnlyForTCPPairsOnLinux(t *testing.T) {
	tcpNear, tcpFar := tcpPair(t)
	pipeNear, _ := pipePair(t)
	wantTCP := runtime.GOOS == "linux"
	if got := canSplice(tcpNear, tcpFar); got != wantTCP {
		t.Errorf("canSplice(tcp, tcp) = %v on %s, want %v", got, runtime.GOOS, wantTCP)
	}
	if canSplice(tcpNear, pipeNear) || canSplice(pipeNear, tcpFar) {
		t.Error("canSplice reported true for a pair that includes a pipe")
	}
}

func TestBufferPoolReturnsBuffersOfRequestedSize(t *testing.T) {
	small := getBuffer(4096)
	large := getBuffer(65536)
	if len(small) != 4096 || len(large) != 65536 {
		t.Fatalf("sizes = %d and %d", len(small), len(large))
	}
	// A resliced buffer must go back to the pool for its capacity, not its
	// length, or the next getBuffer for that size could hand out a short one.
	putBuffer(small[:10])
	putBuffer(large)
	if again := getBuffer(4096); len(again) != 4096 {
		t.Fatalf("buffer after reslice and return has length %d", len(again))
	}
}

func TestNegativeOptionsMeanDefaults(t *testing.T) {
	left, right, results, _ := relayed(t, pipePair, Options{BufferSize: -1, IdleTimeout: -time.Second})
	select {
	case result := <-results:
		t.Fatalf("relay ended early with %v", result.Err)
	case <-time.After(200 * time.Millisecond):
	}
	left.Close()
	right.Close()
	if result := awaitResult(t, results, 5*time.Second); result.Err != nil {
		t.Fatalf("Err = %v after both ends closed, want nil", result.Err)
	}
}
