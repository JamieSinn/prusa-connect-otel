package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"gopkg.in/yaml.v3"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/event"
	"github.com/prusa3d/prusa-connect/sdk/manifest"
	"github.com/prusa3d/prusa-connect/sdk/routesapi"

	"github.com/jamiesinn/prusa-connect-otel/internal/otlpexport"
)

func benchPrinter() deviceapi.Device {
	return deviceapi.Device{
		Kind: "printer", ID: "mk4-1", Name: "Bench MK4", Vendor: "Prusa", Model: "MK4S", Connected: true,
		State: map[string]json.RawMessage{
			"/tools/0/nozzle_temp": json.RawMessage(`214.8`),
			"/state":               json.RawMessage(`"PRINTING"`),
			"/job/display":         json.RawMessage(`"benchy.gcode"`),
			"/tools/0/active":      json.RawMessage(`true`),
		},
	}
}

var bench = map[string]string{
	"prusa.device.id":     "mk4-1",
	"prusa.device.name":   "Bench MK4",
	"prusa.device.vendor": "Prusa",
	"prusa.device.model":  "MK4S",
}

func TestTelemetryExportedAsGauges(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)

	params := h.subscribed()
	for _, want := range followed {
		if !strings.Contains(strings.Join(params.Types, " "), want) {
			t.Errorf("subscription types %v missing %s", params.Types, want)
		}
	}

	if params.Principal != "" {
		t.Errorf("subscribed as %q, want the module itself", params.Principal)
	}

	h.send(busapi.Window{Telemetry: []json.RawMessage{sample(t, "/temperature/bed", 60.5)}, Cursor: "c1"})

	eventually(t, "bed temperature", func() bool {
		m := h.collector.lastMetric("prusa.connect.temperature.bed")
		dp, ok := gaugePoint(m, bench)

		return ok && dp.GetAsDouble() == 60.5 && m.GetUnit() == "Cel"
	})

	// Values from the initial fleet load.
	nozzle := h.collector.lastMetric("prusa.connect.tools.nozzle_temp")
	if dp, ok := gaugePoint(nozzle, map[string]string{"prusa.tools.index": "0", "prusa.device.id": "mk4-1"}); !ok ||
		dp.GetAsDouble() != 214.8 {
		t.Errorf("nozzle_temp = %v, want 214.8 on tool 0", nozzle)
	}

	state := h.collector.lastMetric("prusa.connect.state")
	if _, ok := gaugePoint(state, map[string]string{"prusa.state": "PRINTING"}); !ok {
		t.Errorf("state = %v, want PRINTING", state)
	}

	active := h.collector.lastMetric("prusa.connect.tools.active")
	if dp, ok := gaugePoint(active, nil); !ok || dp.GetAsDouble() != 1 {
		t.Errorf("tools.active = %v, want 1", active)
	}

	if m := h.collector.lastMetric("prusa.connect.job.display"); m != nil {
		t.Errorf("string value exported as a metric: %v", m)
	}

	connected := h.collector.lastMetric("prusa.connect.device.connected")
	if dp, ok := gaugePoint(connected, bench); !ok || dp.GetAsInt() != 1 {
		t.Errorf("device.connected = %v, want 1", connected)
	}
}

