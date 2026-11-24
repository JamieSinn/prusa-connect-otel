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

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/event"

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

// A value that settled before the module started is read off the fleet, and a
// value the stream reports later reaches the collector as a gauge named after
// its path, with the printer's identity and the tool index as attributes.
func TestTelemetryReachesTheCollectorAsGauges(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)

	params := h.subscribed()
	for _, want := range followed {
		if !strings.Contains(strings.Join(params.Types, " "), want) {
			t.Errorf("the subscription does not follow %s: %v", want, params.Types)
		}
	}

	if params.Principal != "" {
		t.Errorf("the subscription is opened for %q, want the module's own work", params.Principal)
	}

	h.send(busapi.Window{Telemetry: []json.RawMessage{sample(t, "/temperature/bed", 60.5)}, Cursor: "c1"})

	eventually(t, "the bed temperature", func() bool {
		m := h.collector.lastMetric("prusa.connect.temperature.bed")
		dp, ok := gaugePoint(m, bench)

		return ok && dp.GetAsDouble() == 60.5 && m.GetUnit() == "Cel"
	})

	nozzle := h.collector.lastMetric("prusa.connect.tools.nozzle_temp")
	if dp, ok := gaugePoint(
		nozzle,
		map[string]string{"prusa.tools.index": "0", "prusa.device.id": "mk4-1"},
	); !ok ||
		dp.GetAsDouble() != 214.8 {
		t.Errorf("the seeded nozzle temperature is %v, want 214.8 on tool 0", nozzle)
	}

	state := h.collector.lastMetric("prusa.connect.state")
	if _, ok := gaugePoint(state, map[string]string{"prusa.state": "PRINTING"}); !ok {
		t.Errorf("the printer state is %v, want PRINTING", state)
	}

	active := h.collector.lastMetric("prusa.connect.tools.active")
	if dp, ok := gaugePoint(active, nil); !ok || dp.GetAsDouble() != 1 {
		t.Errorf("a boolean true is %v, want 1", active)
	}

	if m := h.collector.lastMetric("prusa.connect.job.display"); m != nil {
		t.Errorf("a file name became a metric: %v", m)
	}

	connected := h.collector.lastMetric("prusa.connect.device.connected")
	if dp, ok := gaugePoint(connected, bench); !ok || dp.GetAsInt() != 1 {
		t.Errorf("device.connected is %v, want 1", connected)
	}
}

// A printer that goes away stops reporting values it can no longer vouch for,
// and reports connected 0 instead.
func TestADisconnectedPrinterStopsReportingValues(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	// The sample in the same window is from before the printer left, and must
	// not outlive the presence change.
	h.send(busapi.Window{
		Telemetry: []json.RawMessage{sample(t, "/temperature/bed", 60)},
		Events: []json.RawMessage{envelope(t, event.TypeDevicePresenceChanged, event.ClassState,
			map[string]any{"device_id": "mk4-1", "driver": "xbuddy", "connected": false})},
	})

	eventually(t, "connected 0", func() bool {
		dp, ok := gaugePoint(h.collector.lastMetric("prusa.connect.device.connected"), bench)

		return ok && dp.GetAsInt() == 0
	})

	// The export that reported connected 0 is the newest; the gauges in it
	// carry no point for the printer.
	h.collector.mu.Lock()
	newest := h.collector.metrics[len(h.collector.metrics)-1]
	h.collector.mu.Unlock()

	for _, rm := range newest.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() != "prusa.connect.device.connected" && len(m.GetGauge().GetDataPoints()) > 0 {
					t.Errorf("%s still reports %v after the printer left", m.GetName(), m.GetGauge().GetDataPoints())
				}
			}
		}
	}
	// Back again: a value that did not change while it was away is read off
	// the fleet, because the stream will not report it.
	h.send(busapi.Window{Events: []json.RawMessage{envelope(t, event.TypeDevicePresenceChanged,
		event.ClassState, map[string]any{"device_id": "mk4-1", "driver": "xbuddy", "connected": true})}})

	eventually(t, "the nozzle temperature after the printer came back", func() bool {
		dp, ok := gaugePoint(h.collector.lastMetric("prusa.connect.tools.nozzle_temp"), bench)

		return ok && dp.GetAsDouble() == 214.8
	})
}

