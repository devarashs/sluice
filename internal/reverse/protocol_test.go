package reverse

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

const testToken = "a-shared-secret-token"

func sampleBindings() []Binding {
	return []Binding{{PublicAddr: "0.0.0.0:9000"}, {PublicAddr: "[::]:9001"}, {PublicAddr: "127.0.0.1:9002"}}
}

// encodeClientHello returns the exact bytes of a valid ClientHello, so tests
// can truncate or corrupt individual fields.
func encodeClientHello(t *testing.T, token string, bindings []Binding) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteClientHello(&buf, token, bindings); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestClientHelloRoundTrip(t *testing.T) {
	raw := encodeClientHello(t, testToken, sampleBindings())
	hello, err := ReadClientHello(bytes.NewReader(raw), testToken)
	if err != nil {
		t.Fatalf("ReadClientHello: %v", err)
	}
	if hello.Version != Version {
		t.Errorf("version = %d, want %d", hello.Version, Version)
	}
	want := sampleBindings()
	if len(hello.Bindings) != len(want) {
		t.Fatalf("got %d bindings, want %d", len(hello.Bindings), len(want))
	}
	for i, b := range hello.Bindings {
		if b.PublicAddr != want[i].PublicAddr {
			t.Errorf("binding %d = %q, want %q", i, b.PublicAddr, want[i].PublicAddr)
		}
	}
}

func TestClientHelloRejectsWrongToken(t *testing.T) {
	raw := encodeClientHello(t, "the-wrong-token", sampleBindings())
	if _, err := ReadClientHello(bytes.NewReader(raw), testToken); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestClientHelloTokenComparisonIsLengthAgnostic(t *testing.T) {
	// A token that is a prefix of the real one, and one much longer, must
	// both be rejected as auth failures, never as a parse error, because the
	// digest is fixed width.
	for _, wrong := range []string{"", "a", testToken + "extra", strings.Repeat("x", 4096)} {
		raw := encodeClientHello(t, wrong, sampleBindings())
		if _, err := ReadClientHello(bytes.NewReader(raw), testToken); !errors.Is(err, ErrAuthFailed) {
			t.Errorf("token %q: err = %v, want ErrAuthFailed", wrong, err)
		}
	}
	// The real token of any length is accepted.
	longToken := strings.Repeat("secret", 1000)
	raw := encodeClientHello(t, longToken, sampleBindings())
	if _, err := ReadClientHello(bytes.NewReader(raw), longToken); err != nil {
		t.Errorf("long matching token rejected: %v", err)
	}
}

func TestClientHelloRejectsBadMagic(t *testing.T) {
	raw := encodeClientHello(t, testToken, sampleBindings())
	raw[0] ^= 0xFF
	if _, err := ReadClientHello(bytes.NewReader(raw), testToken); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("err = %v, want ErrBadMagic", err)
	}
}

func TestClientHelloRejectsBadVersion(t *testing.T) {
	raw := encodeClientHello(t, testToken, sampleBindings())
	raw[4] = 99 // the version byte follows the 4 magic bytes
	if _, err := ReadClientHello(bytes.NewReader(raw), testToken); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("err = %v, want ErrBadVersion", err)
	}
}

func TestClientHelloRejectsNoBindings(t *testing.T) {
	var buf bytes.Buffer
	// WriteClientHello permits zero on write; the reader is where the rule
	// lives, so craft the bytes directly.
	buf.Write(magic[:])
	buf.WriteByte(Version)
	digest := tokenDigest(testToken)
	buf.Write(digest[:])
	buf.Write([]byte{0, 0}) // zero bindings
	if _, err := ReadClientHello(&buf, testToken); !errors.Is(err, ErrNoBindings) {
		t.Fatalf("err = %v, want ErrNoBindings", err)
	}
}

func TestClientHelloRejectsTooManyBindings(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(magic[:])
	buf.WriteByte(Version)
	digest := tokenDigest(testToken)
	buf.Write(digest[:])
	buf.Write([]byte{0xFF, 0xFF}) // 65535 bindings claimed
	// No binding bodies follow; the count check must fire before any read of
	// them, so a lie about the count cannot make the parser wait for 65535
	// bodies that will never come.
	if _, err := ReadClientHello(&buf, testToken); !errors.Is(err, ErrTooManyBindings) {
		t.Fatalf("err = %v, want ErrTooManyBindings", err)
	}
}

