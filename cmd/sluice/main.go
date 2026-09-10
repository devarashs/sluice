// Command sluice is the single entry point for every tunnel mode.
//
// All behaviour lives in internal packages; this file only hands the process
// arguments and standard streams to the CLI dispatcher and exits with the code
// it returns, so the whole program is testable without spawning a process.
package main

import (
	"os"

	"github.com/devarashs/sluice/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
