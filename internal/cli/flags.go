package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// parseConfigFlag handles the -config flag every mode shares. It returns the
// configuration path, or an exit code and stop=true when the command must
// end here because help was printed or the arguments were wrong.
func parseConfigFlag(command, defaultPath string, args []string, stderr io.Writer) (path string, exitCode int, stop bool) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "path to the configuration file")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage:\n  %s [-config %s]\n\nFlags:\n", command, defaultPath)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", ExitOK, true
		}
		return "", ExitUsage, true
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %s\n\n", command, strings.Join(flags.Args(), " "))
		flags.Usage()
		return "", ExitUsage, true
	}
	return *configPath, ExitOK, false
}
