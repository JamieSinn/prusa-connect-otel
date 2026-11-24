package otlpexport

import (
	"strings"
	"testing"
	"time"
)

func TestSplitEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		signalPath string
		wantHost   string
		wantPath   string
	}{
		{
			name:       "dynatrace activegate root path",
			raw:        "http://activegate.sinn.dev:9999/e/qtr1392d/api/v2/otlp",
			signalPath: "/v1/metrics",
			wantHost:   "activegate.sinn.dev:9999",
			wantPath:   "/e/qtr1392d/api/v2/otlp/v1/metrics",
		},
		{
			name:       "trailing slash on root path",
			raw:        "https://activegate.sinn.dev:9999/e/qtr1392d/api/v2/otlp/",
			signalPath: "/v1/traces",
			wantHost:   "activegate.sinn.dev:9999",
			wantPath:   "/e/qtr1392d/api/v2/otlp/v1/traces",
		},
		{
			name:       "bare host:port without scheme",
			raw:        "collector:4318",
			signalPath: "/v1/metrics",
			wantHost:   "collector:4318",
			wantPath:   "/v1/metrics",
		},
		{
			name:       "host with root path but no scheme",
			raw:        "activegate.sinn.dev:9999/e/qtr1392d/api/v2/otlp",
			signalPath: "/v1/traces",
			wantHost:   "activegate.sinn.dev:9999",
			wantPath:   "/e/qtr1392d/api/v2/otlp/v1/traces",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, urlPath, err := splitEndpoint(tt.raw, tt.signalPath)
			if err != nil {
				t.Fatalf("splitEndpoint(%q) error: %v", tt.raw, err)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if urlPath != tt.wantPath {
				t.Errorf("path = %q, want %q", urlPath, tt.wantPath)
			}
		})
	}
}

func TestParseHeaders(t *testing.T) {
	t.Run("empty yields nil", func(t *testing.T) {
		h, err := ParseHeaders("   ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h != nil {
			t.Errorf("got %v, want nil", h)
		}
	})

	t.Run("authorization value with spaces and equals", func(t *testing.T) {
		h, err := ParseHeaders("Authorization=Api-Token dt0c01.ABC=123, X-Env = prod ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, want := h["Authorization"], "Api-Token dt0c01.ABC=123"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if got, want := h["X-Env"], "prod"; got != want {
			t.Errorf("X-Env = %q, want %q", got, want)
		}
	})

	t.Run("malformed pair errors without echoing it", func(t *testing.T) {
		_, err := ParseHeaders("Api-Token dt0c01.SECRET")
		if err == nil {
			t.Fatal("expected error for pair without '='")
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("error %q echoes the header value", err)
		}
		if _, err = ParseHeaders("=value"); err == nil {
			t.Error("expected error for empty key")
		}
	})
}

func TestNormalized(t *testing.T) {
	tests := []struct {
		endpoint     string
		insecure     bool
		wantEndpoint string
		wantInsecure bool
	}{
		{"https://c:4318", true, "https://c:4318", false},
		{"http://c:4318", false, "http://c:4318", true},
		{"c:4318", true, "http://c:4318", true},
		{"c:4318", false, "https://c:4318", false},
	}
	for _, tt := range tests {
		o := Options{Endpoint: tt.endpoint, Insecure: tt.insecure}.Normalized()
		if o.Endpoint != tt.wantEndpoint || o.Insecure != tt.wantInsecure {
			t.Errorf("Normalize(%q, insecure=%v) = (%q, %v), want (%q, %v)",
				tt.endpoint, tt.insecure, o.Endpoint, o.Insecure, tt.wantEndpoint, tt.wantInsecure)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Options{Endpoint: "https://c:4318", Protocol: ProtocolHTTP, ExportInterval: time.Second}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid options refused: %v", err)
	}
	bad := map[string]Options{
		"no endpoint":   {Protocol: ProtocolHTTP, ExportInterval: time.Second},
		"bad protocol":  {Endpoint: "https://c:4318", Protocol: "udp", ExportInterval: time.Second},
		"zero interval": {Endpoint: "https://c:4318", Protocol: ProtocolHTTP},
		"no host":       {Endpoint: "https:///path", Protocol: ProtocolHTTP, ExportInterval: time.Second},
	}
	for name, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, o)
		}
	}
}
