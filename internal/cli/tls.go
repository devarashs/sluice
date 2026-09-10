package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/tlsrelay"
)

// tlsCommand is `sluice tls`, the encrypted hop between two hosts.
var tlsCommand = &Command{
	Name:    "tls",
	Summary: "Carry connections between two hosts over TLS",
	Subcommands: []*Command{
		{Name: "entry", Summary: "Accept plain connections and send them to a receiver over TLS", Run: runTLSEntry},
		{Name: "receiver", Summary: "Accept TLS connections from entries and deliver them to a target", Run: runTLSReceiver},
	},
}

func runTLSEntry(args []string, stdout, stderr io.Writer) int {
	path, code, stop := parseConfigFlag("sluice tls entry", "entry.json", args, stderr)
	if stop {
		return code
	}
	cfg, err := config.Load(path, (*tlsrelay.EntryConfig).Validate)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	return app.Run(cfg.Common, stderr, func(ctx context.Context, deps app.Deps) error {
		return tlsrelay.RunEntry(ctx, cfg, deps)
	})
}

func runTLSReceiver(args []string, stdout, stderr io.Writer) int {
	path, code, stop := parseConfigFlag("sluice tls receiver", "receiver.json", args, stderr)
	if stop {
		return code
	}
	cfg, err := config.Load(path, (*tlsrelay.ReceiverConfig).Validate)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	return app.Run(cfg.Common, stderr, func(ctx context.Context, deps app.Deps) error {
		return tlsrelay.RunReceiver(ctx, cfg, deps)
	})
}
