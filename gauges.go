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

// maxNameLen is the longest instrument name OpenTelemetry accepts.
const maxNameLen = 255

// maxInstruments bounds how many distinct metric names the module creates. A
// driver declares its own state paths, so the set is open; past the bound a new
// path is counted and dropped rather than growing memory without limit.
const maxInstruments = 256

// statePaths are the paths whose text value is a state machine word, such as
// IDLE or PRINTING. They become a gauge of 1 with the word as an attribute.
// Every other text value is dropped: a file name or a material as an attribute
// would mint a series per value.
var statePaths = map[string]bool{
	"/state":     true,
	"/job/state": true,
}

// indexSegment is a path segment that numbers a member rather than naming a
// field: "0" in /tools/0/nozzle_temp, or "0.1" for slot 1 of the feeder on
// tool 0.
var indexSegment = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// nameSegment is what a segment is reduced to in a metric name.
var nameUnsafe = regexp.MustCompile(`[^a-z0-9_]+`)

// series is where one state path lands: the metric name, its unit, and the
// attributes the path's index segments carry.
type series struct {
	name  string
	unit  string
	attrs []attribute.KeyValue
}

// seriesOf maps a state path onto a metric. Index segments leave the name and
// become attributes named after the segment before them, so every tool's
// nozzle shares one metric:
//
//	/tools/0/nozzle_temp   ->  <prefix>.tools.nozzle_temp {prusa.tools.index="0"}
//	/temperature/bed       ->  <prefix>.temperature.bed
//
// It reports false for a path with no name segment.
func seriesOf(prefix, path string) (series, bool) {
	var (
		names []string
		attrs []attribute.KeyValue
	)

	parent := ""

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

// isIdentifier reports a field that names something rather than measures it,
// such as /job/id or /dialog/id. A number there is a label, and as a gauge it
// would chart one meaningless line per print.
func isIdentifier(field string) bool {
	return field == "id" || strings.HasSuffix(field, "_id")
}

// unitOf guesses a UCUM unit from the name segments: a temperature anywhere in
// the path, otherwise the last segment's suffix. A wrong guess only mislabels a
// chart, and no guess is the empty unit.
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

// isTemperature reports a name segment with temp or temperature as one of its
// words, as in nozzle_temp or temp_bed, and not inside another word, as in
// retry_attempts.
func isTemperature(segment string) bool {
	for word := range strings.SplitSeq(segment, "_") {
		if word == "temp" || word == "temperature" {
			return true
		}
	}

	return false
}

// device is what the module knows about one printer.
type device struct {
	name, vendor, model string
	connected           bool
}

// pointKey names one value: a printer and the state path it reported.
type pointKey struct {
	device, path string
}

// point is the latest value of one state path on one printer.
type point struct {
	value float64
	attrs []attribute.KeyValue // the path's index attributes, and the state word
}

// gauges holds the latest value of every state path on every printer, and
// observes them as OTel gauges when the metric reader collects.
//
// # Locking
//
// Two locks, always taken in this order and never the other:
//
//   - instMu guards the instrument set. It is held while an instrument is
//     created, and the SDK takes its own pipeline lock inside that call.
//   - mu guards devices and points. An instrument's callback takes it, and the
//     SDK calls the callback while holding that same pipeline lock.
//
// So mu is never held while an instrument is created; otherwise a collection
// running at the same time would deadlock against it. Writes come from the one
// goroutine that follows the event stream. Reads come from the reader.
type gauges struct {
	meter  metric.Meter
	prefix string

	instMu      sync.Mutex
	instruments map[string]bool // metric name -> registered

	mu      sync.Mutex
	devices map[string]*device
	points  map[string]map[pointKey]point // metric name -> its series

	dropped atomic.Int64 // values dropped because the instrument bound was reached
}

func newGauges(meter metric.Meter, prefix string) (*gauges, error) {
	g := &gauges{
		meter:       meter,
		prefix:      prefix,
		instruments: map[string]bool{},
		devices:     map[string]*device{},
		points:      map[string]map[pointKey]point{},
	}

	if _, err := meter.Int64ObservableGauge(prefix+".device.connected",
		metric.WithDescription("1 while Prusa Connect holds a connection to the printer, else 0."),
		metric.WithInt64Callback(g.observeConnected),
	); err != nil {
		return nil, err //nolint:wrapcheck // the SDK names the instrument
	}

	return g, nil
}

// sample stores one reported value of a state path. A value that is not a
// number, a boolean, a state word or null is ignored; null forgets the value,
// because a path a driver no longer reports is unknown, not zero.
func (g *gauges) sample(deviceID, path string, raw json.RawMessage) {
	s, ok := seriesOf(g.prefix, path)
	if !ok {
		return
	}

	key := pointKey{device: deviceID, path: path}

	value, attrs, keep := decode(path, raw)
	if !keep {
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

// decode reads one value. It reports false for a value that has no gauge.
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
		var word string
		if !statePaths[path] || json.Unmarshal(raw, &word) != nil || word == "" {
			return 0, nil, false
		}

		return 1, []attribute.KeyValue{attribute.String("prusa.state", word)}, true
	}

	var number float64
	if json.Unmarshal(raw, &number) != nil {
		return 0, nil, false // an object or an array
	}

	return number, nil, true
}

// ensure creates the gauge for s once. It reports false when the bound on
// instruments is reached. It must not be called with mu held; see the type's
// doc.
func (g *gauges) ensure(s series) bool {
	g.instMu.Lock()
	defer g.instMu.Unlock()

	if registered, seen := g.instruments[s.name]; seen {
		return registered
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

	// A name the SDK refuses is recorded as refused, so it is not retried on
	// every sample.
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
		var up int64
		if d.connected {
			up = 1
		}

		o.Observe(up, metric.WithAttributes(d.attrs(id)...))
	}

	return nil
}

// forget drops one value.
func (g *gauges) forget(name string, key pointKey) {
	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.points[name], key)
}

