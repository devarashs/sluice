package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/devarashs/sluice/internal/app"
	"github.com/devarashs/sluice/internal/config"
	"github.com/devarashs/sluice/internal/forward"
)

// forwardCommand is `sluice forward`.
var forwardCommand = &Command{
	Name:    "forward",
	Summary: "Forward a local port to a remote address",
	Run:     runForward,
}

func runForward(args []string, stdout, stderr io.Writer) int {
	path, code, stop := parseConfigFlag("sluice forward", "forward.json", args, stderr)
	if stop {
		return code
	}
	cfg, err := config.Load(path, (*forward.Config).Validate)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitError
	}
	return app.Run(cfg.Common, stderr, func(ctx context.Context, deps app.Deps) error {
		return forward.Run(ctx, cfg, deps)
	})
}
