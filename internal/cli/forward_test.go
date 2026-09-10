package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardHelpAndUsageErrors(t *testing.T) {
	code, stdout, stderr := run(t, "forward", "-h")
	if code != ExitOK || stdout != "" || !strings.Contains(stderr, "-config") {
		t.Fatalf("-h: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, _, stderr = run(t, "forward", "-bogus")
	if code != ExitUsage || !strings.Contains(stderr, "bogus") {
		t.Fatalf("unknown flag: code %d, stderr %q", code, stderr)
	}

	code, _, stderr = run(t, "forward", "extra")
	if code != ExitUsage || !strings.Contains(stderr, "unexpected argument extra") {
		t.Fatalf("extra argument: code %d, stderr %q", code, stderr)
	}
}

func TestForwardReportsConfigProblemsWithoutStarting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	code, _, stderr := run(t, "forward", "-config", missing)
	if code != ExitError || !strings.Contains(stderr, "nope.json") {
		t.Fatalf("missing file: code %d, stderr %q", code, stderr)
	}

	invalid := filepath.Join(t.TempDir(), "forward.json")
	if err := os.WriteFile(invalid, []byte(`{"listenAddr": ":0", "targetAddr": "nohost"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = run(t, "forward", "-config", invalid)
	if code != ExitError || !strings.Contains(stderr, "targetAddr") {
		t.Fatalf("invalid config: code %d, stderr %q", code, stderr)
	}
}

func TestRootUsageListsForward(t *testing.T) {
	_, stdout, _ := run(t, "help")
	if !strings.Contains(stdout, "forward") {
		t.Fatalf("usage does not list forward:\n%s", stdout)
	}
}
