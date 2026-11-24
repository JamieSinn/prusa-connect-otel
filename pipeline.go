package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/busapi"
	"github.com/prusa3d/prusa-connect/sdk/deviceapi"
	"github.com/prusa3d/prusa-connect/sdk/event"
	"github.com/prusa3d/prusa-connect/sdk/wirerpc"

	"github.com/jamiesinn/connect-otel/internal/otlpexport"
)

var followed = []string{
	event.TypeDeviceTelemetrySampled,
	event.TypeDevicePresenceChanged,
	event.TypeDeviceRecordChanged,
	event.TypeDeviceJobSettled,
}

// Backoff between subscription attempts.
const (
	firstRetry = time.Second
	maxRetry   = 30 * time.Second
)

var errSubscriptionEnded = errors.New("the host ended the subscription")

// host is the part of the host connection the pipeline uses.
type host interface {
	busapi.Opener
	deviceapi.Caller
}

var _ host = (*wirerpc.Conn)(nil)

// pipeline follows the host's event stream on one goroutine and feeds the
// gauges and jobs. gauges and status are also read from other goroutines and
// do their own locking.
type pipeline struct {
	log       *slog.Logger
	providers *otlpexport.Providers
	gauges    *gauges
	jobs      *jobs
	status    *status

	cancel context.CancelFunc
	done   chan struct{}
}

func newPipeline(log *slog.Logger, providers *otlpexport.Providers, prefix string, st *status) (*pipeline, error) {
	meter := providers.MeterProvider.Meter(name)

	g, err := newGauges(meter, prefix)
	if err != nil {
		return nil, err
	}

	j, err := newJobs(providers.TracerProvider.Tracer(name), meter, prefix)
	if err != nil {
		return nil, err
	}

	return &pipeline{log: log, providers: providers, gauges: g, jobs: j, status: st, done: make(chan struct{})}, nil
}

func (p *pipeline) start(ctx context.Context, conn host) {
	ctx, p.cancel = context.WithCancel(ctx)

	go func() {
		defer close(p.done)
		p.follow(ctx, conn)
	}()
}

// stop ends the follow loop and flushes the exporters within ctx.
func (p *pipeline) stop(ctx context.Context) error {
	p.cancel()
	<-p.done

	return p.providers.Shutdown(ctx)
}

// flush exports pending metrics and spans now.
func (p *pipeline) flush(ctx context.Context) error {
	return errors.Join(
		p.providers.MeterProvider.ForceFlush(ctx),
		p.providers.TracerProvider.ForceFlush(ctx),
	)
}

// follower is the follow loop's state. Only the follow goroutine uses it.
type follower struct {
	fleet  *deviceapi.Client
	cursor string
	wait   time.Duration
	seeded bool
}

// follow loads the fleet, then reads the event stream until ctx is done,
// resubscribing with backoff from the last cursor whenever the stream ends.
func (p *pipeline) follow(ctx context.Context, conn host) {
	f := &follower{fleet: deviceapi.NewClient(conn), wait: firstRetry}

	for ctx.Err() == nil {
		err := p.subscribe(ctx, conn, f)
		if ctx.Err() != nil {
			return
		}

		if errors.Is(err, busapi.ErrInvalidSubscription) {
			// Most likely the cursor is no longer valid. Start from the head,
			// and reload the fleet first: a fresh subscription has no resync
			// flag, so changes made in between would otherwise be missed.
			f.cursor = ""
			f.seeded = false
		}

		p.status.streamFailed(err, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-time.After(f.wait):
		}

		f.wait = min(2*f.wait, maxRetry)
	}
}

// subscribe loads the fleet if needed, then reads the stream until it ends.
// It always returns a non-nil error.
func (p *pipeline) subscribe(ctx context.Context, conn host, f *follower) error {
	if !f.seeded {
		// The stream only reports changes, so current values come from the
		// fleet.
		if err := p.seed(ctx, f.fleet); err != nil {
			return err
		}

		f.seeded = true
	}

	params := busapi.SubscribeParams{Types: followed, Resume: f.cursor}

	err := busapi.Subscribe(ctx, conn, params, func(w busapi.Window) error {
		return p.window(ctx, f, w)
	})
	if err != nil {
		return fmt.Errorf("event stream: %w", err)
	}

	return errSubscriptionEnded
}

