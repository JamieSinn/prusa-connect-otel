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

// durationBuckets are histogram bounds in seconds: 1, 5 and 15 minutes, then
// doubling from 30 minutes to 32 hours.
var durationBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800, 57600, 115200}

// jobs exports each finished print as a span plus counters. The job.settled
// event carries both start and end times, so no state is kept between events.
type jobs struct {
	tracer   trace.Tracer
	settled  metric.Int64Counter
	duration metric.Float64Histogram
	filament metric.Float64Counter
}

func newJobs(tracer trace.Tracer, meter metric.Meter, prefix string) (*jobs, error) {
	settled, err := meter.Int64Counter(prefix+".job.settled",
		metric.WithDescription("Finished prints, by outcome."),
		metric.WithUnit("{job}"))
	if err != nil {
		return nil, err
	}

	duration, err := meter.Float64Histogram(prefix+".job.duration",
		metric.WithDescription("Print duration. Prints with an unknown start time are not recorded."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	if err != nil {
		return nil, err
	}

	filament, err := meter.Float64Counter(prefix+".job.filament_used",
		metric.WithDescription("Filament used, as reported by the printer."),
		metric.WithUnit("mm"))
	if err != nil {
		return nil, err
	}

	return &jobs{tracer: tracer, settled: settled, duration: duration, filament: filament}, nil
}

func (j *jobs) record(ctx context.Context, deviceAttrs []attribute.KeyValue, s event.JobSettled) {
	outcome := s.Outcome
	if outcome == "" {
		outcome = event.JobUnknown
	}

	counted := slices.Concat(deviceAttrs, []attribute.KeyValue{attribute.String("prusa.job.outcome", outcome)})
	j.settled.Add(ctx, 1, metric.WithAttributes(counted...))

	ended := s.Ended
	if ended.IsZero() {
		ended = time.Now()
	}

	// Without a valid start the span has zero length and the duration is not
	// recorded.
	started := s.Started
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
	attrs = appendNonEmpty(attrs, "prusa.job.display_name", s.DisplayName)
	attrs = appendNonEmpty(attrs, "prusa.job.path", s.Path)
	attrs = appendNonEmpty(attrs, "prusa.job.machine_state", s.MachineState)

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
		span.SetStatus(codes.Error, "print failed")
	}

	span.End(trace.WithTimestamp(ended))
}