func TestDisconnectDropsValues(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	// The sample predates the disconnect in the same window and must not
	// survive it.
	h.send(busapi.Window{
		Telemetry: []json.RawMessage{sample(t, "/temperature/bed", 60)},
		Events: []json.RawMessage{envelope(t, event.TypeDevicePresenceChanged, event.ClassState,
			map[string]any{"device_id": "mk4-1", "driver": "xbuddy", "connected": false})},
	})

	eventually(t, "connected = 0", func() bool {
		dp, ok := gaugePoint(h.collector.lastMetric("prusa.connect.device.connected"), bench)

		return ok && dp.GetAsInt() == 0
	})

	h.collector.mu.Lock()
	newest := h.collector.metrics[len(h.collector.metrics)-1]
	h.collector.mu.Unlock()

	for _, rm := range newest.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() != "prusa.connect.device.connected" && len(m.GetGauge().GetDataPoints()) > 0 {
					t.Errorf("%s still reported after disconnect: %v", m.GetName(), m.GetGauge().GetDataPoints())
				}
			}
		}
	}

	// On reconnect, unchanged values are reloaded from the fleet.
	h.send(busapi.Window{Events: []json.RawMessage{envelope(t, event.TypeDevicePresenceChanged,
		event.ClassState, map[string]any{"device_id": "mk4-1", "driver": "xbuddy", "connected": true})}})

	eventually(t, "nozzle_temp after reconnect", func() bool {
		dp, ok := gaugePoint(h.collector.lastMetric("prusa.connect.tools.nozzle_temp"), bench)

		return ok && dp.GetAsDouble() == 214.8
	})
}

func TestFinishedPrintExportedAsSpan(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	started := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	ended := started.Add(95 * time.Minute)

	h.send(busapi.Window{Events: []json.RawMessage{envelope(t, event.TypeDeviceJobSettled, event.ClassState,
		event.JobSettled{
			JobID: "0199e3a4-9c0e-7d42-8d25-6a1f0c3b7e94", DeviceJobID: "412", DisplayName: "benchy.gcode",
			Started: started, Ended: ended, Outcome: event.JobFailed, MachineState: "STOPPED", Progress: 42,
			TimePrintingS: 5400, TimePrintingKnown: true, FilamentUsedMM: 1234.5, FilamentUsedKnown: true,
		})}})

	eventually(t, "job.settled counter", func() bool {
		m := h.collector.lastMetric("prusa.connect.job.settled")
		for _, dp := range m.GetSum().GetDataPoints() {
			want := map[string]string{"prusa.job.outcome": "failed", "prusa.device.name": "Bench MK4"}
			if hasAttrs(dp.GetAttributes(), want) {
				return dp.GetAsInt() == 1
			}
		}

		return false
	})

	// plugin.shutdown must flush the span before it returns.
	h.shutdown()

	spans := h.collector.spans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}

	span := spans[0]
	if span.GetName() != "print_job" {
		t.Errorf("span name = %q, want print_job", span.GetName())
	}

	if start := time.Unix(0, int64(span.GetStartTimeUnixNano())).UTC(); !start.Equal(started) {
		t.Errorf("span start = %s, want %s", start, started)
	}

	if end := time.Unix(0, int64(span.GetEndTimeUnixNano())).UTC(); !end.Equal(ended) {
		t.Errorf("span end = %s, want %s", end, ended)
	}

	if span.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		t.Errorf("span status = %v, want error", span.GetStatus())
	}

	want := map[string]string{
		"prusa.job.display_name": "benchy.gcode", "prusa.job.outcome": "failed", "prusa.job.device_job_id": "412",
		"prusa.job.time_printing_s": "5400", "prusa.job.filament_used_mm": "1234.5", "prusa.device.model": "MK4S",
	}
	if !hasAttrs(span.GetAttributes(), want) {
		t.Errorf("span attributes = %v, want at least %v", attrMap(span.GetAttributes()), want)
	}
}

func TestPrintWithoutStartNotTimed(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	h.send(busapi.Window{Events: []json.RawMessage{envelope(t, event.TypeDeviceJobSettled, event.ClassState,
		event.JobSettled{JobID: "j-1", Ended: time.Now(), Outcome: event.JobFinished})}})

	eventually(t, "job.settled counter", func() bool {
		for _, dp := range h.collector.lastMetric("prusa.connect.job.settled").GetSum().GetDataPoints() {
			if dp.GetAsInt() == 1 {
				return true
			}
		}

		return false
	})

	// The counter and histogram are recorded together, so they arrive in the
	// same export.
	for _, dp := range h.collector.lastMetric("prusa.connect.job.duration").GetHistogram().GetDataPoints() {
		if dp.GetCount() != 0 {
			t.Errorf("print without a start time was recorded in job.duration: %v", dp)
		}
	}
}

