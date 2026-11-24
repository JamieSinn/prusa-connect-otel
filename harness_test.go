package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/event"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"
)

// harness runs the module over a real wirerpc session against the SDK's
// host-side bus and device handlers, exporting to an in-process OTLP/HTTP
// collector.
type harness struct {
	t         *testing.T
	module    *module
	host      *wirerpc.Conn
	feed      *feed
	fleet     *fleet
	collector *collector
}

func newHarness(t *testing.T, devices ...deviceapi.Device) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	hostEnd, childEnd := stdioPair(t)

	h := &harness{
		t:         t,
		feed:      &feed{windows: make(chan busapi.Window), opened: make(chan busapi.SubscribeParams, 8)},
		fleet:     &fleet{devices: devices},
		collector: newCollector(t),
	}

	h.module = newModule(ctx, slog.New(slog.NewTextHandler(t.Output(), nil)))

	child, err := wirerpc.NewConn(childEnd, wirerpc.Options{Role: wirerpc.RoleChild, Methods: h.module.methods()})
	if err != nil {
		t.Fatalf("child session: %v", err)
	}

	h.module.attach(child)

	hostMethods := wirerpc.Methods{}
	maps.Copy(hostMethods, busapi.SubscribeHandler(h.feed))
	maps.Copy(hostMethods, deviceapi.Handlers(h.fleet))

	h.host, err = wirerpc.NewConn(hostEnd, wirerpc.Options{Role: wirerpc.RoleHost, Methods: hostMethods})
	if err != nil {
		t.Fatalf("host session: %v", err)
	}

	var serving sync.WaitGroup

	serving.Go(func() { _ = child.Serve(ctx) })
	serving.Go(func() { _ = h.host.Serve(ctx) })

	t.Cleanup(func() {
		_ = h.module.shutdown()
		cancel()
		_ = hostEnd.Close()
		_ = childEnd.Close()
		serving.Wait()
	})

	return h
}

// pipeEnd is one side of a session over OS pipes, like the host's connection
// to a child's stdin/stdout. net.Pipe is unbuffered and would deadlock.
type pipeEnd struct {
	r, w *os.File
}

func (p pipeEnd) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeEnd) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeEnd) Close() error                { return errors.Join(p.r.Close(), p.w.Close()) }

func stdioPair(t *testing.T) (host, child pipeEnd) {
	t.Helper()

	toChild, fromHost, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	toHost, fromChild, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	return pipeEnd{r: toHost, w: fromHost}, pipeEnd{r: toChild, w: fromChild}
}

// configure sends plugin.configure with the test collector as the endpoint
// and a short export interval, plus any overrides.
func (h *harness) configure(overrides map[string]any) error {
	config := map[string]any{
		"otlp_endpoint":   h.collector.server.URL,
		"export_interval": "50ms",
	}
	maps.Copy(config, overrides)

	raw, err := json.Marshal(config)
	if err != nil {
		h.t.Fatalf("encode settings: %v", err)
	}

	_, err = h.host.Call(context.Background(), wirerpc.MethodConfigure, wirerpc.Settings{Config: raw})

	return err
}

func (h *harness) mustConfigure(overrides map[string]any) {
	h.t.Helper()

	if err := h.configure(overrides); err != nil {
		h.t.Fatalf("plugin.configure: %v", err)
	}
}

func (h *harness) shutdown() {
	h.t.Helper()

	if _, err := h.host.Call(context.Background(), wirerpc.MethodShutdown, nil); err != nil {
		h.t.Fatalf("plugin.shutdown: %v", err)
	}
}

// subscribed waits for the module to subscribe and returns the parameters.
func (h *harness) subscribed() busapi.SubscribeParams {
	h.t.Helper()

	select {
	case params := <-h.feed.opened:
		return params
	case <-time.After(5 * time.Second):
		h.t.Fatal("the module did not subscribe")
	}

	return busapi.SubscribeParams{}
}

func (h *harness) send(w busapi.Window) {
	h.t.Helper()

	select {
	case h.feed.windows <- w:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the module did not read the window")
	}
}

// feed is a fake event stream. Each subscription reports its parameters on
// opened, then delivers windows from the test until ended.
type feed struct {
	windows chan busapi.Window
	opened  chan busapi.SubscribeParams
	end     chan struct{} // closed to end the current subscription
	refusal error         // returned by the next subscription
	mu      sync.Mutex
}

func (f *feed) Subscribe(ctx context.Context, params busapi.SubscribeParams, write func(busapi.Window) error) error {
	f.mu.Lock()
	end := make(chan struct{})
	f.end = end
	refusal := f.refusal
	f.refusal = nil
	f.mu.Unlock()

	f.opened <- params

	if refusal != nil {
		return refusal
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-end:
			return nil
		case w := <-f.windows:
			if err := write(w); err != nil {
				return err
			}
		}
	}
}

func (f *feed) endSubscription() {
	f.mu.Lock()
	defer f.mu.Unlock()

	close(f.end)
}

func (f *feed) refuseNext(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.refusal = err
}

