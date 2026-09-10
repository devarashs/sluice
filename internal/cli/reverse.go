package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/reverse"
)

// reverseCommand is `sluice reverse`, the multiplexed reverse tunnel.
var reverseCommand = &Command{
	Name:    "reverse",
	Summary: "Expose services behind NAT through a public server",
	Subcommands: []*Command{
		{Name: "server", Summary: "Run the public host that accepts clients and users", Run: runReverseServer},
		{Name: "client", Summary: "Run beside private services and dial out to the server", Run: runReverseClient},
	},
}

func runReverseServer(args []string, stdout, stderr io.Writer) int {
	path, code, stop := parseConfigFlag("sluice reverse server", "reverse-server.json", args, stderr)
	if stop {
		return code
	}
	cfg, err := config.Load(path, (*reverse.ServerConfig).Validate)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	return app.Run(cfg.Common, stderr, func(ctx context.Context, deps app.Deps) error {
		return reverse.RunServer(ctx, cfg, deps)
	})
}

func runReverseClient(args []string, stdout, stderr io.Writer) int {
	path, code, stop := parseConfigFlag("sluice reverse client", "reverse-client.json", args, stderr)
	if stop {
		return code
	}
	cfg, err := config.Load(path, (*reverse.ClientConfig).Validate)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	return app.Run(cfg.Common, stderr, func(ctx context.Context, deps app.Deps) error {
		return reverse.RunClient(ctx, cfg, deps)
	})
}
