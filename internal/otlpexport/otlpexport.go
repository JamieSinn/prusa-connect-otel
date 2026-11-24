// Package otlpexport builds the module's OTLP metric and trace pipelines.
//
// It owns endpoint handling (root paths such as Dynatrace's
// "/e/<env>/api/v2/otlp", scheme-driven TLS), header parsing in the
// OTEL_EXPORTER_OTLP_HEADERS form, and an optional hook that observes the
// outcome of every export so a caller can report exporter health.
package otlpexport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc/credentials"
)

// The OTLP protocols this package can export over.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// The signal names passed to Options.OnExport.
const (
	SignalMetrics = "metrics"
	SignalTraces  = "traces"
)

// Options configures the exporters and providers.
type Options struct {
	// Endpoint is host:port or a URL, optionally with a root path onto which
	// /v1/metrics and /v1/traces are joined. An http:// or https:// scheme
	// decides TLS; a schemeless endpoint follows Insecure. Call Normalize to
	// reconcile the two before Setup.
	Endpoint      string
	Protocol      string            // ProtocolGRPC or ProtocolHTTP
	Insecure      bool              // plaintext; implied by an http:// endpoint
	TLSSkipVerify bool              // accept a self-signed collector certificate
	Headers       map[string]string // extra request headers, e.g. Authorization
	ServiceName   string

	// ExportInterval is the periodic metric reader's cadence.
	ExportInterval time.Duration

	// Attributes are added to the resource beside service.name.
	Attributes []attribute.KeyValue

	// OnExport, when set, is called after every metric and trace export with
	// the signal name and the export's error (nil on success). It is called
	// from the SDK's export goroutines and must not block.
	OnExport func(signal string, err error)
}

// Normalized returns o with the endpoint scheme and Insecure reconciled so the
// two can never disagree: an explicit http/https scheme is authoritative and
// sets Insecure, and a schemeless endpoint is given the scheme Insecure implies.
func (o Options) Normalized() Options {
	switch {
	case o.Endpoint == "":
	case strings.HasPrefix(o.Endpoint, "https://"):
		o.Insecure = false
	case strings.HasPrefix(o.Endpoint, "http://"):
		o.Insecure = true
	case o.Insecure:
		o.Endpoint = "http://" + o.Endpoint
	default:
		o.Endpoint = "https://" + o.Endpoint
	}

	return o
}

// Validate reports the first setting Setup could not use.
func (o Options) Validate() error {
	if o.Endpoint == "" {
		return errors.New("otlp endpoint is required")
	}

	if _, _, err := splitEndpoint(o.Endpoint, ""); err != nil {
		return err
	}

	switch o.Protocol {
	case ProtocolGRPC, ProtocolHTTP:
	default:
		return fmt.Errorf("otlp protocol %q must be %s or %s", o.Protocol, ProtocolGRPC, ProtocolHTTP)
	}

	if o.ExportInterval <= 0 {
		return fmt.Errorf("otlp export interval %s must be positive", o.ExportInterval)
	}

	return nil
}

// ParseHeaders parses comma-separated key=value pairs (the
// OTEL_EXPORTER_OTLP_HEADERS convention). Values may contain '=' (e.g. base64
// tokens); empty input yields a nil map.
func ParseHeaders(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil //nolint:nilnil // no headers is a valid, empty result
	}

	headers := make(map[string]string)

	for pair := range strings.SplitSeq(raw, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}

		key, val, ok := strings.Cut(pair, "=")

		key = strings.TrimSpace(key)
		if !ok || key == "" {
			// The pair is not echoed: it may be half of a credential.
			return nil, errors.New("otlp headers: a pair is not in key=value form")
		}

		headers[key] = strings.TrimSpace(val)
	}

	return headers, nil
}

// Providers holds the meter and tracer providers Setup built.
type Providers struct {
	MeterProvider  *sdkmetric.MeterProvider
	TracerProvider *sdktrace.TracerProvider
}

// Setup builds the OTLP exporters and the meter and tracer providers. It does
// not install them as the global providers; callers that want that call
// otel.SetMeterProvider / otel.SetTracerProvider themselves.
func Setup(ctx context.Context, o Options) (*Providers, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(append([]attribute.KeyValue{semconv.ServiceName(o.ServiceName)}, o.Attributes...)...),
	)
	if err != nil {
		return nil, fmt.Errorf("otlpexport: build resource: %w", err)
	}

	metricExp, err := newMetricExporter(ctx, o)
	if err != nil {
		return nil, err
	}

	traceExp, err := newTraceExporter(ctx, o)
	if err != nil {
		return nil, errors.Join(err, metricExp.Shutdown(ctx))
	}

	if o.OnExport != nil {
		metricExp = observedMetrics{Exporter: metricExp, report: o.OnExport}
		traceExp = observedSpans{SpanExporter: traceExp, report: o.OnExport}
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(o.ExportInterval))),
		sdkmetric.WithResource(res),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)

	return &Providers{MeterProvider: mp, TracerProvider: tp}, nil
}

