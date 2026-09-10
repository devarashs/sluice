// Package reverse carries the reverse-tunnel protocol: the wire format a
// client behind NAT and a public server speak to expose the client's local
// services through the server.
//
// This file is the wire format alone; the server and client that drive it
// live beside it. The format is deliberately small and every field is
// bounded, because this is the one place an untrusted peer's bytes are
// parsed before it is known to be a sluice client at all. Nothing is
// allocated from a length an attacker controls without a ceiling, every read
// is exact, and the token is checked in constant time.
//
// The sequence, inside an already-established TLS session:
//
//  1. The client sends a ClientHello: a magic marker, the version, a digest
//     of the shared token, and the public addresses it wants the server to
//     open on its behalf (its bindings).
//  2. The server checks the marker, version, and token, tries to bind each
//     address, and replies with a ServerHello: an overall status and, when
//     accepted, one result per binding in the same order.
//  3. yamux multiplexes the session. For each user that connects to a bound
//     public address, the server opens a stream and writes a StreamHeader
//     naming the binding, so the client knows which local service to dial.
package reverse

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/devarashs/sluice/internal/config"
)

// magic marks the start of a ClientHello and a ServerHello so a peer that is
// not speaking this protocol, or a port probe, is rejected on the first four
// bytes instead of being parsed further.
var magic = [4]byte{'S', 'L', 'C', 'r'}

// Version is the protocol version this build speaks. A peer that sends a
// different version is rejected rather than guessed at.
const Version uint8 = 1

// Bounds on everything read from the wire, so a malformed or hostile frame
// cannot make the parser allocate without limit or block forever.
const (
	// MaxBindings caps how many public addresses one client may request.
	MaxBindings = 256
	// maxAddrLen caps one address string. "[ipv6]:port" fits comfortably.
	maxAddrLen = 255
	// maxMessageLen caps a per-binding failure reason in a ServerHello.
	maxMessageLen = 255
	// tokenDigestLen is the fixed width of the token digest on the wire,
	// which is what lets the comparison be constant-time regardless of the
	// token's own length.
	tokenDigestLen = sha256.Size
)

// Errors a reader returns. They are typed so the server can tell a wrong
// token from a wrong version from a truncated frame and answer, or log,
// accordingly.
var (
	// ErrBadMagic means the peer is not speaking the reverse-tunnel protocol.
	ErrBadMagic = errors.New("reverse: not a sluice reverse-tunnel peer")
	// ErrBadVersion means the peer's protocol version is not supported.
	ErrBadVersion = errors.New("reverse: unsupported protocol version")
	// ErrAuthFailed means the token did not match.
	ErrAuthFailed = errors.New("reverse: token rejected")
	// ErrNoBindings means the client asked for nothing, which is a
	// misconfiguration rather than a usable session.
	ErrNoBindings = errors.New("reverse: client declared no bindings")
	// ErrTooManyBindings means the client asked for more than MaxBindings.
	ErrTooManyBindings = fmt.Errorf("reverse: more than %d bindings", MaxBindings)
	// ErrDuplicateBinding means two bindings named the same public address.
	ErrDuplicateBinding = errors.New("reverse: duplicate public address in bindings")
)

// Binding is one public address the client asks the server to listen on.
// Its position in ClientHello.Bindings is its id, which a StreamHeader then
// uses to say which binding a stream belongs to.
type Binding struct {
	// PublicAddr is the host:port the server should accept users on.
	PublicAddr string
}

// ClientHello is the first frame the client sends.
type ClientHello struct {
	Version  uint8
	Bindings []Binding
}

// WriteClientHello writes a ClientHello carrying a digest of token and the
// given bindings. It does not validate the bindings; the server does that on
// read, so one side owns the rules.
func WriteClientHello(w io.Writer, token string, bindings []Binding) error {
	if len(bindings) > MaxBindings {
		return ErrTooManyBindings
	}
	digest := tokenDigest(token)

	buf := make([]byte, 0, 4+1+tokenDigestLen+2)
	buf = append(buf, magic[:]...)
	buf = append(buf, Version)
	buf = append(buf, digest[:]...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(bindings)))
	for _, b := range bindings {
		if len(b.PublicAddr) > maxAddrLen {
			return fmt.Errorf("reverse: binding address %q is longer than %d bytes", b.PublicAddr, maxAddrLen)
		}
		buf = append(buf, byte(len(b.PublicAddr)))
		buf = append(buf, b.PublicAddr...)
	}
	return writeAll(w, buf)
}

