package main

import (
	"context"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/prusa3d/prusa-connect/sdk/event"
)

// durationBuckets are the print duration histogram's bounds in seconds: one
// minute, five and fifteen minutes, then from half an hour doubling to 32 hours.
//
//nolint:gochecknoglobals // a fixed table of bounds, read-only after start
var durationBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800, 57600, 115200}

// jobs turns each finished print into one span and three measurements.
//
// The settled frame carries when the print started and when it ended, so the
// span is built from that one frame with both timestamps. Nothing is held
// between the started and the settled frame, and a module that starts while a
// print runs still reports it when it ends.
type jobs struct {
	tracer   trace.Tracer
	settled  metric.Int64Counter
	duration metric.Float64Histogram
	filament metric.Float64Counter
}

func newJobs(tracer trace.Tracer, meter metric.Meter, prefix string) (*jobs, error) {
	settled, err := meter.Int64Counter(prefix+".job.settled",
		metric.WithDescription("Prints that ended, by outcome."),
		metric.WithUnit("{job}"))
	if err != nil {
		return nil, err //nolint:wrapcheck // the SDK names the instrument
	}

	duration, err := meter.Float64Histogram(prefix+".job.duration",
		metric.WithDescription("How long each print took, from when Prusa Connect saw it start to when it saw it end. "+
			"A print whose start it did not see is not timed."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		return nil, err //nolint:wrapcheck // the SDK names the instrument
	}

	filament, err := meter.Float64Counter(prefix+".job.filament_used",
		metric.WithDescription("Filament the printer says its prints used. Prints that reported none are not counted."),
		metric.WithUnit("mm"))
	if err != nil {
		return nil, err //nolint:wrapcheck // the SDK names the instrument
	}

	return &jobs{tracer: tracer, settled: settled, duration: duration, filament: filament}, nil
}

// record exports one finished print.
func (j *jobs) record(ctx context.Context, deviceAttrs []attribute.KeyValue, s event.JobSettled) {
	outcome := s.Outcome
	if outcome == "" {
		outcome = event.JobUnknown
	}

	counted := slices.Concat(deviceAttrs, []attribute.KeyValue{attribute.String("prusa.job.outcome", outcome)})
	j.settled.Add(ctx, 1, metric.WithAttributes(counted...))

	started, ended := s.Started, s.Ended
	if ended.IsZero() {
		ended = time.Now()
	}

	// A start the host did not record, or one after the end, gives no
	// duration: the span is drawn as an instant, and the histogram is not told
	// the print took no time.
	if started.IsZero() || started.After(ended) {
		started = ended
	} else {
		j.duration.Record(ctx, ended.Sub(started).Seconds(), metric.WithAttributes(counted...))
	}

	if s.FilamentUsedKnown {
		j.filament.Add(ctx, s.FilamentUsedMM, metric.WithAttributes(deviceAttrs...))
	}

	attrs := slices.Concat(counted, []attribute.KeyValue{
		attribute.String("prusa.job.id", s.JobID),
		attribute.String("prusa.job.device_job_id", s.DeviceJobID),
		attribute.Float64("prusa.job.progress", s.Progress),
	})

	for _, kv := range []struct{ key, value string }{
		{"prusa.job.display_name", s.DisplayName},
		{"prusa.job.path", s.Path},
		{"prusa.job.machine_state", s.MachineState},
	} {
		if kv.value != "" {
			attrs = append(attrs, attribute.String(kv.key, kv.value))
		}
	}

	if s.TimePrintingKnown {
		attrs = append(attrs, attribute.Int64("prusa.job.time_printing_s", s.TimePrintingS))
	}

	if s.FilamentUsedKnown {
		attrs = append(attrs, attribute.Float64("prusa.job.filament_used_mm", s.FilamentUsedMM))
	}

	_, span := j.tracer.Start(ctx, "print_job",
		trace.WithTimestamp(started),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)

	if outcome == event.JobFailed {
		span.SetStatus(codes.Error, "the printer stopped the print on a fault")
	}

	span.End(trace.WithTimestamp(ended))
}
