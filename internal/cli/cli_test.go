package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// run exercises the real root tree and returns exit code, stdout, stderr.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersionPrintsBuildIdentityToStdout(t *testing.T) {
	code, stdout, stderr := run(t, "version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.HasPrefix(stdout, "sluice dev (commit unknown") {
		t.Fatalf("stdout = %q, want the default build identity", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	code, stdout, stderr := run(t, "version", "--json")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "takes no arguments") || !strings.Contains(stderr, "--json") {
		t.Fatalf("stderr = %q, want a message naming the stray argument", stderr)
	}
}

func TestNoCommandPrintsUsageToStderrAsUsageError(t *testing.T) {
	code, stdout, stderr := run(t)
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "Usage:") || !strings.Contains(stderr, "sluice <command>") {
		t.Fatalf("stderr = %q, want the root usage", stderr)
	}
}

func TestHelpSpellingsPrintUsageToStdoutAndSucceed(t *testing.T) {
	for _, spelling := range []string{"help", "-h", "--help"} {
		t.Run(spelling, func(t *testing.T) {
			code, stdout, stderr := run(t, spelling)
			if code != ExitOK {
				t.Fatalf("exit code = %d, want %d", code, ExitOK)
			}
			if !strings.Contains(stdout, "Usage:") {
				t.Fatalf("stdout = %q, want usage", stdout)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestUsageListsEveryRegisteredCommandAndHelp(t *testing.T) {
	_, stdout, _ := run(t, "help")
	for _, sub := range rootCommand().Subcommands {
		if !strings.Contains(stdout, sub.Name) || !strings.Contains(stdout, sub.Summary) {
			t.Errorf("usage is missing %q with summary %q:\n%s", sub.Name, sub.Summary, stdout)
		}
	}
	if !strings.Contains(stdout, "help") {
		t.Errorf("usage does not mention help:\n%s", stdout)
	}
}

func TestUnknownCommandIsNamedAndIsAUsageError(t *testing.T) {
	code, stdout, stderr := run(t, "teleport")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown command "teleport"`) {
		t.Fatalf("stderr = %q, want the unknown command named", stderr)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q, want usage after the error", stderr)
	}
}

// nestedTree is a two-level tree standing in for the mode groups that later
// issues register, so nesting is tested before any mode exists.
func nestedTree(leafArgs *[]string) *Command {
	return &Command{
		Name:    "sluice",
		Summary: "Root",
		Subcommands: []*Command{
			{
				Name:    "tls",
				Summary: "Encrypted hop",
				Subcommands: []*Command{
					{Name: "entry", Summary: "Dial side", Run: func(args []string, _, _ io.Writer) int {
						*leafArgs = args
						return 7
					}},
					{Name: "receiver", Summary: "Listen side", Run: func([]string, io.Writer, io.Writer) int { return ExitOK }},
				},
			},
		},
	}
}

func TestGroupDispatchesToNestedLeafWithRemainingArgs(t *testing.T) {
	var got []string
	var stdout, stderr bytes.Buffer
	code := dispatch(nestedTree(&got), []string{"sluice"}, []string{"tls", "entry", "-config", "x.json"}, &stdout, &stderr)
	if code != 7 {
		t.Fatalf("exit code = %d, want the leaf's 7", code)
	}
	if strings.Join(got, " ") != "-config x.json" {
		t.Fatalf("leaf received %q, want the arguments after its name", got)
	}
}

func TestGroupWithoutSubcommandListsItsChildrenAsUsageError(t *testing.T) {
	var got []string
	var stdout, stderr bytes.Buffer
	code := dispatch(nestedTree(&got), []string{"sluice"}, []string{"tls"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	out := stderr.String()
	for _, want := range []string{"sluice tls <command>", "entry", "receiver", "Dial side", "Listen side"} {
		if !strings.Contains(out, want) {
			t.Errorf("group usage is missing %q:\n%s", want, out)
		}
	}
}

func TestUnknownSubcommandInGroupNamesTheFullPath(t *testing.T) {
	var got []string
	var stdout, stderr bytes.Buffer
	code := dispatch(nestedTree(&got), []string{"sluice"}, []string{"tls", "exit"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), `sluice tls: unknown command "exit"`) {
		t.Fatalf("stderr = %q, want the group path and the bad name", stderr.String())
	}
}

func TestChildPathDoesNotAliasParent(t *testing.T) {
	parent := make([]string, 1, 4)
	parent[0] = "sluice"
	first := childPath(parent, "tls")
	second := childPath(parent, "reverse")
	if first[1] != "tls" || second[1] != "reverse" {
		t.Fatalf("sibling paths clobbered each other: %v %v", first, second)
	}
}
