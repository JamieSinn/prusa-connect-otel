package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSDKErrorsRateLimited(t *testing.T) {
	var out bytes.Buffer

	h := newSDKErrors(slog.New(slog.NewTextHandler(&out, nil)))
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	down := errors.New("failed to upload metrics: 401 Unauthorized")

	for range 100 {
		h.Handle(down)
	}

	h.Handle(errors.New("failed to upload traces: 401 Unauthorized"))
	h.Handle(nil)

	if got := strings.Count(out.String(), "\n"); got != 2 {
		t.Fatalf("logged %d lines, want 2:\n%s", got, out.String())
	}

	now = now.Add(repeatWindow)
	h.Handle(down)

	if got := strings.Count(out.String(), "\n"); got != 3 {
		t.Fatalf("error not logged again after the window:\n%s", out.String())
	}

	if len(h.logged) != 1 {
		t.Errorf("%d messages tracked, want 1", len(h.logged))
	}
}
