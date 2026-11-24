package main

import (
	"log/slog"
	"sync"
	"time"
)

// repeatWindow is how long a repeated SDK error is suppressed.
const repeatWindow = 5 * time.Minute

// sdkErrors is the OpenTelemetry error handler. The SDK reports every failed
// export, so a down collector would log on every interval; this logs each
// distinct message at most once per repeatWindow. Current export health is on
// GET /status.
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

	for m, at := range h.logged {
		if now.Sub(at) >= repeatWindow {
			delete(h.logged, m)
		}
	}

	_, suppressed := h.logged[msg]
	if !suppressed {
		h.logged[msg] = now
	}

	h.mu.Unlock()

	if !suppressed {
		h.log.Warn("opentelemetry sdk", "error", msg)
	}
}
