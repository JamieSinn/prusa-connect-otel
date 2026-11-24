// Command otel-exporter is a Prusa Connect module that exports printer
// telemetry as OTLP metrics and finished prints as spans.
//
// The host starts it as a child process and talks to it over stdin/stdout.
// It follows the host's event stream (which needs the observe permission)
// and serves one route, GET /status.
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

// The host refuses to start the module if these differ from module.yaml.
const (
	name    = "otel-exporter"
	release = extension.Version("0.1.0") // x-release-please-version
)

//go:embed openapi.json
var openAPI []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}

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
		return fmt.Errorf("handshake: %w", err)
	}

	if _, err = os.Stdout.WriteString(line); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	// stdout carries the protocol, so logs go to stderr. The host copies
	// stderr into its own log.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	otel.SetErrorHandler(newSDKErrors(log))

	m := newModule(ctx, log)

	conn, err := wirerpc.NewConn(stdio{}, wirerpc.Options{Role: wirerpc.RoleChild, Methods: m.methods()})
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}

	m.attach(conn)

	serveErr := conn.Serve(ctx)

	// The host can close the pipe without sending plugin.shutdown, so flush
	// here as well.
	if err = m.shutdown(); err != nil {
		log.Warn("final export failed", "error", err)
	}

	if serveErr != nil {
		return fmt.Errorf("serve: %w", serveErr)
	}

	return nil
}

// stdio is the connection to the host.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }
