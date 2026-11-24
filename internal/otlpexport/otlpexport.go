// Package otlpexport builds the OTLP metric and trace providers.
//
// It handles endpoints with a root path (such as Dynatrace's
// /e/<env>/api/v2/otlp), picks TLS from the URL scheme, parses headers in the
// OTEL_EXPORTER_OTLP_HEADERS format, and can report the result of every export.
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

const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// Signal names passed to Options.OnExport.
const (
	SignalMetrics = "metrics"
	SignalTraces  = "traces"
)

type Options struct {
	// Endpoint is host:port or a URL. A path in the URL is used as a prefix
	// for /v1/metrics and /v1/traces. Call Normalized to reconcile the scheme
	// with Insecure.
	Endpoint      string
	Protocol      string // ProtocolGRPC or ProtocolHTTP
	Insecure      bool   // no TLS
	TLSSkipVerify bool   // accept any server certificate
	Headers       map[string]string
	ServiceName   string

	// ExportInterval is how often metrics are exported.
	ExportInterval time.Duration

	// Attributes are added to the resource alongside service.name.
	Attributes []attribute.KeyValue

	// OnExport, if set, is called after every export with the signal name and
	// the export error. It runs on the SDK's export goroutines and must not
	// block.
	OnExport func(signal string, err error)
}

// Normalized makes the endpoint scheme and Insecure agree. An http:// or
// https:// scheme wins; otherwise a scheme is added based on Insecure.
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

func (o Options) Validate() error {
	if o.Endpoint == "" {
		return errors.New("otlp endpoint is required")
	}

	if _, _, err := splitEndpoint(o.Endpoint, ""); err != nil {
		return err
	}

	if o.Protocol != ProtocolGRPC && o.Protocol != ProtocolHTTP {
		return fmt.Errorf("otlp protocol %q must be %s or %s", o.Protocol, ProtocolGRPC, ProtocolHTTP)
	}

	if o.ExportInterval <= 0 {
		return fmt.Errorf("export interval %s must be positive", o.ExportInterval)
	}

	return nil
}

// ParseHeaders parses comma-separated key=value pairs. Values may contain '='.
// Errors don't include the input, since it may hold credentials.
func ParseHeaders(raw string) (map[string]string, error) {
	headers := map[string]string{}

	for pair := range strings.SplitSeq(raw, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}

		key, value, ok := strings.Cut(pair, "=")

		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, errors.New("each header must be key=value")
		}

		headers[key] = strings.TrimSpace(value)
	}

	return headers, nil
}

type Providers struct {
	MeterProvider  *sdkmetric.MeterProvider
	TracerProvider *sdktrace.TracerProvider
}

// Setup builds the exporters and providers. It does not register them
// globally.
func Setup(ctx context.Context, o Options) (*Providers, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(append([]attribute.KeyValue{semconv.ServiceName(o.ServiceName)}, o.Attributes...)...),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
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
		metricExp = reportingMetricExporter{Exporter: metricExp, report: o.OnExport}
		traceExp = reportingSpanExporter{SpanExporter: traceExp, report: o.OnExport}
	}

	return &Providers{
		MeterProvider: sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(o.ExportInterval))),
			sdkmetric.WithResource(res),
		),
		TracerProvider: sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExp),
			sdktrace.WithResource(res),
		),
	}, nil
}

// Shutdown flushes and stops both providers. ctx must not be cancelled
// already, or nothing is flushed.
func (p *Providers) Shutdown(ctx context.Context) error {
	return errors.Join(
		p.TracerProvider.Shutdown(ctx),
		p.MeterProvider.Shutdown(ctx),
	)
}

// splitEndpoint returns the host:port and the request path for signalPath,
// joined onto any path in the endpoint.
func splitEndpoint(raw, signalPath string) (host, urlPath string, err error) {
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", "", fmt.Errorf("otlp endpoint %q is not a valid host:port or URL: %w", raw, err)
	}

	if u.Host == "" {
		return "", "", fmt.Errorf("otlp endpoint %q has no host", raw)
	}

	return u.Host, path.Join(strings.TrimSuffix(u.Path, "/"), signalPath), nil
}

// transport is the connection settings shared by both exporters.
type transport struct {
	host     string
	path     string // HTTP only
	headers  map[string]string
	insecure bool
	tls      *tls.Config // nil means the system default
}

func (o Options) transport(signalPath string) (transport, error) {
	host, urlPath, err := splitEndpoint(o.Endpoint, signalPath)
	if err != nil {
		return transport{}, err
	}

	t := transport{host: host, path: urlPath, headers: o.Headers, insecure: o.Insecure}
	if !o.Insecure && o.TLSSkipVerify {
		t.tls = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in setting for self-signed collectors
	}

	return t, nil
}

// The metric and trace exporters are built the same way, but each OTel
// exporter package has its own option types, so the code is repeated.

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
		return nil, fmt.Errorf("metric exporter: %w", err)
	}

	return exp, nil
}

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
		return nil, fmt.Errorf("trace exporter: %w", err)
	}

	return exp, nil
}

type reportingMetricExporter struct {
	sdkmetric.Exporter

	report func(signal string, err error)
}

func (e reportingMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	e.report(SignalMetrics, err)

	return err
}

type reportingSpanExporter struct {
	sdktrace.SpanExporter

	report func(signal string, err error)
}

func (e reportingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	e.report(SignalTraces, err)

	return err
}
