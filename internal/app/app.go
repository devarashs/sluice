// Package app runs a mode. It turns the configuration blocks every mode
// shares into a logger, runtime settings, a metrics registry, and an admin
// listener, ties process signals to a context, runs the mode's main function
// until it returns, and turns the outcome into an exit code.
//
// The shape is the same for every mode so that operating one sluice process
// is operating all of them: the same log format, the same admin endpoints,
// the same shutdown sequence. One signal begins a graceful stop that lets
// connections drain; a second signal ends the process at once.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/devarashs/sluice/internal/admin"
	"github.com/devarashs/sluice/internal/logging"
	"github.com/devarashs/sluice/internal/metrics"
	"github.com/devarashs/sluice/internal/process"
	"github.com/devarashs/sluice/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
)

// Common is the configuration every mode shares. Modes embed it in their own
// Config so the blocks sit at the top level of the file.
type Common struct {
	Log     logging.Config `json:"log"`
	Process process.Config `json:"process"`
	Admin   admin.Config   `json:"admin"`
}

// Validate validates each shared block under its JSON name.
func (c *Common) Validate() error {
	if err := c.Log.Validate("log"); err != nil {
		return err
	}
	if err := c.Process.Validate("process"); err != nil {
		return err
	}
	return c.Admin.Validate("admin")
}

// Deps is what a mode receives from Run.
type Deps struct {
	Logger   *slog.Logger
	Registry *prometheus.Registry
	// Ready is how the mode reports readiness to /readyz.
	Ready *Readiness
	// AdminAddr is the bound admin address, or nil when the admin listener
	// is disabled.
	AdminAddr net.Addr
}

// Main is a mode's main loop. It must return once ctx ends, after its own
// drain, and return nil for a clean stop.
type Main func(ctx context.Context, deps Deps) error

// Run runs main with the process signals wired to its context and returns
// the exit code. Logs go to stderr.
func Run(common Common, stderr io.Writer, main Main) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, restore default handling so a second one
		// ends the process immediately instead of waiting out the drain.
		<-ctx.Done()
		stop()
	}()
	return RunContext(ctx, common, stderr, main)
}

// RunContext is Run with a caller-supplied context, for tests and for
// embedding.
func RunContext(ctx context.Context, common Common, stderr io.Writer, main Main) int {
	logger := logging.New(common.Log, stderr)
	logger.Info("starting", "build", version.String())
	process.Apply(common.Process, logger)

	registry := metrics.NewRegistry()
	ready := NewReadiness()
	deps := Deps{Logger: logger, Registry: registry, Ready: ready}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	adminDone := make(chan error, 1)
	if common.Admin.Disabled {
		adminDone <- nil
	} else {
		server := admin.New(common.Admin, ready.Check, registry, logger)
		listener, err := server.Listen()
		if err != nil {
			logger.Error("admin listener failed", "error", err)
			return ExitError
		}
		deps.AdminAddr = listener.Addr()
		go func() { adminDone <- server.Serve(ctx, listener) }()
	}

	mainDone := make(chan error, 1)
	go func() {
		err := main(ctx, deps)
		// Whatever ended main also ends the admin listener.
		cancel()
		mainDone <- err
	}()

	mainErr := <-mainDone
	adminErr := <-adminDone

	exit := ExitOK
	if mainErr != nil && !errors.Is(mainErr, context.Canceled) {
		logger.Error("stopped with error", "error", mainErr)
		exit = ExitError
	}
	if adminErr != nil {
		logger.Error("admin listener stopped with error", "error", adminErr)
		exit = ExitError
	}
	if exit == ExitOK {
		logger.Info("stopped")
	}
	return exit
}