// ReadClientHello reads and fully validates a ClientHello, checking the token
// against expectedToken in constant time. Every failure is one of the typed
// errors above so the caller can react to each kind.
func ReadClientHello(r io.Reader, expectedToken string) (*ClientHello, error) {
	header := make([]byte, 4+1+tokenDigestLen+2)
	if err := readFull(r, header); err != nil {
		return nil, err
	}
	if !magicMatches(header[:4]) {
		return nil, ErrBadMagic
	}
	version := header[4]
	if version != Version {
		return nil, ErrBadVersion
	}

	// Compare before touching the bindings, and in constant time, so neither
	// a mismatch nor the token's length is revealed by timing.
	expected := tokenDigest(expectedToken)
	if subtle.ConstantTimeCompare(header[5:5+tokenDigestLen], expected[:]) != 1 {
		return nil, ErrAuthFailed
	}

	count := binary.BigEndian.Uint16(header[5+tokenDigestLen:])
	if count == 0 {
		return nil, ErrNoBindings
	}
	if int(count) > MaxBindings {
		return nil, ErrTooManyBindings
	}

	bindings := make([]Binding, 0, count)
	seen := make(map[string]struct{}, count)
	for i := 0; i < int(count); i++ {
		addr, err := readString(r, maxAddrLen)
		if err != nil {
			return nil, err
		}
		if err := config.ListenAddr(fmt.Sprintf("bindings[%d]", i), addr); err != nil {
			return nil, fmt.Errorf("reverse: %w", err)
		}
		if _, dup := seen[addr]; dup {
			return nil, ErrDuplicateBinding
		}
		seen[addr] = struct{}{}
		bindings = append(bindings, Binding{PublicAddr: addr})
	}
	return &ClientHello{Version: version, Bindings: bindings}, nil
}

// HelloStatus is the overall verdict in a ServerHello.
type HelloStatus uint8

const (
	// StatusAccepted means the token and version were good; per-binding
	// results follow.
	StatusAccepted HelloStatus = 0
	// StatusBadVersion means the server does not speak the client's version.
	StatusBadVersion HelloStatus = 1
	// StatusAuthFailed means the token did not match.
	StatusAuthFailed HelloStatus = 2
	// StatusError means the server failed for a reason unrelated to the
	// client, such as its own resources.
	StatusError HelloStatus = 3
)

// String names a status for logs.
func (s HelloStatus) String() string {
	switch s {
	case StatusAccepted:
		return "accepted"
	case StatusBadVersion:
		return "bad version"
	case StatusAuthFailed:
		return "auth failed"
	case StatusError:
		return "server error"
	default:
		return fmt.Sprintf("status(%d)", uint8(s))
	}
}

// BindResult is the outcome of one requested binding, in the order the client
// asked for them.
type BindResult struct {
	// OK is true when the server bound the address.
	OK bool
	// Message explains a failure, such as "address already in use". Empty
	// when OK.
	Message string
}

// ServerHello is the server's reply to a ClientHello.
type ServerHello struct {
	Status  HelloStatus
	Results []BindResult
}

// RejectHello builds a ServerHello that carries a rejection status and no
// results, from the error ReadClientHello returned.
func RejectHello(readErr error) ServerHello {
	switch {
	case errors.Is(readErr, ErrBadVersion):
		return ServerHello{Status: StatusBadVersion}
	case errors.Is(readErr, ErrAuthFailed):
		return ServerHello{Status: StatusAuthFailed}
	default:
		return ServerHello{Status: StatusError}
	}
}