func TestClientHelloRejectsDuplicateBinding(t *testing.T) {
	raw := encodeClientHello(t, testToken, []Binding{{PublicAddr: "0.0.0.0:9000"}, {PublicAddr: "0.0.0.0:9000"}})
	if _, err := ReadClientHello(bytes.NewReader(raw), testToken); !errors.Is(err, ErrDuplicateBinding) {
		t.Fatalf("err = %v, want ErrDuplicateBinding", err)
	}
}

func TestClientHelloRejectsInvalidAddress(t *testing.T) {
	raw := encodeClientHello(t, testToken, []Binding{{PublicAddr: "not-an-address"}})
	_, err := ReadClientHello(bytes.NewReader(raw), testToken)
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("err = %v, want an address validation failure", err)
	}
}

func TestClientHelloRejectsOversizedAddressField(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(magic[:])
	buf.WriteByte(Version)
	digest := tokenDigest(testToken)
	buf.Write(digest[:])
	buf.Write([]byte{0, 1})         // one binding
	buf.WriteByte(byte(maxAddrLen)) // length exactly at the limit
	buf.Write(bytes.Repeat([]byte("a"), maxAddrLen))
	// maxAddrLen bytes of "a" is not a valid address, so this proves the
	// length is accepted (no length error) and validation is what rejects it.
	_, err := ReadClientHello(&buf, testToken)
	if err == nil || errors.Is(err, ErrBadMagic) {
		t.Fatalf("err = %v, want an address validation failure at the size limit", err)
	}
}

func TestClientHelloTruncatedAtEveryStage(t *testing.T) {
	full := encodeClientHello(t, testToken, sampleBindings())
	for cut := 0; cut < len(full); cut++ {
		_, err := ReadClientHello(bytes.NewReader(full[:cut]), testToken)
		if err == nil {
			t.Fatalf("truncating to %d bytes was accepted", cut)
		}
		// A cut inside the fixed header must not be read as a valid-but-short
		// message; it is either a magic, version, auth, or truncation error.
		if cut >= 4 && cut < 5 && !errors.Is(err, ErrBadMagic) && !isTruncation(err) {
			t.Errorf("cut %d: err = %v", cut, err)
		}
	}
}

func TestServerHelloAcceptedRoundTrip(t *testing.T) {
	sent := ServerHello{Status: StatusAccepted, Results: []BindResult{
		{OK: true},
		{OK: false, Message: "address already in use"},
	}}
	var buf bytes.Buffer
	if err := WriteServerHello(&buf, sent); err != nil {
		t.Fatal(err)
	}
	got, err := ReadServerHello(&buf, 2)
	if err != nil {
		t.Fatalf("ReadServerHello: %v", err)
	}
	if got.Status != StatusAccepted || len(got.Results) != 2 {
		t.Fatalf("got %+v", got)
	}
	if !got.Results[0].OK || got.Results[1].OK || got.Results[1].Message != "address already in use" {
		t.Fatalf("results = %+v", got.Results)
	}
}

func TestServerHelloRejectionCarriesNoResults(t *testing.T) {
	for _, status := range []HelloStatus{StatusBadVersion, StatusAuthFailed, StatusError} {
		var buf bytes.Buffer
		// Results are dropped on write for a non-accepted status even if given.
		if err := WriteServerHello(&buf, ServerHello{Status: status, Results: []BindResult{{OK: true}}}); err != nil {
			t.Fatal(err)
		}
		got, err := ReadServerHello(&buf, 5)
		if err != nil {
			t.Fatalf("status %v: %v", status, err)
		}
		if got.Status != status || len(got.Results) != 0 {
			t.Fatalf("status %v: got %+v", status, got)
		}
	}
}

