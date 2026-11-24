package main

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"time"

	"prusa-connect-otel.jamiesinn.ca/internal/otlpexport"
)

// Defaults for settings the host sends empty. They must match the defaults in
// module.yaml; TestManifestDefaults checks that.
const (
	defaultProtocol       = otlpexport.ProtocolHTTP
	defaultServiceName    = "prusa-connect-otel"
	defaultExportInterval = "10s"
	defaultMetricPrefix   = "prusa.connect"
)

// Settings is the module config the host sends on plugin.configure. The host
// rejects keys that aren't declared here.
type Settings struct {
	Endpoint       string `json:"otlp_endpoint"`
	Protocol       string `json:"otlp_protocol"`
	Headers        string `json:"otlp_headers"` // secret: never log or echo it
	Insecure       bool   `json:"otlp_insecure"`
	TLSSkipVerify  bool   `json:"otlp_tls_skip_verify"`
	ServiceName    string `json:"service_name"`
	ExportInterval string `json:"export_interval"`
	MetricPrefix   string `json:"metric_prefix"`
}

var metricPrefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]{0,63}$`)

type config struct {
	otlp   otlpexport.Options
	prefix string
}

// resolve applies defaults and validates the settings. Errors name the
// setting but never include header values.
func (s Settings) resolve() (config, error) {
	if s.Endpoint == "" {
		return config{}, errors.New("otlp_endpoint is required")
	}

	interval, err := time.ParseDuration(cmp.Or(s.ExportInterval, defaultExportInterval))
	if err != nil {
		return config{}, fmt.Errorf("export_interval: %w", err)
	}

	prefix := cmp.Or(s.MetricPrefix, defaultMetricPrefix)
	if !metricPrefixPattern.MatchString(prefix) {
		return config{}, fmt.Errorf("metric_prefix %q must start with a letter and contain only "+
			"letters, digits, '_', '.' or '-' (max 64 characters)", prefix)
	}

	headers, err := otlpexport.ParseHeaders(s.Headers)
	if err != nil {
		return config{}, fmt.Errorf("otlp_headers: %w", err)
	}

	o := otlpexport.Options{
		Endpoint:       s.Endpoint,
		Protocol:       cmp.Or(s.Protocol, defaultProtocol),
		Insecure:       s.Insecure,
		TLSSkipVerify:  s.TLSSkipVerify,
		Headers:        headers,
		ServiceName:    cmp.Or(s.ServiceName, defaultServiceName),
		ExportInterval: interval,
	}.Normalized()

	if err = o.Validate(); err != nil {
		return config{}, err
	}

	return config{otlp: o, prefix: prefix}, nil
}
