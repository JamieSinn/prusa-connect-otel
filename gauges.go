package main

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// maxNameLen is OpenTelemetry's limit on instrument names.
	maxNameLen = 255
	// maxInstruments caps the number of metric names. Drivers define their
	// own state paths, so without a cap memory could grow without bound.
	maxInstruments = 256
)

// statePaths hold a state name such as IDLE or PRINTING. They are exported
// as a gauge of 1 with the state as an attribute. Other string values are
// dropped, since each distinct value would create a new series.
var statePaths = map[string]bool{
	"/state":     true,
	"/job/state": true,
}

var (
	// indexSegment matches path segments that are indexes, such as "0" in
	// /tools/0/nozzle_temp or "0.1" for slot 1 of tool 0.
	indexSegment = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	nameUnsafe   = regexp.MustCompile(`[^a-z0-9_]+`)
)

type series struct {
	name  string
	unit  string
	attrs []attribute.KeyValue
}

// seriesOf maps a state path to a metric. Index segments become attributes,
// so all tools share one metric:
//
//	/tools/0/nozzle_temp  ->  <prefix>.tools.nozzle_temp {prusa.tools.index="0"}
//	/temperature/bed      ->  <prefix>.temperature.bed
//
// It returns false for paths that have no name or end in an id.
func seriesOf(prefix, path string) (series, bool) {
	var (
		names  []string
		attrs  []attribute.KeyValue
		parent string
	)

	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" {
			continue
		}

		if indexSegment.MatchString(segment) {
			key := "prusa.index"
			if parent != "" {
				key = "prusa." + parent + ".index"
			}

			attrs = append(attrs, attribute.String(key, segment))

			continue
		}

		clean := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(segment), "_"), "_")
		if clean == "" {
			continue
		}

		names = append(names, clean)
		parent = clean
	}

	if len(names) == 0 || isIdentifier(names[len(names)-1]) {
		return series{}, false
	}

	name := prefix + "." + strings.Join(names, ".")
	if len(name) > maxNameLen {
		return series{}, false
	}

	return series{name: name, unit: unitOf(names), attrs: attrs}, true
}

// isIdentifier reports fields like /job/id whose numeric value is a label,
// not a measurement.
func isIdentifier(field string) bool {
	return field == "id" || strings.HasSuffix(field, "_id")
}

// unitOf guesses a UCUM unit from the path. An unknown unit is left empty.
func unitOf(names []string) string {
	field := names[len(names)-1]

	switch {
	case slices.ContainsFunc(names, isTemperature):
		return "Cel"
	case strings.HasSuffix(field, "_rpm"):
		return "{rpm}"
	case strings.HasSuffix(field, "_mm"), strings.HasSuffix(field, "_diameter"),
		names[0] == "axis", names[0] == "build_volume":
		return "mm"
	case field == "progress", field == "flow", field == "speed", strings.HasSuffix(field, "_percent"):
		return "%"
	case strings.HasPrefix(field, "time_"), strings.HasSuffix(field, "_s"):
		return "s"
	}

	return ""
}

// isTemperature matches segments that contain the word temp or temperature,
// such as nozzle_temp, but not retry_attempts.
func isTemperature(segment string) bool {
	for word := range strings.SplitSeq(segment, "_") {
		if word == "temp" || word == "temperature" {
			return true
		}
	}

	return false
}

type device struct {
	name, vendor, model string
	connected           bool
}

type pointKey struct {
	device, path string
}

type point struct {
	value float64
	attrs []attribute.KeyValue
}

// gauges holds the latest value of every state path on every device and
// reports them through observable gauges.
//
// Lock order: instMu, then the SDK's internal lock, then mu. Instruments are
// created under instMu, and the SDK holds its lock while calling the gauge
// callbacks, which take mu. So mu must never be held while creating an
// instrument, or a concurrent collection deadlocks.
type gauges struct {
	meter  metric.Meter
	prefix string

	instMu      sync.Mutex
	instruments map[string]bool // metric name -> created successfully

	mu      sync.Mutex
	devices map[string]*device
	points  map[string]map[pointKey]point // metric name -> series

	dropped atomic.Int64 // values dropped because maxInstruments was reached
}

func newGauges(meter metric.Meter, prefix string) (*gauges, error) {
	g := &gauges{
		meter:       meter,
		prefix:      prefix,
		instruments: map[string]bool{},
		devices:     map[string]*device{},
		points:      map[string]map[pointKey]point{},
	}

	_, err := meter.Int64ObservableGauge(prefix+".device.connected",
		metric.WithDescription("1 while Prusa Connect is connected to the printer, otherwise 0."),
		metric.WithInt64Callback(g.observeConnected),
	)
	if err != nil {
		return nil, err
	}

	return g, nil
}

