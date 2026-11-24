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

// flushBudget bounds the last export on shutdown. The host kills a child that
// does not stop, so a collector that does not answer must not hold it.
const flushBudget = 5 * time.Second

// errConfiguredTwice refuses a second plugin.configure. The host sends one per
// process, and a second pipeline would export every value twice.
var errConfiguredTwice = errors.New("this instance is already configured; the host restarts it to change settings")

// module is the process's state: the session, the status the route reports,
// and the pipeline once the host has configured it.
//
// The pipeline is set once, by plugin.configure on the wire goroutine, and
// taken away once, by shutdown; mu guards it.
type module struct {
	ctx    context.Context // the process's lifetime; the pipeline runs under it
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

// attach hands the module the session it calls the host on. It is called
// before the session serves, so before any handler can run.
func (m *module) attach(conn host) { m.conn = conn }

// methods is the lifecycle set every child answers, plus the module's routes.
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
		wirerpc.MethodConfigure: wirerpc.Configure(func(_ context.Context, sent Settings) error {
			return m.configure(sent)
		}),
		wirerpc.MethodShutdown: func(context.Context, wirerpc.Call) (any, error) {
			// Answering is the last thing the host waits for, so the flush
			// happens first.
			if err := m.shutdown(); err != nil {
				m.log.Warn("the last export did not reach the collector", "error", err)
			}

			return nil, nil
		},
		routesapi.MethodServe: routesapi.Serve(m.routes()),
	}
}

// configure builds the exporters and starts following the stream. An error
// refuses the start, and the operator reads it on the instance.
func (m *module) configure(sent Settings) error {
	settings, err := sent.resolve()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return errors.New("this instance is stopping")
	}

	if m.pipeline != nil {
		return errConfiguredTwice
	}

	settings.otlp.OnExport = m.status.exported

	providers, err := otlpexport.Setup(m.ctx, settings.otlp)
	if err != nil {
		return err //nolint:wrapcheck // the message already names the setting
	}

	p, err := newPipeline(m.log, providers, settings.prefix, m.status)
	if err != nil {
		return errors.Join(err, providers.Shutdown(m.ctx))
	}

	m.status.configuredFor(redacted(settings.otlp.Endpoint), settings.otlp.Protocol)
	p.start(m.ctx, m.conn)
	m.pipeline = p

	m.log.Info("exporting", "endpoint", redacted(settings.otlp.Endpoint), "protocol", settings.otlp.Protocol,
		"interval", settings.otlp.ExportInterval, "prefix", settings.prefix)

	return nil
}

// shutdown stops the pipeline and flushes it, once. Later calls do nothing.
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

// routes is the module's HTTP surface. The host authorizes every request
// before it arrives.
func (m *module) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		report := m.status.report()

		m.mu.Lock()
		if m.pipeline != nil {
			report.Devices, report.Values, report.Metrics = m.pipeline.gauges.counts()
			report.Dropped = m.pipeline.gauges.dropped.Load()
		}
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		refuse(w, r, RouteNotFound.
			New("this module serves no route at "+r.URL.Path).
			With(fault.StringParam("path", r.URL.Path)))
	})

	return mux
}

// redacted is the endpoint with any password in it masked, for a log line or
// the status route.
func redacted(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "(unparsable endpoint)"
	}

	return u.Redacted()
}
