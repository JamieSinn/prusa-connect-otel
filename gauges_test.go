package main

import (
	"encoding/json"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestSeriesOf(t *testing.T) {
	for _, tc := range []struct {
		path, name, unit string
		attrs            []attribute.KeyValue
	}{
		{"/tools/0/nozzle_temp", "p.tools.nozzle_temp", "Cel", []attribute.KeyValue{attribute.String("prusa.tools.index", "0")}},
		{"/tools/0.1/feeder/slot_count", "p.tools.feeder.slot_count", "", []attribute.KeyValue{attribute.String("prusa.tools.index", "0.1")}},
		{"/temperature/bed", "p.temperature.bed", "Cel", nil},
		{"/tools/0/print_fan_rpm", "p.tools.print_fan_rpm", "{rpm}", []attribute.KeyValue{attribute.String("prusa.tools.index", "0")}},
		{"/job/progress", "p.job.progress", "%", nil},
		{"/job/time_remaining", "p.job.time_remaining", "s", nil},
		{"/job/filament_used_mm", "p.job.filament_used_mm", "mm", nil},
		{"/Odd Name!/x", "p.odd_name.x", "", nil},
		{"/3/x", "p.x", "", []attribute.KeyValue{attribute.String("prusa.index", "3")}},
		// Paths the demo's xBuddy and Klipper drivers report.
		{"/axis/z", "p.axis.z", "mm", nil},
		{"/build_volume/x", "p.build_volume.x", "mm", nil},
		{"/tools/0/features/nozzle_diameter", "p.tools.features.nozzle_diameter", "mm",
			[]attribute.KeyValue{attribute.String("prusa.tools.index", "0")}},
		{"/flow", "p.flow", "%", nil},
		{"/speed", "p.speed", "%", nil},
		{"/temperature/chamber_target", "p.temperature.chamber_target", "Cel", nil},
		{"/chamber/fan_1_rpm", "p.chamber.fan_1_rpm", "{rpm}", nil},
		{"/temp_bed", "p.temp_bed", "Cel", nil},
		{"/job/attempts", "p.job.attempts", "", nil},
		{"/mmu/retry_attempts", "p.mmu.retry_attempts", "", nil},
	} {
		s, ok := seriesOf("p", tc.path)
		if !ok || s.name != tc.name || s.unit != tc.unit || !slices.Equal(s.attrs, tc.attrs) {
			t.Errorf("seriesOf(%q) = %+v, %v; want %s %q %v", tc.path, s, ok, tc.name, tc.unit, tc.attrs)
		}
	}

	// No name, or an identifier field.
	for _, path := range []string{"", "/", "/0/1", "/!!!", "/job/id", "/dialog/id", "/tools/0/spool_id"} {
		if s, ok := seriesOf("p", path); ok {
			t.Errorf("seriesOf(%q) = %+v, want no metric", path, s)
		}
	}
}

func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		path, raw string
		value     float64
		keep      bool
		state     string
	}{
		{"/x", `21.5`, 21.5, true, ""},
		{"/x", `-3`, -3, true, ""},
		{"/x", `true`, 1, true, ""},
		{"/x", `false`, 0, true, ""},
		{"/x", `null`, 0, false, ""},
		{"/x", `"text"`, 0, false, ""},
		{"/x", `{"a":1}`, 0, false, ""},
		{"/x", `[1,2]`, 0, false, ""},
		{"/state", `"PRINTING"`, 1, true, "PRINTING"},
		{"/state", `""`, 0, false, ""},
		{"/job/state", `"PAUSED"`, 1, true, "PAUSED"},
	} {
		value, attrs, keep := decode(tc.path, json.RawMessage(tc.raw))
		if keep != tc.keep || value != tc.value {
			t.Errorf("decode(%s, %s) = %v, %v; want %v, %v", tc.path, tc.raw, value, keep, tc.value, tc.keep)
		}

		if tc.state != "" && (len(attrs) != 1 || attrs[0].Value.AsString() != tc.state) {
			t.Errorf("decode(%s, %s) carries %v, want prusa.state=%s", tc.path, tc.raw, attrs, tc.state)
		}
	}
}