// fleet is a fake device API over a fixed list.
type fleet struct {
	mu      sync.Mutex
	devices []deviceapi.Device
}

var errNotServed = errors.New("not served by the test fleet")

func (f *fleet) List(_ context.Context, params deviceapi.ListParams) (deviceapi.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	items := f.devices[min(params.Offset, len(f.devices)):]
	items = items[:min(params.Limit, len(items))]

	return deviceapi.Page{Items: items, Total: len(f.devices), Limit: params.Limit, Offset: params.Offset}, nil
}

func (f *fleet) Get(_ context.Context, params deviceapi.GetParams) (deviceapi.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, d := range f.devices {
		if d.ID == params.ID {
			return d, nil
		}
	}

	return deviceapi.Device{}, deviceapi.ErrNoDevice
}

func (f *fleet) set(devices ...deviceapi.Device) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.devices = devices
}

func (*fleet) Command(context.Context, deviceapi.CommandParams) (deviceapi.Result, error) {
	return deviceapi.Result{}, errNotServed
}

func (*fleet) Approve(context.Context, deviceapi.ApproveParams) (deviceapi.Registration, error) {
	return deviceapi.Registration{}, errNotServed
}

func (*fleet) Jobs(context.Context, deviceapi.JobsParams) (deviceapi.Jobs, error) {
	return deviceapi.Jobs{}, errNotServed
}

func (*fleet) Files(context.Context, deviceapi.FilesParams) (deviceapi.Listing, error) {
	return deviceapi.Listing{}, errNotServed
}

func (*fleet) Print(context.Context, deviceapi.PrintParams) (deviceapi.Started, error) {
	return deviceapi.Started{}, errNotServed
}

func (*fleet) JobHistory(context.Context, deviceapi.JobHistoryParams) (deviceapi.JobPage, error) {
	return deviceapi.JobPage{}, errNotServed
}

// collector is an OTLP/HTTP receiver that keeps every request.
type collector struct {
	server *httptest.Server

	mu      sync.Mutex
	metrics []*colmetricspb.ExportMetricsServiceRequest
	traces  []*coltracepb.ExportTraceServiceRequest
	refuse  int // reject this many requests with 401 (not retried by the exporter)
}

func newCollector(t *testing.T) *collector {
	t.Helper()

	c := &collector{}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)

	return c
}

func (c *collector) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.refuse > 0 {
		c.refuse--
		http.Error(w, "the token is not valid", http.StatusUnauthorized)

		return
	}

	switch r.URL.Path {
	case "/v1/metrics":
		req := &colmetricspb.ExportMetricsServiceRequest{}
		if err = proto.Unmarshal(body, req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		c.metrics = append(c.metrics, req)
	case "/v1/traces":
		req := &coltracepb.ExportTraceServiceRequest{}
		if err = proto.Unmarshal(body, req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		c.traces = append(c.traces, req)
	default:
		http.NotFound(w, r)

		return
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
}

// lastMetric returns the named metric from the newest export containing it.
func (c *collector) lastMetric(name string) *metricspb.Metric {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, req := range slices.Backward(c.metrics) {
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					if m.GetName() == name {
						return m
					}
				}
			}
		}
	}

	return nil
}

func (c *collector) spans() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()

	var spans []*tracepb.Span

	for _, req := range c.traces {
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				spans = append(spans, ss.GetSpans()...)
			}
		}
	}

	return spans
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// gaugePoint returns the first data point whose attributes include want.
func gaugePoint(m *metricspb.Metric, want map[string]string) (*metricspb.NumberDataPoint, bool) {
	for _, dp := range m.GetGauge().GetDataPoints() {
		if hasAttrs(dp.GetAttributes(), want) {
			return dp, true
		}
	}

	return nil, false
}

func hasAttrs(attrs []*commonpb.KeyValue, want map[string]string) bool {
	got := attrMap(attrs)
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}

	return true
}

func attrMap(attrs []*commonpb.KeyValue) map[string]string {
	got := map[string]string{}

	for _, kv := range attrs {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			got[kv.GetKey()] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			got[kv.GetKey()] = strconv.FormatInt(v.IntValue, 10)
		case *commonpb.AnyValue_DoubleValue:
			raw, _ := json.Marshal(v.DoubleValue)
			got[kv.GetKey()] = string(raw)
		}
	}

	return got
}

// envelope builds a stream event for the bench printer.
func envelope(t *testing.T, eventType string, class event.Class, data any) json.RawMessage {
	t.Helper()

	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode data: %v", err)
	}

	const deviceID = "mk4-1"

	raw, err := json.Marshal(event.Envelope{
		SpecVersion: event.SpecVersion,
		ID:          "test-" + eventType,
		Source:      "urn:prusa:site:test/device/" + deviceID,
		Type:        eventType,
		Time:        time.Now(),
		Subject:     "device:" + deviceID,
		Class:       class,
		Data:        payload,
	})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}

	return raw
}

func sample(t *testing.T, path string, value any) json.RawMessage {
	t.Helper()

	return envelope(t, event.TypeDeviceTelemetrySampled, event.ClassTelemetry,
		map[string]any{"path": path, "value": value})
}