// describe records what a printer is called and whether it is connected. A
// printer that is not connected has no current telemetry, so its values are
// dropped and only the connected gauge reports it.
func (g *gauges) describe(id string, d device) {
	g.mu.Lock()
	defer g.mu.Unlock()

	*g.deviceLocked(id) = d

	if !d.connected {
		g.dropValuesLocked(id)
	}
}

// presence records a printer connecting or going away.
func (g *gauges) presence(id string, connected bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.deviceLocked(id).connected = connected

	if !connected {
		g.dropValuesLocked(id)
	}
}

// remove forgets a printer entirely, as for one an operator removed.
func (g *gauges) remove(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.dropValuesLocked(id)
	delete(g.devices, id)
}

// retain forgets every printer not in keep, as after a fresh listing.
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

// touch makes a printer known by its id alone, leaving what is known of it as
// it is. A printer first seen on the stream is taken to be connected.
func (g *gauges) touch(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.deviceLocked(id)
}

// known reports whether the printer has been described or sampled.
func (g *gauges) known(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	_, ok := g.devices[id]

	return ok
}

// attrsOf is the printer's attributes, for a span or a counter.
func (g *gauges) attrsOf(id string) []attribute.KeyValue {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.devices[id].attrs(id)
}

// counts is what the status route reports.
func (g *gauges) counts() (devices, values, instruments int) {
	g.instMu.Lock()
	instruments = len(g.instruments)
	g.instMu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	for _, byKey := range g.points {
		values += len(byKey)
	}

	return len(g.devices), values, instruments
}

func (g *gauges) deviceLocked(id string) *device {
	d, ok := g.devices[id]
	if !ok {
		d = &device{connected: true}
		g.devices[id] = d
	}

	return d
}

func (g *gauges) dropValuesLocked(id string) {
	for _, byKey := range g.points {
		for key := range byKey {
			if key.device == id {
				delete(byKey, key)
			}
		}
	}
}

// attrs is the printer's identity as attributes. A nil device is one only its
// id is known for.
func (d *device) attrs(id string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String("prusa.device.id", id)}
	if d == nil {
		return attrs
	}

	for _, kv := range []struct{ key, value string }{
		{"prusa.device.name", d.name},
		{"prusa.device.vendor", d.vendor},
		{"prusa.device.model", d.model},
	} {
		if kv.value != "" {
			attrs = append(attrs, attribute.String(kv.key, kv.value))
		}
	}

	return attrs
}
