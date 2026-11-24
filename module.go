package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/fault"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"

	"github.com/jamiesinn/prusa-connect-otel/internal/otlpexport"
)

const (
	// flushBudget bounds the final export on shutdown, so an unresponsive
	// collector can't stop the process from exiting.
	flushBudget = 5 * time.Second
	// exportBudget bounds an export requested through POST /export.
	exportBudget = 15 * time.Second
)

var (
	errConfiguredTwice = errors.New("already configured; the host restarts the module to change settings")
	errStopping        = errors.New("the module is shutting down")
)

// module holds the process state. The pipeline is created by configure and
// removed by shutdown; mu guards it.
type module struct {
	ctx    context.Context
	log    *slog.Logger
	status *status
	conn   host

	mu       sync.Mutex
	pipeline *pipeline
	stopped  bool
}

func newModule(ctx context.Context, log *slog.Logger) *module {
	return &module{ctx: ctx, log: log, status: newStatus(log)}
}

// attach sets the host connection. Call it before serving.
func (m *module) attach(conn host) { m.conn = conn }

func (m *module) methods() wirerpc.Methods {
	return wirerpc.Methods{
		wirerpc.MethodInfo: func(context.Context, wirerpc.Call) (any, error) {
			return routesapi.Describe(wirerpc.Info{
				Name:     name,
				Version:  release,
				Contract: wirerpc.Contract,
				Serves:   []wirerpc.Method{wirerpc.MethodConfigure},
			}, openAPI)
		},
		wirerpc.MethodConfigure: wirerpc.Configure(func(_ context.Context, s Settings) error {
			return m.configure(s)
		}),
		wirerpc.MethodShutdown: func(context.Context, wirerpc.Call) (any, error) {
			if err := m.shutdown(); err != nil {
				m.log.Warn("final export failed", "error", err)
			}

			return nil, nil
		},
		routesapi.MethodServe: routesapi.Serve(m.routes()),
	}
}

// configure builds the exporters and starts the pipeline. A returned error
// is shown to the operator on the module's instance.
func (m *module) configure(s Settings) error {
	cfg, err := s.resolve()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return errStopping
	}

	if m.pipeline != nil {
		return errConfiguredTwice
	}

	cfg.otlp.OnExport = m.status.exported

	providers, err := otlpexport.Setup(m.ctx, cfg.otlp)
	if err != nil {
		return err
	}

	p, err := newPipeline(m.log, providers, cfg.prefix, m.status)
	if err != nil {
		return errors.Join(err, providers.Shutdown(m.ctx))
	}

	endpoint := redacted(cfg.otlp.Endpoint)
	m.status.configuredFor(endpoint, cfg.otlp.Protocol)
	p.start(m.ctx, m.conn)
	m.pipeline = p

	m.log.Info("exporting", "endpoint", endpoint, "protocol", cfg.otlp.Protocol,
		"interval", cfg.otlp.ExportInterval, "prefix", cfg.prefix)

	return nil
}

// shutdown stops the pipeline and flushes pending data. Only the first call
// does anything.
func (m *module) shutdown() error {
	m.mu.Lock()
	p := m.pipeline
	m.pipeline, m.stopped = nil, true
	m.mu.Unlock()

	if p == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), flushBudget)
	defer cancel()

	return p.stop(ctx)
}

// routes serves the module's HTTP API. The host authorizes requests before
// they get here.
func (m *module) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		report := m.status.report()

		if p := m.current(); p != nil {
			report.Devices, report.Values, report.Metrics = p.gauges.counts()
			report.Dropped = p.gauges.dropped.Load()
		}

		writeJSON(w, report)
	})

	mux.HandleFunc("GET /devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		var report deviceReport

		if p := m.current(); p != nil {
			report.Configured = true
			report.Exported, report.Connected, report.Values = p.gauges.deviceState(r.PathValue("id"))
		}

		writeJSON(w, report)
	})

	mux.HandleFunc("POST /export", func(w http.ResponseWriter, r *http.Request) {
		p := m.current()
		if p == nil {
			writeFault(w, r, notConfigured.New("otlp_endpoint is not set"))

			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), exportBudget)
		defer cancel()

		var report exportReport
		if err := p.flush(ctx); err != nil {
			report.Error = err.Error()
		} else {
			report.OK = true
		}

		writeJSON(w, report)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeFault(w, r, routeNotFound.
			New("no route at "+r.URL.Path).
			With(fault.StringParam("path", r.URL.Path)))
	})

	return mux
}

// current returns the running pipeline, or nil before configure and after
// shutdown.
func (m *module) current() *pipeline {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.pipeline
}

// deviceReport is the body of GET /devices/{id}.
type deviceReport struct {
	Configured bool `json:"configured"`
	Exported   bool `json:"exported"`
	Connected  bool `json:"connected"`
	Values     int  `json:"values"`
}

// exportReport is the body of POST /export.
type exportReport struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// redacted masks any password in the endpoint URL.
func redacted(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "(invalid endpoint)"
	}

	return u.Redacted()
}
