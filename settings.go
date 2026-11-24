package main

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jamiesinn/prusa-connect-otel/internal/otlpexport"
)

// The defaults module.yaml publishes for each setting. A setting the host sends
// empty takes the same value, so the manifest and the code cannot drift apart.
// TestTheManifestPublishesTheCodesDefaults holds the two together.
const (
	defaultProtocol       = otlpexport.ProtocolHTTP
	defaultServiceName    = "prusa-connect-otel"
	defaultExportInterval = "10s"
	defaultMetricPrefix   = "prusa.connect"
)

// Settings is what an operator writes under this instance's config block. The
// host merges it over the defaults module.yaml declares and sends the result on
// plugin.configure. A key this struct does not carry refuses the start.
type Settings struct {
	Endpoint       string `json:"otlp_endpoint"`
	Protocol       string `json:"otlp_protocol"`
	Headers        string `json:"otlp_headers"` // a secret option; never logged or echoed
	Insecure       bool   `json:"otlp_insecure"`
	TLSSkipVerify  bool   `json:"otlp_tls_skip_verify"`
	ServiceName    string `json:"service_name"`
	ExportInterval string `json:"export_interval"`
	MetricPrefix   string `json:"metric_prefix"`
}

// metricPrefixPattern is the part of an OTel instrument name a prefix may be.
var metricPrefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]{0,63}$`)

// resolved is the settings turned into what the pipeline runs on.
type resolved struct {
	otlp   otlpexport.Options
	prefix string
}

// resolve fills the defaults in, parses what the host sent as text, and refuses
// a setting the exporter could not use. The error names the key and never a
// header value, because those are credentials.
func (s Settings) resolve() (resolved, error) {
	s.Protocol = orDefault(s.Protocol, defaultProtocol)
	s.ServiceName = orDefault(s.ServiceName, defaultServiceName)
	s.ExportInterval = orDefault(s.ExportInterval, defaultExportInterval)
	s.MetricPrefix = orDefault(s.MetricPrefix, defaultMetricPrefix)

	if s.Endpoint == "" {
		return resolved{}, errors.New("otlp_endpoint is required")
	}

	interval, err := time.ParseDuration(s.ExportInterval)
	if err != nil {
		return resolved{}, fmt.Errorf("export_interval %q is not a duration: %w", s.ExportInterval, err)
	}

	if !metricPrefixPattern.MatchString(s.MetricPrefix) {
		return resolved{}, fmt.Errorf("metric_prefix %q is not the start of a metric name: "+
			"a letter, then letters, digits, '_', '.' or '-', at most 64 characters", s.MetricPrefix)
	}

	headers, err := otlpexport.ParseHeaders(s.Headers)
	if err != nil {
		return resolved{}, fmt.Errorf("otlp_headers: %w", err)
	}

	o := otlpexport.Options{
		Endpoint:       s.Endpoint,
		Protocol:       s.Protocol,
		Insecure:       s.Insecure,
		TLSSkipVerify:  s.TLSSkipVerify,
		Headers:        headers,
		ServiceName:    s.ServiceName,
		ExportInterval: interval,
	}.Normalized()

	if err = o.Validate(); err != nil {
		return resolved{}, err //nolint:wrapcheck // the message already names the setting
	}

	return resolved{otlp: o, prefix: s.MetricPrefix}, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
