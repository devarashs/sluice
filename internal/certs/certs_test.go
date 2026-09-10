package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func generateInto(t *testing.T, dir string) (certPath, keyPath string, cert *x509.Certificate) {
	t.Helper()
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	cert, err := Generate(certPath, keyPath, []string{"relay.example", "203.0.113.5"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, cert
}

func TestGenerateWritesAnECDSAPairWithSANsAndSafeKeyMode(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, cert := generateInto(t, dir)

	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || cert.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatalf("public key is %T, want ECDSA P-256", cert.PublicKey)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "relay.example" {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("203.0.113.5")) {
		t.Errorf("IPAddresses = %v", cert.IPAddresses)
	}
	if cert.NotBefore.After(time.Now()) {
		t.Errorf("NotBefore %v is in the future", cert.NotBefore)
	}
	if remaining := time.Until(cert.NotAfter); remaining < DefaultValidity-24*time.Hour {
		t.Errorf("validity %v, want about %v", remaining, DefaultValidity)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("key file mode = %o, want 600", mode)
		}
	}

	loaded, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("the written pair does not load: %v", err)
	}
	if len(loaded.Certificate) != 1 {
		t.Errorf("loaded %d certificates, want 1", len(loaded.Certificate))
	}
}

func TestGenerateRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := generateInto(t, dir)
	if _, err := Generate(certPath, keyPath, nil, 0); err == nil {
		t.Fatal("second Generate over existing files should fail")
	}
}

func TestEnsureServerGeneratesOnceThenLoads(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "s.crt")
	keyPath := filepath.Join(dir, "s.key")

	first, generated, err := EnsureServer(certPath, keyPath, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Fatal("first call should generate")
	}
	second, generated, err := EnsureServer(certPath, keyPath, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Fatal("second call should load, not generate")
	}
	if Fingerprint(first.Leaf) != Fingerprint(second.Leaf) {
		t.Fatal("loaded certificate differs from the generated one")
	}
}

func TestEnsureServerRefusesHalfAPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := generateInto(t, dir)

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureServer(certPath, keyPath, nil); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("err = %v, want a complaint about the missing key", err)
	}

	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(certPath, keyPath, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureServer(certPath, keyPath, nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v, want a complaint about the missing certificate", err)
	}
}

func TestLoadPinsReadsOneOrMoreCertificatesAndRejectsNone(t *testing.T) {
	dir := t.TempDir()
	certA, _, a := generateInto(t, t.TempDir())
	certB, _, b := generateInto(t, t.TempDir())

	pins, err := LoadPins(certA)
	if err != nil || len(pins) != 1 || Fingerprint(pins[0]) != Fingerprint(a) {
		t.Fatalf("single pin: %v, %d", err, len(pins))
	}

	pemA, _ := os.ReadFile(certA)
	pemB, _ := os.ReadFile(certB)
	bundle := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(bundle, append(append([]byte("# comment\n"), pemA...), pemB...), 0o644); err != nil {
		t.Fatal(err)
	}
	pins, err = LoadPins(bundle)
	if err != nil || len(pins) != 2 || Fingerprint(pins[1]) != Fingerprint(b) {
		t.Fatalf("bundle: %v, %d pins", err, len(pins))
	}

	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not pem at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPins(empty); err == nil || !strings.Contains(err.Error(), "no CERTIFICATE") {
		t.Fatalf("err = %v, want no CERTIFICATE block", err)
	}
	if _, err := LoadPins(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing file should error")
	}
}

func TestClientConfigRefusesNeitherPinNorInsecure(t *testing.T) {
	if _, err := ClientConfig(nil, false); err == nil {
		t.Fatal("no pins and not insecure must be refused")
	}
	cfg, err := ClientConfig(nil, true)
	if err != nil || !cfg.InsecureSkipVerify || cfg.VerifyPeerCertificate != nil {
		t.Fatalf("insecure config = %+v, %v", cfg, err)
	}
}

// startTLSServer serves one handshake per accepted connection and returns
// the address.
func startTLSServer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", ServerConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				// Drive the handshake and then hold until the client is done.
				if tlsConn, ok := conn.(*tls.Conn); ok {
					_ = tlsConn.Handshake()
				}
				buf := make([]byte, 1)
				conn.Read(buf)
			}()
		}
	}()
	return listener.Addr().String()
}

func TestPinnedHandshakeSucceedsOnlyAgainstThePinnedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, leaf := generateInto(t, dir)
	serverCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	addr := startTLSServer(t, serverCert)

	pinned, err := ClientConfig([]*x509.Certificate{leaf}, false)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", addr, pinned)
	if err != nil {
		t.Fatalf("pinned dial failed: %v", err)
	}
	if version := conn.ConnectionState().Version; version != tls.VersionTLS13 {
		t.Errorf("negotiated TLS version %x, want 1.3", version)
	}
	conn.Close()

	otherDir := filepath.Join(dir, "other")
	if err := os.Mkdir(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, otherLeaf := generateInto(t, otherDir)
	wrongPin, err := ClientConfig([]*x509.Certificate{otherLeaf}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.Dial("tcp", addr, wrongPin); err == nil || !strings.Contains(err.Error(), "does not match any pinned certificate") {
		t.Fatalf("dial with the wrong pin: err = %v, want a pin mismatch naming the fingerprint", err)
	}

	rotation, err := ClientConfig([]*x509.Certificate{otherLeaf, leaf}, false)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := tls.Dial("tcp", addr, rotation); err != nil {
		t.Fatalf("dial with old and new pins should succeed: %v", err)
	} else {
		conn.Close()
	}

	insecure, _ := ClientConfig(nil, true)
	if conn, err := tls.Dial("tcp", addr, insecure); err != nil {
		t.Fatalf("insecure dial should succeed: %v", err)
	} else {
		conn.Close()
	}
}

func TestFingerprintFormat(t *testing.T) {
	_, _, cert := generateInto(t, t.TempDir())
	fp := Fingerprint(cert)
	if !strings.HasPrefix(fp, "sha256:") || len(fp) != len("sha256:")+32*3-1 {
		t.Fatalf("fingerprint %q has the wrong shape", fp)
	}
}
