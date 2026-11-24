// Command otel-exporter is a Prusa Connect module that exports the fleet's
// telemetry and finished prints over OTLP.
//
// It is one Go process and draws nothing in the app. It reads the host's event
// stream as itself, which takes the observe permission, and exports:
//
//   - one gauge per state path a printer reports, such as /tools/0/nozzle_temp
//     as <prefix>.tools.nozzle_temp with a prusa.tools.index attribute
//   - <prefix>.device.connected, 1 while Prusa Connect holds a connection
//   - one print_job span per finished print, with its start and end times
//   - <prefix>.job.settled, <prefix>.job.duration and <prefix>.job.filament_used
//
// # The route
//
//	GET /status   whether the stream is read and whether exports reach the collector
//
// The path is relative to the mount, so a caller reaches it at
// /api/v1/modules/<namespace>/status.
//
// # Settings
//
// module.yaml declares every setting, with its type and default. The host sends
// them on plugin.configure before the first request, and the pipeline starts
// then. Nothing here reads an environment variable.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.opentelemetry.io/otel"

	"github.com/prusa3d/prusa-connect/sdk/extension"
	"github.com/prusa3d/prusa-connect/sdk/wire"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// What this child reports when the host asks it what it is. The name is the
// manifest's, and the host holds the two to each other.
const (
	name    = "otel-exporter"
	release = extension.Version("0.1.0") // x-release-please-version
)

// openAPI is the fragment describing the route, with every path relative to the
// mount.
//
//go:embed openapi.json
var openAPI []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

// run writes the handshake line and then serves frames until the host closes
// the channel. Standard output carries the protocol, so the log goes to
// standard error, which the host copies into its own log.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	line, err := wire.Handshake{
		Protocol: wire.ProtocolMajor,
		MinMinor: wire.MinMinor,
		MaxMinor: wire.MaxMinor,
		Address:  wire.StdioAddress,
	}.Line()
	if err != nil {
		return fmt.Errorf("render the handshake: %w", err)
	}

	if _, err = os.Stdout.WriteString(line); err != nil {
		return fmt.Errorf("write the handshake: %w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The SDK's own errors go to the host's log, each message once per
	// repeatWindow rather than once per export.
	otel.SetErrorHandler(newSDKErrors(log))

	m := newModule(ctx, log)

	conn, err := wirerpc.NewConn(stdio{}, wirerpc.Options{Role: wirerpc.RoleChild, Methods: m.methods()})
	if err != nil {
		return fmt.Errorf("build the session: %w", err)
	}

	m.attach(conn)

	serveErr := conn.Serve(ctx)

	// The host may close the channel without plugin.shutdown, such as when it
	// stops itself. Flush what is buffered either way.
	if err = m.shutdown(); err != nil {
		m.log.Warn("the last export did not reach the collector", "error", err)
	}

	if serveErr != nil {
		return fmt.Errorf("serve: %w", serveErr)
	}

	return nil
}

// stdio is the pair the host handed over. Close shuts the reading half, which
// is what ends a session whose host has gone quiet.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }
