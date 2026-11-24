package main

import (
	"log/slog"
	"sync"
	"time"
)

// repeatWindow is how long an SDK error is kept quiet after it was logged once.
const repeatWindow = 5 * time.Minute

// sdkErrors is the OpenTelemetry SDK's error handler. The SDK reports every
// failed export there, so a collector that is down for an hour would write one
// line per export into the host's log. It logs each distinct message once per
// repeatWindow. Whether exports currently fail is on GET /status, which clears
// when they recover.
type sdkErrors struct {
	log *slog.Logger
	now func() time.Time

	mu     sync.Mutex
	logged map[string]time.Time
}

func newSDKErrors(log *slog.Logger) *sdkErrors {
	return &sdkErrors{log: log, now: time.Now, logged: map[string]time.Time{}}
}

// Handle implements otel.ErrorHandler.
func (h *sdkErrors) Handle(err error) {
	if err == nil {
		return
	}

	msg := err.Error()
	now := h.now()

	h.mu.Lock()

	for seen, at := range h.logged {
		if now.Sub(at) >= repeatWindow {
			delete(h.logged, seen)
		}
	}

	_, quiet := h.logged[msg]
	if !quiet {
		h.logged[msg] = now
	}

	h.mu.Unlock()

	if !quiet {
		h.log.Warn("opentelemetry sdk", "error", msg, "repeats_hidden_for", repeatWindow)
	}
}
