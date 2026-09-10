package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/forward"
	"github.com/devarashs/sluice/internal/reverse"
	"github.com/devarashs/sluice/internal/tlsrelay"
)

// TestExampleConfigsAreValid loads every shipped example config through the
// same strict loader and validator the real command uses, so a rename or a
// typo in an example is caught here instead of by a user. Validation is pure:
// it checks and defaults fields without binding ports or reading certificates.
func TestExampleConfigsAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")
	cases := []struct {
		file     string
		validate func([]byte) error
	}{
		{"forward.json", func(b []byte) error { _, err := config.Decode(b, (*forward.Config).Validate); return err }},
		{"tls-entry.json", func(b []byte) error { _, err := config.Decode(b, (*tlsrelay.EntryConfig).Validate); return err }},
		{"tls-receiver.json", func(b []byte) error { _, err := config.Decode(b, (*tlsrelay.ReceiverConfig).Validate); return err }},
		{"reverse-server.json", func(b []byte) error { _, err := config.Decode(b, (*reverse.ServerConfig).Validate); return err }},
		{"reverse-client.json", func(b []byte) error { _, err := config.Decode(b, (*reverse.ClientConfig).Validate); return err }},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, c.file))
			if err != nil {
				t.Fatalf("read %s: %v", c.file, err)
			}
			if err := c.validate(raw); err != nil {
				t.Fatalf("%s failed to validate: %v", c.file, err)
			}
		})
	}
}