func (p *pipeline) window(ctx context.Context, f *follower, w busapi.Window) error {
	f.wait = firstRetry
	p.status.window(time.Now())

	if w.Resync {
		if err := p.seed(ctx, f.fleet); err != nil {
			return err
		}
	}

	p.apply(ctx, f.fleet, w)

	if w.Cursor != "" {
		f.cursor = w.Cursor
	}

	return nil
}

// seed loads every device and its state, and drops devices that are no longer
// listed.
func (p *pipeline) seed(ctx context.Context, fleet *deviceapi.Client) error {
	listed := map[string]bool{}

	for offset := 0; ; {
		page, err := fleet.List(ctx, deviceapi.ListParams{Limit: deviceapi.MaxPageLimit, Offset: offset})
		if err != nil {
			return fmt.Errorf("list devices: %w", err)
		}

		for _, d := range page.Items {
			listed[d.ID] = true
			p.take(d)
		}

		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			break
		}
	}

	p.gauges.retain(listed)

	return nil
}

func (p *pipeline) take(d deviceapi.Device) {
	if d.Retired {
		p.gauges.remove(d.ID)

		return
	}

	p.gauges.describe(d.ID, device{name: d.Name, vendor: d.Vendor, model: d.Model, connected: d.Connected})

	if !d.Connected {
		return
	}

	for _, path := range slices.Sorted(maps.Keys(d.State)) {
		p.gauges.sample(d.ID, path, d.State[path])
	}
}

// apply handles one window. Telemetry is applied before events so that a
// disconnect in the same window clears the values that preceded it.
func (p *pipeline) apply(ctx context.Context, fleet *deviceapi.Client, w busapi.Window) {
	for _, raw := range w.Telemetry {
		var e event.Envelope
		if err := json.Unmarshal(raw, &e); err != nil || e.Type != event.TypeDeviceTelemetrySampled {
			continue
		}

		id, ok := deviceOf(e)
		if !ok {
			continue
		}

		var s struct {
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		}

		if json.Unmarshal(e.Data, &s) != nil || s.Path == "" {
			continue
		}

		if !p.gauges.known(id) {
			p.lookup(ctx, fleet, id)
		}

		p.gauges.sample(id, s.Path, s.Value)
	}

	for _, raw := range w.Events {
		var e event.Envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}

		p.event(ctx, fleet, e)
	}
}

func (p *pipeline) event(ctx context.Context, fleet *deviceapi.Client, e event.Envelope) {
	id, ok := deviceOf(e)
	if !ok {
		return
	}

	switch e.Type {
	case event.TypeDevicePresenceChanged:
		var change struct {
			Connected bool `json:"connected"`
		}

		if json.Unmarshal(e.Data, &change) != nil {
			return
		}

		p.gauges.presence(id, change.Connected)

		// Values that didn't change while the device was away won't appear on
		// the stream, so reload them.
		if change.Connected {
			p.lookup(ctx, fleet, id)
		}

	case event.TypeDeviceRecordChanged:
		// The event payload isn't part of the SDK contract, so reload the
		// device instead of reading it.
		p.lookup(ctx, fleet, id)

	case event.TypeDeviceJobSettled:
		var settled event.JobSettled
		if err := json.Unmarshal(e.Data, &settled); err != nil {
			p.log.WarnContext(ctx, "unreadable job.settled event", "device", id, "error", err)

			return
		}

		if !p.gauges.known(id) {
			p.lookup(ctx, fleet, id)
		}

		p.jobs.record(ctx, p.gauges.attrsOf(id), settled)
	}
}

// lookup loads one device. If that fails, the device is still tracked by id.
func (p *pipeline) lookup(ctx context.Context, fleet *deviceapi.Client, id string) {
	d, err := fleet.Get(ctx, deviceapi.GetParams{ID: id})
	if err != nil {
		p.log.WarnContext(ctx, "device lookup failed", "device", id, "error", err)
		p.gauges.touch(id)

		return
	}

	p.take(d)
}

func deviceOf(e event.Envelope) (string, bool) {
	origin, err := event.ParseOrigin(e.Subject)
	if err != nil || origin.Kind() != event.KindDevice {
		return "", false
	}

	return origin.ID(), true
}