func TestResubscribeFromLastCursor(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	h.send(busapi.Window{Cursor: "cursor-7"})
	h.feed.endSubscription()

	if again := h.subscribed(); again.Resume != "cursor-7" {
		t.Errorf("resubscribed from %q, want cursor-7", again.Resume)
	}
}

// A rejected cursor means restarting from the head, which has no resync flag,
// so the fleet must be reloaded to catch changes made in between.
func TestRejectedCursorReloadsFleet(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	h.send(busapi.Window{Cursor: "cursor-7"})

	gone := benchPrinter()
	gone.Connected = false
	h.fleet.set(gone)

	h.feed.refuseNext(busapi.ErrInvalidSubscription)
	h.feed.endSubscription()

	if rejected := h.subscribed(); rejected.Resume != "cursor-7" {
		t.Fatalf("resubscribed from %q, want cursor-7", rejected.Resume)
	}

	if fresh := h.subscribed(); fresh.Resume != "" {
		t.Fatalf("resubscribed from %q after rejection, want the head", fresh.Resume)
	}

	eventually(t, "connected = 0 from the reloaded fleet", func() bool {
		dp, ok := gaugePoint(h.collector.lastMetric("prusa.connect.device.connected"), bench)

		return ok && dp.GetAsInt() == 0
	})
}

func TestUnknownDeviceLookedUpAndRemovedOnResync(t *testing.T) {
	h := newHarness(t)
	h.mustConfigure(nil)
	h.subscribed()

	h.fleet.set(benchPrinter())
	h.send(busapi.Window{Telemetry: []json.RawMessage{sample(t, "/temperature/bed", 61)}})

	eventually(t, "device attributes from lookup", func() bool {
		_, ok := gaugePoint(h.collector.lastMetric("prusa.connect.temperature.bed"), bench)

		return ok
	})

	h.fleet.set()
	h.send(busapi.Window{Resync: true})

	eventually(t, "device removed", func() bool {
		devices, values, _ := h.module.pipeline.gauges.counts()

		return devices == 0 && values == 0
	})
}

func TestConfigureRejectsInvalidSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		mentions string
	}{
		"no endpoint":      {map[string]any{"otlp_endpoint": ""}, "otlp_endpoint"},
		"bad protocol":     {map[string]any{"otlp_protocol": "udp"}, "protocol"},
		"bad interval":     {map[string]any{"export_interval": "often"}, "export_interval"},
		"bad prefix":       {map[string]any{"metric_prefix": "9lives"}, "metric_prefix"},
		"malformed header": {map[string]any{"otlp_headers": "Bearer SECRET"}, "otlp_headers"},
		"unknown key":      {map[string]any{"otlp_timeout": "5s"}, "otlp_timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)

			err := h.configure(tc.settings)
			if err == nil {
				t.Fatal("configure succeeded")
			}

			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("error %q doesn't mention %s", err, tc.mentions)
			}

			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error %q leaks a header value", err)
			}
		})
	}
}

func TestSecondConfigureRejected(t *testing.T) {
	h := newHarness(t)
	h.mustConfigure(nil)

	if err := h.configure(nil); err == nil || !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("second configure returned %v, want an error", err)
	}
}

// request calls one of the module's routes.
func request(h *harness, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.module.routes().ServeHTTP(rec, httptest.NewRequest(method, path, nil))

	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response %d is not JSON: %v: %s", rec.Code, err, rec.Body)
	}

	return v
}