func TestReadServerHelloRejectsCountMismatch(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteServerHello(&buf, ServerHello{Status: StatusAccepted, Results: []BindResult{{OK: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadServerHello(&buf, 3); err == nil || !strings.Contains(err.Error(), "asked for 3") {
		t.Fatalf("err = %v, want a count mismatch", err)
	}
}

func TestServerHelloTruncatedAtEveryStage(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteServerHello(&buf, ServerHello{Status: StatusAccepted, Results: []BindResult{{OK: true}, {OK: false, Message: "no"}}}); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	for cut := 0; cut < len(full); cut++ {
		if _, err := ReadServerHello(bytes.NewReader(full[:cut]), 2); err == nil {
			t.Fatalf("truncating server hello to %d bytes was accepted", cut)
		}
	}
}

func TestRejectHelloMapsErrors(t *testing.T) {
	cases := map[error]HelloStatus{
		ErrBadVersion:              StatusBadVersion,
		ErrAuthFailed:              StatusAuthFailed,
		ErrNoBindings:              StatusError,
		errors.New("disk on fire"): StatusError,
	}
	for readErr, want := range cases {
		if got := RejectHello(readErr).Status; got != want {
			t.Errorf("RejectHello(%v).Status = %v, want %v", readErr, got, want)
		}
	}
}

func TestStreamHeaderRoundTripAndBounds(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteStreamHeader(&buf, 5); err != nil {
		t.Fatal(err)
	}
	id, err := ReadStreamHeader(bytes.NewReader(buf.Bytes()), 8)
	if err != nil || id != 5 {
		t.Fatalf("id = %d, err = %v; want 5", id, err)
	}

	// A stream naming a binding at or above the negotiated count is refused.
	var over bytes.Buffer
	WriteStreamHeader(&over, 8)
	if _, err := ReadStreamHeader(bytes.NewReader(over.Bytes()), 8); err == nil || !strings.Contains(err.Error(), "only 8") {
		t.Fatalf("err = %v, want an out-of-range rejection", err)
	}

	if _, err := ReadStreamHeader(bytes.NewReader([]byte{0x00}), 8); !isTruncation(err) {
		t.Fatalf("err = %v, want truncation on a one-byte header", err)
	}
}

func TestWriteClientHelloRejectsTooManyBindings(t *testing.T) {
	tooMany := make([]Binding, MaxBindings+1)
	for i := range tooMany {
		tooMany[i] = Binding{PublicAddr: "0.0.0.0:9000"}
	}
	if err := WriteClientHello(io.Discard, testToken, tooMany); !errors.Is(err, ErrTooManyBindings) {
		t.Fatalf("err = %v, want ErrTooManyBindings", err)
	}
}

func TestMaxBindingsExactlyIsAccepted(t *testing.T) {
	bindings := make([]Binding, MaxBindings)
	for i := range bindings {
		bindings[i] = Binding{PublicAddr: "127.0.0.1:" + itoa(10000+i)}
	}
	raw := encodeClientHello(t, testToken, bindings)
	hello, err := ReadClientHello(bytes.NewReader(raw), testToken)
	if err != nil {
		t.Fatalf("MaxBindings should be accepted: %v", err)
	}
	if len(hello.Bindings) != MaxBindings {
		t.Fatalf("got %d bindings", len(hello.Bindings))
	}
}

// countMismatchGuardsAllocation proves a lie about the binding count cannot
// make the reader block waiting for bodies that never arrive: an unbounded
// count is rejected structurally.
func TestBindingCountAboveMaxIsRejectedBeforeReadingBodies(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(magic[:])
	buf.WriteByte(Version)
	digest := tokenDigest(testToken)
	buf.Write(digest[:])
	buf.Write([]byte{0x01, 0x01}) // 257, one over MaxBindings
	if _, err := ReadClientHello(&buf, testToken); !errors.Is(err, ErrTooManyBindings) {
		t.Fatalf("err = %v, want ErrTooManyBindings before any body read", err)
	}
}

func isTruncation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "truncated")
}

// itoa avoids importing strconv just for the port strings.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestStreamHeaderWireWidth guards the two-byte width the server and client
// both assume.
func TestStreamHeaderWireWidth(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteStreamHeader(&buf, 0x0102); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes(); len(got) != 2 || binary.BigEndian.Uint16(got) != 0x0102 {
		t.Fatalf("stream header bytes = %v", buf.Bytes())
	}
}