// A finished print becomes one span with the start and end the host recorded,
// and is counted by outcome. plugin.shutdown flushes it before answering.
func TestAFinishedPrintBecomesASpan(t *testing.T) {
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

	eventually(t, "the settled counter", func() bool {
		m := h.collector.lastMetric("prusa.connect.job.settled")
		for _, dp := range m.GetSum().GetDataPoints() {
			if hasAttrs(
				dp.GetAttributes(),
				map[string]string{"prusa.job.outcome": "failed", "prusa.device.name": "Bench MK4"},
			) {
				return dp.GetAsInt() == 1
			}
		}

		return false
	})

	h.shutdown()

	spans := h.collector.spans()
	if len(spans) != 1 {
		t.Fatalf("the collector received %d spans, want 1", len(spans))
	}

	span := spans[0]
	if span.GetName() != "print_job" {
		t.Errorf("the span is %q, want print_job", span.GetName())
	}

	start := time.Unix(0, int64(span.GetStartTimeUnixNano())).UTC()
	if !start.Equal(started) {
		t.Errorf("the span starts %s, want %s", start, started)
	}

	end := time.Unix(0, int64(span.GetEndTimeUnixNano())).UTC()
	if !end.Equal(ended) {
		t.Errorf("the span ends %s, want %s", end, ended)
	}

	if span.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		t.Errorf("a failed print has status %v, want error", span.GetStatus())
	}

	want := map[string]string{
		"prusa.job.display_name": "benchy.gcode", "prusa.job.outcome": "failed", "prusa.job.device_job_id": "412",
		"prusa.job.time_printing_s": "5400", "prusa.job.filament_used_mm": "1234.5", "prusa.device.model": "MK4S",
	}
	if got := attrMap(span.GetAttributes()); !hasAttrs(span.GetAttributes(), want) {
		t.Errorf("the span carries %v, want at least %v", got, want)
	}
}

// When the host ends the subscription, the module opens it again from the
// cursor of the last window it read, so no settled print is missed or counted
// twice.
func TestAnEndedSubscriptionResumesFromTheLastCursor(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	h.send(busapi.Window{Cursor: "cursor-7"})
	h.feed.endSubscription()

	if again := h.subscribed(); again.Resume != "cursor-7" {
		t.Errorf("the module resubscribed from %q, want cursor-7", again.Resume)
	}
}

// A cursor the host refuses is dropped and the stream is followed again from
// the head. A fresh subscription carries no resync, so nothing would say what
// changed in between: the fleet is read again instead, here a printer that
// went away while the module was not following.
func TestARefusedCursorReadsTheFleetAgain(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	h.send(busapi.Window{Cursor: "cursor-7"})

	gone := benchPrinter()
	gone.Connected = false
	h.fleet.set(gone)

	h.feed.refuseNext(busapi.ErrInvalidSubscription)
	h.feed.endSubscription()

	if refused := h.subscribed(); refused.Resume != "cursor-7" {
		t.Fatalf("the module resubscribed from %q, want cursor-7", refused.Resume)
	}

	if fresh := h.subscribed(); fresh.Resume != "" {
		t.Fatalf("after the refusal the module resubscribed from %q, want the head", fresh.Resume)
	}

	eventually(t, "the departed printer read off the fleet", func() bool {
		dp, ok := gaugePoint(h.collector.lastMetric("prusa.connect.device.connected"), bench)

		return ok && dp.GetAsInt() == 0
	})
}