func TestDeviceRoute(t *testing.T) {
	h := newHarness(t, benchPrinter())

	if r := decodeBody[deviceReport](t, request(h, http.MethodGet, "/devices/mk4-1")); r.Configured {
		t.Errorf("before configure: %+v, want configured = false", r)
	}

	h.mustConfigure(nil)
	h.subscribed()

	// nozzle_temp, state and tools.active; the file name is not exported.
	eventually(t, "the printer's values", func() bool {
		r := decodeBody[deviceReport](t, request(h, http.MethodGet, "/devices/mk4-1"))

		return r.Configured && r.Exported && r.Connected && r.Values == 3
	})

	if r := decodeBody[deviceReport](t, request(h, http.MethodGet, "/devices/other")); r.Exported || r.Values != 0 {
		t.Errorf("unknown printer: %+v, want not exported", r)
	}

	h.send(busapi.Window{Events: []json.RawMessage{envelope(t, event.TypeDevicePresenceChanged, event.ClassState,
		map[string]any{"device_id": "mk4-1", "driver": "xbuddy", "connected": false})}})

	eventually(t, "the disconnected printer", func() bool {
		r := decodeBody[deviceReport](t, request(h, http.MethodGet, "/devices/mk4-1"))

		return r.Exported && !r.Connected && r.Values == 0
	})
}

func TestExportRoute(t *testing.T) {
	h := newHarness(t, benchPrinter())

	if rec := request(h, http.MethodPost, "/export"); rec.Code != http.StatusConflict {
		t.Errorf("before configure: status %d, want 409: %s", rec.Code, rec.Body)
	}

	// A long interval, so only the forced export reaches the collector.
	h.mustConfigure(map[string]any{
		"export_interval": "1h",
		"otlp_headers":    "Authorization=Bearer SECRET",
	})
	h.subscribed()

	rec := request(h, http.MethodPost, "/export")
	if r := decodeBody[exportReport](t, rec); rec.Code != http.StatusOK || !r.OK || r.Error != "" {
		t.Fatalf("POST /export = %d %+v, want ok", rec.Code, r)
	}

	if h.collector.lastMetric("prusa.connect.tools.nozzle_temp") == nil {
		t.Error("POST /export returned before the metrics reached the collector")
	}

	h.collector.mu.Lock()
	h.collector.refuse = 1_000_000
	h.collector.mu.Unlock()

	rec = request(h, http.MethodPost, "/export")
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("POST /export leaks a header value: %s", rec.Body)
	}

	if r := decodeBody[exportReport](t, rec); rec.Code != http.StatusOK || r.OK || r.Error == "" {
		t.Errorf("POST /export against a refusing collector = %d %+v, want ok = false with an error", rec.Code, r)
	}
}

func TestStatusReportsExportFailureUntilRecovery(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.collector.mu.Lock()
	h.collector.refuse = 1_000_000
	h.collector.mu.Unlock()

	h.mustConfigure(map[string]any{"otlp_headers": "Authorization=Bearer SECRET"})
	h.subscribed()

	read := func() statusReport {
		rec := httptest.NewRecorder()
		h.module.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Fatalf("GET /status leaks a header value: %s", rec.Body)
		}

		var r statusReport
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("GET /status: %v", err)
		}

		return r
	}

	eventually(t, "failed metric export", func() bool {
		m := read().Exports[otlpexport.SignalMetrics]

		return m.Error != "" && !m.OK && m.FailingSince != nil
	})

	h.collector.mu.Lock()
	h.collector.refuse = 0
	h.collector.mu.Unlock()

	eventually(t, "failure cleared", func() bool {
		m := read().Exports[otlpexport.SignalMetrics]

		return m.OK && m.Error == "" && m.FailingSince == nil
	})

	h.send(busapi.Window{Cursor: "c1"})

	eventually(t, "stream ok", func() bool {
		r := read()

		return r.Stream.OK && r.LastWindow != nil
	})

	if r := read(); !r.Configured || r.Devices != 1 || r.Values == 0 {
		t.Errorf("GET /status = %+v", r)
	}
}

