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

	"github.com/jamiesinn/prusa-connect-otel/internal/otlpexport"
)

// followed is every event type the module reads.
var followed = []string{
	event.TypeDeviceTelemetrySampled,
	event.TypeDevicePresenceChanged,
	event.TypeDeviceRecordChanged,
	event.TypeDeviceJobSettled,
}

// The wait between two attempts to follow the stream, doubling from the first
// to the last.
const (
	firstRetry  = time.Second
	lastRetry   = 30 * time.Second
	retryGrowth = 2
)

// host is the part of the session the pipeline calls: a stream for the
// subscription, and calls for the fleet. *wirerpc.Conn is one.
type host interface {
	busapi.Opener
	deviceapi.Caller
}

var _ host = (*wirerpc.Conn)(nil)

// pipeline follows the host's event stream and exports what it reads. One
// goroutine runs follow; the gauges and the status have their own locks for
// the readers on other goroutines.
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

// start runs follow until stop.
func (p *pipeline) start(ctx context.Context, conn host) {
	ctx, p.cancel = context.WithCancel(ctx)

	go func() {
		defer close(p.done)
		p.follow(ctx, conn)
	}()
}

// stop ends follow, then flushes what is buffered to the collector. ctx bounds
// the flush.
func (p *pipeline) stop(ctx context.Context) error {
	p.cancel()
	<-p.done

	return p.providers.Shutdown(ctx)
}

// errSubscriptionEnded is a subscription the host ended without an error.
var errSubscriptionEnded = errors.New("the host ended the subscription")

// follower is the state of one follow loop: where the stream is resumed from
// and how long to wait before the next attempt. Only follow's goroutine
// touches it.
type follower struct {
	conn   host
	fleet  *deviceapi.Client
	cursor string
	wait   time.Duration
	seeded bool
}

// follow reads the fleet once, then the stream, and reads the fleet again
// whenever the stream says continuity is gone. A refused or ended subscription
// is opened again after a wait that doubles up to lastRetry, from the cursor of
// the last window read, so no settled print is missed or counted twice while
// the process lives.
func (p *pipeline) follow(ctx context.Context, conn host) {
	f := &follower{conn: conn, fleet: deviceapi.NewClient(conn), wait: firstRetry}

	for ctx.Err() == nil {
		err := p.attempt(ctx, f)
		if ctx.Err() != nil {
			return
		}

		if errors.Is(err, busapi.ErrInvalidSubscription) {
			// The likeliest field to be out of bounds is the cursor, so the
			// next attempt starts at the head. A subscription from the head
			// carries no resync, so nothing would report what changed since
			// the last window: the fleet is read again first.
			f.cursor = ""
			f.seeded = false
		}

		p.status.streamFailed(err, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-time.After(f.wait):
		}

		f.wait = min(retryGrowth*f.wait, lastRetry)
	}
}

// attempt seeds the gauges if they are not yet, then reads the stream until it
// ends. It always returns why it ended.
func (p *pipeline) attempt(ctx context.Context, f *follower) error {
	if !f.seeded {
		// The stream says when a value changes, so a value that settled before
		// the module started is read off the fleet.
		if err := p.seed(ctx, f.fleet); err != nil {
			return err
		}

		f.seeded = true
	}

	params := busapi.SubscribeParams{Types: followed, Resume: f.cursor}

	err := busapi.Subscribe(ctx, f.conn, params, func(w busapi.Window) error {
		return p.window(ctx, f, w)
	})
	if err != nil {
		return fmt.Errorf("follow the event stream: %w", err)
	}

	return errSubscriptionEnded
}

// window applies one window and moves the cursor past it.
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

// seed reads every printer the module may see, with its state tree, and
// forgets any it held that the fleet no longer lists.
func (p *pipeline) seed(ctx context.Context, fleet *deviceapi.Client) error {
	listed := map[string]bool{}

	for offset := 0; ; {
		page, err := fleet.List(ctx, deviceapi.ListParams{Limit: deviceapi.MaxPageLimit, Offset: offset})
		if err != nil {
			return fmt.Errorf("list the fleet: %w", err)
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

// take records one printer as the fleet answered it.
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

// apply reads one window. Telemetry goes first and events after it, so a
// printer that went away in this window ends without values rather than with
// the ones it sent before it left.
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
			p.introduce(ctx, fleet, id)
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

// event applies one durable event.
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

		// The stream reports what changes after the printer is back, so a
		// value that is the same as before it left is read off the fleet.
		if change.Connected {
			p.introduce(ctx, fleet, id)
		}

	case event.TypeDeviceRecordChanged:
		// The frame's own fields are not in the SDK's contract, so the record
		// is read again through the one that is.
		p.introduce(ctx, fleet, id)

	case event.TypeDeviceJobSettled:
		var settled event.JobSettled
		if err := json.Unmarshal(e.Data, &settled); err != nil {
			p.log.WarnContext(ctx, "a settled print could not be read", "device", id, "error", err)

			return
		}

		if !p.gauges.known(id) {
			p.introduce(ctx, fleet, id)
		}

		p.jobs.record(ctx, p.gauges.attrsOf(id), settled)
	}
}

// introduce reads one printer's record. A printer the fleet will not answer for
// is still reported, by its id alone.
func (p *pipeline) introduce(ctx context.Context, fleet *deviceapi.Client, id string) {
	d, err := fleet.Get(ctx, deviceapi.GetParams{ID: id})
	if err != nil {
		p.log.WarnContext(ctx, "a printer's record could not be read", "device", id, "error", err)
		p.gauges.touch(id)

		return
	}

	p.take(d)
}

// deviceOf reads the printer an envelope is about from its subject.
func deviceOf(e event.Envelope) (string, bool) {
	origin, err := event.ParseOrigin(e.Subject)
	if err != nil || origin.Kind() != event.KindDevice {
		return "", false
	}

	return origin.ID(), true
}