// A print whose start the host did not record is still a span and counted,
// but it does not tell the duration histogram it took no time at all.
func TestAPrintWithNoKnownStartIsNotTimed(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(nil)
	h.subscribed()

	h.send(busapi.Window{Events: []json.RawMessage{envelope(t, event.TypeDeviceJobSettled, event.ClassState,
		event.JobSettled{JobID: "j-1", Ended: time.Now(), Outcome: event.JobFinished})}})

	eventually(t, "the settled counter", func() bool {
		for _, dp := range h.collector.lastMetric("prusa.connect.job.settled").GetSum().GetDataPoints() {
			if dp.GetAsInt() == 1 {
				return true
			}
		}

		return false
	})

	// The counter and the histogram are recorded together, so an export that
	// carries the one carries the other.
	for _, dp := range h.collector.lastMetric("prusa.connect.job.duration").GetHistogram().GetDataPoints() {
		if dp.GetCount() != 0 {
			t.Errorf("a print with no known start was timed: %v", dp)
		}
	}
}

// A printer first seen on the stream is named from device.get, and one the
// fleet no longer lists after a resync is forgotten.
func TestPrintersAreNamedAndForgottenWithTheFleet(t *testing.T) {
	h := newHarness(t)
	h.mustConfigure(nil)
	h.subscribed()

	h.fleet.set(benchPrinter())
	h.send(busapi.Window{Telemetry: []json.RawMessage{sample(t, "/temperature/bed", 61)}})

	eventually(t, "the late printer's name", func() bool {
		_, ok := gaugePoint(h.collector.lastMetric("prusa.connect.temperature.bed"), bench)

		return ok
	})

	h.fleet.set()
	h.send(busapi.Window{Resync: true})

	eventually(t, "the removed printer to be forgotten", func() bool {
		devices, values, _ := h.module.pipeline.gauges.counts()

		return devices == 0 && values == 0
	})
}

// The settings are checked when the host sends them, and a refusal reaches the
// host as the answer to plugin.configure without echoing a header value.
func TestConfigureRefusesSettingsTheExporterCannotUse(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		mentions string
	}{
		"no endpoint":      {map[string]any{"otlp_endpoint": ""}, "otlp_endpoint"},
		"bad protocol":     {map[string]any{"otlp_protocol": "udp"}, "protocol"},
		"bad interval":     {map[string]any{"export_interval": "often"}, "export_interval"},
		"bad prefix":       {map[string]any{"metric_prefix": "9lives"}, "metric_prefix"},
		"malformed header": {map[string]any{"otlp_headers": "Api-Token dt0c01.SECRET"}, "otlp_headers"},
		"unknown key":      {map[string]any{"otlp_timeout": "5s"}, "otlp_timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)

			err := h.configure(tc.settings)
			if err == nil {
				t.Fatal("plugin.configure accepted the settings")
			}

			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("the refusal %q does not name %s", err, tc.mentions)
			}

			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("the refusal %q echoes a header value", err)
			}
		})
	}
}

// The host sends plugin.configure once. A second one is refused rather than
// starting a second pipeline that would export every value twice.
func TestASecondConfigureIsRefused(t *testing.T) {
	h := newHarness(t)
	h.mustConfigure(nil)

	if err := h.configure(nil); err == nil || !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("a second plugin.configure answered %v, want a refusal", err)
	}
}

// GET /status reports a collector that refuses exports, here with a token it
// does not accept, and clears the failure
// once an export succeeds again.
func TestStatusReportsAFailingCollectorUntilItRecovers(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.collector.mu.Lock()
	h.collector.refuse = 1_000_000
	h.collector.mu.Unlock()

	h.mustConfigure(map[string]any{"otlp_headers": "Authorization=Api-Token dt0c01.SECRET"})
	h.subscribed()

	read := func() statusReport {
		rec := httptest.NewRecorder()
		h.module.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Fatalf("GET /status leaks a header value: %s", rec.Body)
		}

		var r statusReport
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("GET /status is not JSON: %v", err)
		}

		return r
	}

	eventually(t, "a failed metric export", func() bool {
		m := read().Exports[otlpexport.SignalMetrics]

		return m.Error != "" && !m.OK && m.FailingFor != nil
	})

	h.collector.mu.Lock()
	h.collector.refuse = 0
	h.collector.mu.Unlock()

	eventually(t, "the failure to clear", func() bool {
		m := read().Exports[otlpexport.SignalMetrics]

		return m.OK && m.Error == "" && m.FailingFor == nil
	})

	h.send(busapi.Window{Cursor: "c1"})

	eventually(t, "the stream to report a window", func() bool {
		r := read()

		return r.Stream.OK && r.LastWindow != nil
	})

	if r := read(); !r.Configured || r.Devices != 1 || r.Values == 0 {
		t.Errorf("GET /status reports %+v", r)
	}
}