// sample stores a value for a state path. Values that aren't numbers,
// booleans or state names are ignored. null removes the value.
func (g *gauges) sample(deviceID, path string, raw json.RawMessage) {
	s, ok := seriesOf(g.prefix, path)
	if !ok {
		return
	}

	key := pointKey{device: deviceID, path: path}

	value, attrs, ok := decode(path, raw)
	if !ok {
		g.forget(s.name, key)

		return
	}

	if !g.ensure(s) {
		g.dropped.Add(1)

		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.deviceLocked(deviceID)

	if g.points[s.name] == nil {
		g.points[s.name] = map[pointKey]point{}
	}

	g.points[s.name][key] = point{value: value, attrs: append(s.attrs, attrs...)}
}

// decode parses a state value. It returns false if the value can't be a gauge.
func decode(path string, raw json.RawMessage) (float64, []attribute.KeyValue, bool) {
	raw = bytes.TrimSpace(raw)

	switch {
	case len(raw) == 0, bytes.Equal(raw, []byte("null")):
		return 0, nil, false
	case bytes.Equal(raw, []byte("true")):
		return 1, nil, true
	case bytes.Equal(raw, []byte("false")):
		return 0, nil, true
	case raw[0] == '"':
		var state string
		if !statePaths[path] || json.Unmarshal(raw, &state) != nil || state == "" {
			return 0, nil, false
		}

		return 1, []attribute.KeyValue{attribute.String("prusa.state", state)}, true
	}

	var number float64
	if json.Unmarshal(raw, &number) != nil {
		return 0, nil, false
	}

	return number, nil, true
}

// ensure creates the gauge for s if it doesn't exist yet. It returns false if
// the instrument cap is reached or the SDK rejected the name. Don't call it
// with mu held.
func (g *gauges) ensure(s series) bool {
	g.instMu.Lock()
	defer g.instMu.Unlock()

	if created, seen := g.instruments[s.name]; seen {
		return created
	}

	if len(g.instruments) >= maxInstruments {
		return false
	}

	name := s.name

	_, err := g.meter.Float64ObservableGauge(name,
		metric.WithUnit(s.unit),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			g.observe(name, o)

			return nil
		}),
	)

	// Remember rejected names too, so they aren't retried on every sample.
	g.instruments[name] = err == nil

	return err == nil
}

func (g *gauges) observe(name string, o metric.Float64Observer) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for key, p := range g.points[name] {
		attrs := append(g.devices[key.device].attrs(key.device), p.attrs...)
		o.Observe(p.value, metric.WithAttributes(attrs...))
	}
}

func (g *gauges) observeConnected(_ context.Context, o metric.Int64Observer) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for id, d := range g.devices {
		var connected int64
		if d.connected {
			connected = 1
		}

		o.Observe(connected, metric.WithAttributes(d.attrs(id)...))
	}

	return nil
}

func (g *gauges) forget(name string, key pointKey) {
	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.points[name], key)
}

// describe sets a device's details. A disconnected device has no current
// telemetry, so its values are dropped.
func (g *gauges) describe(id string, d device) {
	g.mu.Lock()
	defer g.mu.Unlock()

	*g.deviceLocked(id) = d

	if !d.connected {
		g.dropValuesLocked(id)
	}
}

func (g *gauges) presence(id string, connected bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.deviceLocked(id).connected = connected

	if !connected {
		g.dropValuesLocked(id)
	}
}

func (g *gauges) remove(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.dropValuesLocked(id)
	delete(g.devices, id)
}

// retain removes every device not in keep.
func (g *gauges) retain(keep map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for id := range g.devices {
		if !keep[id] {
			g.dropValuesLocked(id)
			delete(g.devices, id)
		}
	}
}

// touch adds a device by id if it isn't known yet.
func (g *gauges) touch(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.deviceLocked(id)
}

func (g *gauges) known(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	_, ok := g.devices[id]

	return ok
}

func (g *gauges) attrsOf(id string) []attribute.KeyValue {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.devices[id].attrs(id)
}

func (g *gauges) counts() (devices, values, instruments int) {
	g.instMu.Lock()
	instruments = len(g.instruments)
	g.instMu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	for _, series := range g.points {
		values += len(series)
	}

	return len(g.devices), values, instruments
}

// deviceLocked returns the device, adding it if needed. A device first seen
// on the stream is assumed to be connected.
func (g *gauges) deviceLocked(id string) *device {
	d, ok := g.devices[id]
	if !ok {
		d = &device{connected: true}
		g.devices[id] = d
	}

	return d
}

func (g *gauges) dropValuesLocked(id string) {
	for _, series := range g.points {
		for key := range series {
			if key.device == id {
				delete(series, key)
			}
		}
	}
}

// attrs returns the device's identifying attributes. d may be nil if only the
// id is known.
func (d *device) attrs(id string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String("prusa.device.id", id)}
	if d == nil {
		return attrs
	}

	attrs = appendNonEmpty(attrs, "prusa.device.name", d.name)
	attrs = appendNonEmpty(attrs, "prusa.device.vendor", d.vendor)
	attrs = appendNonEmpty(attrs, "prusa.device.model", d.model)

	return attrs
}

func appendNonEmpty(attrs []attribute.KeyValue, key, value string) []attribute.KeyValue {
	if value == "" {
		return attrs
	}

	return append(attrs, attribute.String(key, value))
}