// Creating instruments while the reader collects must not deadlock (see the
// lock order on gauges).
func TestNewMetricsDuringCollection(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(map[string]any{"export_interval": "1ms"})
	h.subscribed()

	telemetry := make([]json.RawMessage, 0, 300)
	for i := range 300 {
		telemetry = append(telemetry, sample(t, fmt.Sprintf("/sensor_%d/reading", i), i))
	}

	h.send(busapi.Window{Telemetry: telemetry})

	eventually(t, "instrument cap reached", func() bool {
		_, _, metrics := h.module.pipeline.gauges.counts()

		return metrics == maxInstruments
	})

	if h.module.pipeline.gauges.dropped.Load() == 0 {
		t.Error("no values counted as dropped past the cap")
	}
}

// manifestOf parses module.yaml the way the host does, so the tests check
// values rather than formatting.
func manifestOf(t *testing.T) manifest.Manifest {
	t.Helper()

	document, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Parse("module.yaml", document, yaml.Unmarshal)
	if err != nil {
		t.Fatalf("the host would reject module.yaml: %v", err)
	}

	return m
}

func TestManifestDefaults(t *testing.T) {
	defaults := map[string]string{}
	for _, o := range manifestOf(t).Options {
		defaults[o.Key] = o.Default
	}

	for key, want := range map[string]string{
		"otlp_protocol":   defaultProtocol,
		"service_name":    defaultServiceName,
		"export_interval": defaultExportInterval,
		"metric_prefix":   defaultMetricPrefix,
	} {
		got, ok := defaults[key]
		if !ok {
			t.Errorf("module.yaml has no option %s", key)

			continue
		}

		if got != want {
			t.Errorf("module.yaml default for %s = %q, want %q", key, got, want)
		}
	}
}

// The host won't start the module if plugin.info and module.yaml disagree on
// the version. release-please bumps all three files.
func TestVersionsMatch(t *testing.T) {
	declared := manifestOf(t).Version

	if release.String() != declared {
		t.Errorf("main.go version %s != module.yaml version %s", release, declared)
	}

	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}

	if err := json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Info.Version != declared {
		t.Errorf("openapi.json version %s != module.yaml version %s", doc.Info.Version, declared)
	}
}

// Both builder calls must publish under extensionModule. Without module: the
// builder would use the Go module path, and the extension id would change.
func TestWorkflowsPublishUnderExtensionModule(t *testing.T) {
	if got, want := faultDomain.String(), "ca.jamiesinn.connect-otel"; got != want {
		t.Errorf("fault domain = %s, want %s", got, want)
	}

	for _, workflow := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		raw, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatal(err)
		}

		found := false

		for line := range strings.Lines(string(raw)) {
			value, ok := strings.CutPrefix(strings.TrimSpace(line), "module:")
			if !ok {
				continue
			}

			found = true
			value, _, _ = strings.Cut(value, "#")

			if strings.TrimSpace(value) != extensionModule {
				t.Errorf("%s publishes as module %s, want %s", workflow, strings.TrimSpace(value), extensionModule)
			}
		}

		if !found {
			t.Errorf("%s doesn't pass module: %s to the builder", workflow, extensionModule)
		}
	}
}

// The host rejects a module whose openapi.json it can't parse.
func TestOpenAPIFragmentAccepted(t *testing.T) {
	if _, err := routesapi.ParseFragment(openAPI); err != nil {
		t.Fatal(err)
	}
}

// All settings come from module.yaml, so they show up in the UI and API.
func TestNoEnvironmentVariables(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}

		source, readErr := os.ReadFile(e.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}

		if strings.Contains(string(source), "os.Getenv") || strings.Contains(string(source), "os.LookupEnv") {
			t.Errorf("%s reads an environment variable; add a setting to module.yaml instead", e.Name())
		}
	}
}
