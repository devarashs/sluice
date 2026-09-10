// Package cli parses the sluice command line and dispatches to the mode that
// was asked for.
//
// The command tree is plain data so it can be inspected in tests and extended
// by each mode without touching the dispatcher. A group such as `sluice tls`
// holds subcommands; a leaf such as `sluice tls entry` holds a Run function
// that receives the arguments after its name and owns its own flag parsing.
//
// Exit codes follow the flag package convention: 0 on success, 2 on a usage
// error, and whatever the leaf returns otherwise, which is 1 for a runtime
// failure.
package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/devarashs/sluice/internal/version"
)

// Process exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Command is a node in the command tree. Exactly one of Run or Subcommands is
// set: a leaf runs, a group dispatches to its children.
type Command struct {
	// Name is the word typed on the command line, such as "entry".
	Name string
	// Summary is the one-line description shown in help listings. It reads as
	// a sentence fragment starting with a capital, such as "Print the build
	// version", because usage output appends the full stop.
	Summary string
	// Run executes a leaf command with the arguments that followed its name
	// and returns the process exit code. Nil for a group.
	Run func(args []string, stdout, stderr io.Writer) int
	// Subcommands are the children of a group. Nil for a leaf.
	Subcommands []*Command
}

// Run dispatches args, which is os.Args without the program name, against the
// root command tree and returns the process exit code. Output goes to the
// given writers so the whole CLI can be exercised in-process.
func Run(args []string, stdout, stderr io.Writer) int {
	return dispatch(rootCommand(), []string{"sluice"}, args, stdout, stderr)
}

// rootCommand builds the tree of commands the binary supports. Each mode adds
// its group here when it is implemented; nothing is registered before it works.
func rootCommand() *Command {
	return &Command{
		Name:    "sluice",
		Summary: "Moves TCP connections between hosts",
		Subcommands: []*Command{
			forwardCommand,
			{Name: "version", Summary: "Print the build version", Run: runVersion},
		},
	}
}

// runVersion prints the build identity. It takes no arguments, and rejects any
// rather than ignoring them, so a typo like `sluice version --json` is caught
// instead of silently doing something else than the operator expected.
func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "sluice version: takes no arguments, got %s\n", strings.Join(args, " "))
		return ExitUsage
	}
	fmt.Fprintln(stdout, version.String())
	return ExitOK
}

// dispatch resolves one level of the tree. path holds the command names
// reached so far and is used to render invocations such as "sluice tls entry".
func dispatch(cmd *Command, path []string, args []string, stdout, stderr io.Writer) int {
	if cmd.Run != nil {
		return cmd.Run(args, stdout, stderr)
	}
	if len(args) == 0 {
		writeUsage(stderr, cmd, path)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeUsage(stdout, cmd, path)
		return ExitOK
	}
	for _, sub := range cmd.Subcommands {
		if sub.Name == args[0] {
			return dispatch(sub, childPath(path, sub.Name), args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n\n", strings.Join(path, " "), args[0])
	writeUsage(stderr, cmd, path)
	return ExitUsage
}

// childPath returns path extended by name as a fresh slice. Appending to the
// caller's slice directly could alias its backing array between siblings.
func childPath(path []string, name string) []string {
	next := make([]string, 0, len(path)+1)
	next = append(next, path...)
	return append(next, name)
}

// writeUsage renders the help for a group: what it does, how it is invoked,
// and the subcommands it accepts, aligned in columns.
func writeUsage(w io.Writer, cmd *Command, path []string) {
	invocation := strings.Join(path, " ")
	fmt.Fprintf(w, "%s.\n\nUsage:\n  %s <command> [arguments]\n\nCommands:\n", cmd.Summary, invocation)
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, sub := range cmd.Subcommands {
		fmt.Fprintf(tw, "  %s\t%s\n", sub.Name, sub.Summary)
	}
	fmt.Fprintf(tw, "  help\tPrint this help\n")
	tw.Flush()
	fmt.Fprintf(w, "\nRun \"%s <command> -h\" for a command's flags.\n", invocation)
}