// WriteServerHello writes hello. When the status is not accepted the results
// are omitted, because there was no binding attempt to report.
func WriteServerHello(w io.Writer, hello ServerHello) error {
	buf := make([]byte, 0, 4+1+2)
	buf = append(buf, magic[:]...)
	buf = append(buf, byte(hello.Status))

	results := hello.Results
	if hello.Status != StatusAccepted {
		results = nil
	}
	if len(results) > MaxBindings {
		return ErrTooManyBindings
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(results)))
	for _, result := range results {
		if len(result.Message) > maxMessageLen {
			result.Message = result.Message[:maxMessageLen]
		}
		var ok byte
		if result.OK {
			ok = 1
		}
		buf = append(buf, ok, byte(len(result.Message)))
		buf = append(buf, result.Message...)
	}
	return writeAll(w, buf)
}

// ReadServerHello reads the server's reply. When the status is accepted it
// insists on exactly expectBindings results, in order, so the client can line
// them up with the bindings it asked for.
func ReadServerHello(r io.Reader, expectBindings int) (*ServerHello, error) {
	header := make([]byte, 4+1+2)
	if err := readFull(r, header); err != nil {
		return nil, err
	}
	if !magicMatches(header[:4]) {
		return nil, ErrBadMagic
	}
	hello := &ServerHello{Status: HelloStatus(header[4])}
	count := binary.BigEndian.Uint16(header[5:])

	if hello.Status != StatusAccepted {
		if count != 0 {
			return nil, fmt.Errorf("reverse: server sent status %q with %d results, want none", hello.Status, count)
		}
		return hello, nil
	}
	if int(count) != expectBindings {
		return nil, fmt.Errorf("reverse: server returned %d binding results, asked for %d", count, expectBindings)
	}

	hello.Results = make([]BindResult, 0, count)
	for i := 0; i < int(count); i++ {
		flags := make([]byte, 1)
		if err := readFull(r, flags); err != nil {
			return nil, err
		}
		message, err := readString(r, maxMessageLen)
		if err != nil {
			return nil, err
		}
		hello.Results = append(hello.Results, BindResult{OK: flags[0] == 1, Message: message})
	}
	return hello, nil
}

// WriteStreamHeader writes the binding id at the start of a freshly opened
// stream, so the client can route it to the right local service.
func WriteStreamHeader(w io.Writer, bindingID uint16) error {
	return writeAll(w, binary.BigEndian.AppendUint16(nil, bindingID))
}

// ReadStreamHeader reads a stream's binding id and checks it is within the
// bindingCount the session negotiated.
func ReadStreamHeader(r io.Reader, bindingCount int) (uint16, error) {
	buf := make([]byte, 2)
	if err := readFull(r, buf); err != nil {
		return 0, err
	}
	id := binary.BigEndian.Uint16(buf)
	if int(id) >= bindingCount {
		return 0, fmt.Errorf("reverse: stream names binding %d but only %d were negotiated", id, bindingCount)
	}
	return id, nil
}

// tokenDigest hashes a token to a fixed width so comparison is constant-time
// whatever the token's length.
func tokenDigest(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func magicMatches(b []byte) bool {
	return subtle.ConstantTimeCompare(b, magic[:]) == 1
}

// readString reads a one-byte length and then that many bytes, refusing a
// length above max before reading the body.
func readString(r io.Reader, max int) (string, error) {
	lengthByte := make([]byte, 1)
	if err := readFull(r, lengthByte); err != nil {
		return "", err
	}
	length := int(lengthByte[0])
	if length > max {
		return "", fmt.Errorf("reverse: string of %d bytes exceeds the %d-byte limit", length, max)
	}
	if length == 0 {
		return "", nil
	}
	body := make([]byte, length)
	if err := readFull(r, body); err != nil {
		return "", err
	}
	return string(body), nil
}

// readFull fills buf, turning an early EOF into a clear truncation error so a
// short frame is not mistaken for a clean end of stream.
func readFull(r io.Reader, buf []byte) error {
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return fmt.Errorf("reverse: truncated frame: %w", err)
		}
		return err
	}
	return nil
}

func writeAll(w io.Writer, buf []byte) error {
	_, err := w.Write(buf)
	return err
}