// Shutdown flushes and stops both providers. Call it with a context that is
// not already cancelled, or nothing is flushed.
func (p *Providers) Shutdown(ctx context.Context) error {
	return errors.Join(
		p.TracerProvider.Shutdown(ctx),
		p.MeterProvider.Shutdown(ctx),
	)
}

// splitEndpoint splits the endpoint into host:port and the request path for the
// given signal. Any root path in the endpoint is treated as a prefix onto which
// signalPath is joined; a missing scheme is tolerated so bare "host:port"
// endpoints work.
func splitEndpoint(raw, signalPath string) (host, urlPath string, err error) {
	r := raw
	if !strings.Contains(r, "://") {
		r = "http://" + r
	}

	u, err := url.Parse(r)
	if err != nil {
		return "", "", fmt.Errorf("otlp endpoint %q is not a valid host:port or URL: %w", raw, err)
	}

	if u.Host == "" {
		return "", "", fmt.Errorf("otlp endpoint %q names no host", raw)
	}

	return u.Host, path.Join(strings.TrimSuffix(u.Path, "/"), signalPath), nil
}

// transport is how one signal reaches the collector, resolved once from the
// options so the two exporters below differ only in the SDK package they call.
type transport struct {
	host     string
	path     string // the HTTP request path; gRPC ignores it
	headers  map[string]string
	insecure bool
	tls      *tls.Config // nil for the system default
}

func (o Options) transport(signalPath string) (transport, error) {
	host, urlPath, err := splitEndpoint(o.Endpoint, signalPath)
	if err != nil {
		return transport{}, err
	}

	t := transport{host: host, path: urlPath, headers: o.Headers, insecure: o.Insecure}
	if !o.Insecure && o.TLSSkipVerify {
		t.tls = skipVerifyTLS()
	}

	return t, nil
}

// newMetricExporter and newTraceExporter read alike because the four OTel
// exporter packages each have their own option types, with no interface in
// common to build both from one function.
//
//nolint:dupl // parallel by the SDK's design; see above
func newMetricExporter(ctx context.Context, o Options) (sdkmetric.Exporter, error) {
	t, err := o.transport("/v1/metrics")
	if err != nil {
		return nil, err
	}

	var exp sdkmetric.Exporter

	if o.Protocol == ProtocolHTTP {
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(t.host),
			otlpmetrichttp.WithURLPath(t.path),
			otlpmetrichttp.WithHeaders(t.headers),
		}

		switch {
		case t.insecure:
			opts = append(opts, otlpmetrichttp.WithInsecure())
		case t.tls != nil:
			opts = append(opts, otlpmetrichttp.WithTLSClientConfig(t.tls))
		}

		exp, err = otlpmetrichttp.New(ctx, opts...)
	} else {
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(t.host),
			otlpmetricgrpc.WithHeaders(t.headers),
		}

		switch {
		case t.insecure:
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		case t.tls != nil:
			opts = append(opts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(t.tls)))
		}

		exp, err = otlpmetricgrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, fmt.Errorf("otlpexport: metric exporter: %w", err)
	}

	return exp, nil
}

//nolint:dupl // parallel to newMetricExporter by the SDK's design
func newTraceExporter(ctx context.Context, o Options) (sdktrace.SpanExporter, error) {
	t, err := o.transport("/v1/traces")
	if err != nil {
		return nil, err
	}

	var exp sdktrace.SpanExporter

	if o.Protocol == ProtocolHTTP {
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(t.host),
			otlptracehttp.WithURLPath(t.path),
			otlptracehttp.WithHeaders(t.headers),
		}

		switch {
		case t.insecure:
			opts = append(opts, otlptracehttp.WithInsecure())
		case t.tls != nil:
			opts = append(opts, otlptracehttp.WithTLSClientConfig(t.tls))
		}

		exp, err = otlptracehttp.New(ctx, opts...)
	} else {
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(t.host),
			otlptracegrpc.WithHeaders(t.headers),
		}

		switch {
		case t.insecure:
			opts = append(opts, otlptracegrpc.WithInsecure())
		case t.tls != nil:
			opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(t.tls)))
		}

		exp, err = otlptracegrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, fmt.Errorf("otlpexport: trace exporter: %w", err)
	}

	return exp, nil
}

func skipVerifyTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in by the operator for a self-signed collector
}

// observedMetrics reports the outcome of each metric export.
type observedMetrics struct {
	sdkmetric.Exporter

	report func(signal string, err error)
}

func (e observedMetrics) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	e.report(SignalMetrics, err)

	return err //nolint:wrapcheck // the SDK reads the exporter's own error
}

// observedSpans reports the outcome of each span export.
type observedSpans struct {
	sdktrace.SpanExporter

	report func(signal string, err error)
}

func (e observedSpans) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	e.report(SignalTraces, err)

	return err //nolint:wrapcheck // the SDK reads the exporter's own error
}
