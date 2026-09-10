package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTLSGroupAndSubcommandUsage(t *testing.T) {
	code, _, stderr := run(t, "tls")
	if code != ExitUsage || !strings.Contains(stderr, "entry") || !strings.Contains(stderr, "receiver") {
		t.Fatalf("bare tls: code %d, stderr %q", code, stderr)
	}
	for _, sub := range []string{"entry", "receiver"} {
		code, stdout, stderr := run(t, "tls", sub, "-h")
		if code != ExitOK || stdout != "" || !strings.Contains(stderr, "-config") {
			t.Errorf("tls %s -h: code %d, stdout %q, stderr %q", sub, code, stdout, stderr)
		}
	}
}

func TestTLSSubcommandsReportConfigProblems(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "entry.json")
	if err := os.WriteFile(entry, []byte(`{"listenAddr": ":0", "receiverAddr": "h:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "tls", "entry", "-config", entry)
	if code != ExitError || !strings.Contains(stderr, "pinnedCertFile") {
		t.Fatalf("entry without a pin: code %d, stderr %q", code, stderr)
	}

	receiver := filepath.Join(dir, "receiver.json")
	if err := os.WriteFile(receiver, []byte(`{"listenAddr": ":0", "targetAddr": "h:1", "certFile": "a", "keyFile": "a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = run(t, "tls", "receiver", "-config", receiver)
	if code != ExitError || !strings.Contains(stderr, "keyFile") {
		t.Fatalf("receiver with one file for both: code %d, stderr %q", code, stderr)
	}
}
