// Package certs generates, loads, and pins the TLS material sluice uses.
//
// Both ends of every encrypted hop are sluice, so there is no public CA and
// no hostname to verify. Trust is a pin: the client holds the exact
// certificate the server presents and accepts nothing else. For this purpose
// that is stronger than a CA chain and simpler for operators, who copy one
// file. Hostnames, subject alternative names, and expiry play no part in the
// decision. The only way to switch verification off is the explicit insecure
// flag, and every mode that honours it logs the choice loudly.
package certs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// DefaultValidity is long on purpose. Pins do not rely on expiry, so a short
// validity would only schedule an outage for whoever forgets to rotate.
const DefaultValidity = 10 * 365 * 24 * time.Hour

// Generate creates a self-signed ECDSA P-256 certificate and writes it and
// its key as PEM to certPath and keyPath. The key file is created with mode
// 0600 and never overwritten. hosts become subject alternative names; pinning
// ignores them, but they let the same certificate satisfy curl or a browser
// when an operator is debugging.
func Generate(certPath, keyPath string, hosts []string, validity time.Duration) (*x509.Certificate, error) {
	if validity <= 0 {
		validity = DefaultValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certs: generate serial: %w", err)
	}

	// NotBefore sits an hour in the past so a peer whose clock lags does not
	// see a certificate from the future.
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "sluice"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("certs: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("certs: encode key: %w", err)
	}

	if err := writeExclusive(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := writeExclusive(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// writeExclusive creates path with mode and refuses to overwrite an existing
// file, so a running server's key can never be replaced by accident.
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("certs: create %s: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("certs: write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("certs: write %s: %w", path, err)
	}
	return nil
}

// EnsureServer returns the certificate pair at certPath and keyPath,
// generating a new one when neither file exists. Exactly one file present is
// refused: the pair is broken or half copied, and generating over it would
// hide that.
func EnsureServer(certPath, keyPath string, hosts []string) (cert tls.Certificate, generated bool, err error) {
	certExists, err := fileExists(certPath)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	switch {
	case certExists && keyExists:
	case !certExists && !keyExists:
		if _, err := Generate(certPath, keyPath, hosts, DefaultValidity); err != nil {
			return tls.Certificate{}, false, err
		}
		generated = true
	case certExists:
		return tls.Certificate{}, false, fmt.Errorf("certs: %s exists but its key %s does not; restore the key or remove both to generate a new pair", certPath, keyPath)
	default:
		return tls.Certificate{}, false, fmt.Errorf("certs: %s exists but its certificate %s does not; restore the certificate or remove both to generate a new pair", keyPath, certPath)
	}

	cert, err = tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("certs: load %s and %s: %w", certPath, keyPath, err)
	}
	if cert.Leaf == nil {
		// Older Go versions leave Leaf unset; parse it so callers can log
		// the fingerprint.
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, false, fmt.Errorf("certs: parse %s: %w", certPath, err)
		}
		cert.Leaf = leaf
	}
	return cert, generated, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("certs: stat %s: %w", path, err)
}

// LoadPins reads every certificate in the PEM file at path. More than one
// lets a client accept both the old and the new certificate during a
// rotation.
func LoadPins(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("certs: read pins: %w", err)
	}
	pins, err := ParsePEMCertificates(data)
	if err != nil {
		return nil, fmt.Errorf("certs: %s: %w", path, err)
	}
	return pins, nil
}

// ParsePEMCertificates returns every CERTIFICATE block in data, ignoring
// other block types, and fails when there is none.
func ParsePEMCertificates(data []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate %d: %w", len(certificates)+1, err)
		}
		certificates = append(certificates, cert)
	}
	if len(certificates) == 0 {
		return nil, errors.New("no CERTIFICATE block found")
	}
	return certificates, nil
}

// Fingerprint renders the SHA-256 of a certificate the way operators compare
// them, so a log line and an openssl command agree.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	var out strings.Builder
	out.WriteString("sha256:")
	for i, b := range sum {
		if i > 0 {
			out.WriteByte(':')
		}
		fmt.Fprintf(&out, "%02X", b)
	}
	return out.String()
}

// ServerConfig is the TLS configuration a sluice listener uses: this
// certificate, TLS 1.3 only. Both ends are sluice, so there is nothing older
// to accommodate and no cipher negotiation to get wrong.
func ServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientConfig is the TLS configuration a sluice dialer uses. With pins, the
// server's leaf certificate must match one of them byte for byte and nothing
// else is checked. With no pins, insecure must be true and the server is not
// verified at all; the caller is expected to have logged that choice.
func ClientConfig(pins []*x509.Certificate, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	switch {
	case len(pins) > 0:
		// InsecureSkipVerify only disables the standard chain and hostname
		// checks, which cannot apply to a pinned self-signed certificate.
		// VerifyPeerCertificate then enforces the pin on every handshake.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = pinVerifier(pins)
	case insecure:
		cfg.InsecureSkipVerify = true
	default:
		return nil, errors.New("certs: no pinned certificate and insecure verification not enabled; refusing to dial without knowing who answers")
	}
	return cfg, nil
}

// pinVerifier accepts a handshake only when the presented leaf is one of the
// pinned certificates.
func pinVerifier(pins []*x509.Certificate) func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("certs: server presented no certificate")
		}
		leaf := rawCerts[0]
		for _, pin := range pins {
			if bytes.Equal(leaf, pin.Raw) {
				return nil
			}
		}
		presented, err := x509.ParseCertificate(leaf)
		if err != nil {
			return fmt.Errorf("certs: server certificate does not match any pin and cannot be parsed: %w", err)
		}
		return fmt.Errorf("certs: server certificate %s does not match any pinned certificate", Fingerprint(presented))
	}
}