// New metric names are created while the reader collects. The instrument lock
// and the value lock are taken in one order only, so this finishes rather than
// deadlocking; -race checks the rest.
func TestNewPathsWhileCollectingDoNotDeadlock(t *testing.T) {
	h := newHarness(t, benchPrinter())
	h.mustConfigure(map[string]any{"export_interval": "1ms"})
	h.subscribed()

	telemetry := make([]json.RawMessage, 0, 300)
	for i := range 300 {
		telemetry = append(telemetry, sample(t, fmt.Sprintf("/sensor_%d/reading", i), i))
	}

	h.send(busapi.Window{Telemetry: telemetry})

	// A deadlock leaves the count short of the bound and times this out.
	eventually(t, "every metric name up to the bound", func() bool {
		_, _, metrics := h.module.pipeline.gauges.counts()

		return metrics == maxInstruments
	})

	if dropped := h.module.pipeline.gauges.dropped.Load(); dropped == 0 {
		t.Error("no value was counted as dropped past the bound")
	}
}

// module.yaml publishes the defaults the code applies, so an operator reading
// the manifest learns what an unset key does.
func TestTheManifestPublishesTheCodesDefaults(t *testing.T) {
	manifest, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatalf("read module.yaml: %v", err)
	}

	for key, value := range map[string]string{
		"otlp_protocol": defaultProtocol, "service_name": defaultServiceName,
		"export_interval": defaultExportInterval, "metric_prefix": defaultMetricPrefix,
	} {
		entry := "- key: " + key + "\n    type: "
		i := strings.Index(string(manifest), entry)
		if i < 0 {
			t.Errorf("module.yaml declares no option %s", key)

			continue
		}

		rest := string(manifest)[i:]
		if next := strings.Index(rest[1:], "- key:"); next > 0 {
			rest = rest[:next+1]
		}

		if !strings.Contains(rest, `default: "`+value+`"`) {
			t.Errorf("module.yaml does not publish %q as the default of %s", value, key)
		}
	}
}

// The host refuses to start a module whose plugin.info names another version
// than its manifest, and the marketplace publishes the version module.yaml
// names. release-please bumps all three (release-please-config.json); this
// holds them together if one is missed.
func TestTheVersionAgreesEverywhere(t *testing.T) {
	manifest, err := os.ReadFile("module.yaml")
	if err != nil {
		t.Fatalf("read module.yaml: %v", err)
	}

	var declared string

	for line := range strings.Lines(string(manifest)) {
		if v, ok := strings.CutPrefix(line, "version:"); ok {
			declared = strings.TrimSpace(v)

			break
		}
	}

	if declared == "" {
		t.Fatal("module.yaml names no version")
	}

	if release.String() != declared {
		t.Errorf("plugin.info reports %s and module.yaml declares %s; the host would refuse to start it",
			release, declared)
	}

	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}

	if err = json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}

	if doc.Info.Version != declared {
		t.Errorf("openapi.json is version %s and module.yaml %s", doc.Info.Version, declared)
	}
}

// Every setting is in the manifest. One read from the environment is one an
// operator cannot find, see on the settings pane, or set through the API.
func TestNoSettingIsReadFromTheEnvironment(t *testing.T) {
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
			t.Errorf("%s reads an environment variable. Declare the setting in module.yaml instead", e.Name())
		}
	}
}
