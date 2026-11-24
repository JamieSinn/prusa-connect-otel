package main

import (
	"log/slog"
	"sync"
	"time"
)

// status tracks stream and export health for GET /status. A failure is
// reported until the next success clears it. It is written by the follow
// goroutine and the SDK's export goroutines; mu guards all fields.
type status struct {
	log *slog.Logger

	mu         sync.Mutex
	configured bool
	endpoint   string
	protocol   string
	stream     outcome
	lastWindow time.Time
	exports    map[string]*outcome
}

// outcome is the latest result of a retried operation.
type outcome struct {
	lastSuccess  time.Time
	failingSince time.Time // zero while healthy
	err          string
}

func newStatus(log *slog.Logger) *status {
	return &status{log: log, exports: map[string]*outcome{}}
}

func (s *status) configuredFor(endpoint, protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.configured, s.endpoint, s.protocol = true, endpoint, protocol
}

func (s *status) window(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastWindow = at
	s.settle("event stream", &s.stream, nil, at)
}

func (s *status) streamFailed(err error, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.settle("event stream", &s.stream, err, at)
}

// exported records the result of one export. It is called from the SDK's
// export goroutines.
func (s *status) exported(signal string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.exports[signal]
	if !ok {
		o = &outcome{}
		s.exports[signal] = o
	}

	s.settle(signal+" export", o, err, time.Now())
}

// settle records a result. It logs only on a change between healthy and
// failing, so a long outage produces two log lines rather than one per try.
func (s *status) settle(what string, o *outcome, err error, at time.Time) {
	if err == nil {
		if !o.failingSince.IsZero() {
			s.log.Info(what+" recovered", "failing_since", o.failingSince)
		}

		o.lastSuccess, o.failingSince, o.err = at, time.Time{}, ""

		return
	}

	if o.failingSince.IsZero() {
		o.failingSince = at
		s.log.Warn(what+" failing", "error", err)
	}

	o.err = err.Error()
}

type statusReport struct {
	Configured bool                     `json:"configured"`
	Endpoint   string                   `json:"endpoint,omitempty"`
	Protocol   string                   `json:"protocol,omitempty"`
	Stream     outcomeReport            `json:"stream"`
	LastWindow *time.Time               `json:"last_window,omitempty"`
	Exports    map[string]outcomeReport `json:"exports"`
	Devices    int                      `json:"devices"`
	Values     int                      `json:"values"`
	Metrics    int                      `json:"metrics"`
	Dropped    int64                    `json:"dropped_values"`
}

type outcomeReport struct {
	OK           bool       `json:"ok"`
	LastSuccess  *time.Time `json:"last_success,omitempty"`
	FailingSince *time.Time `json:"failing_since,omitempty"`
	Error        string     `json:"error,omitempty"`
}

func (s *status) report() statusReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := statusReport{
		Configured: s.configured,
		Endpoint:   s.endpoint,
		Protocol:   s.protocol,
		Stream:     s.stream.report(),
		LastWindow: timeOrNil(s.lastWindow),
		Exports:    map[string]outcomeReport{},
	}

	for signal, o := range s.exports {
		r.Exports[signal] = o.report()
	}

	return r
}

func (o outcome) report() outcomeReport {
	return outcomeReport{
		OK:           !o.lastSuccess.IsZero() && o.failingSince.IsZero(),
		LastSuccess:  timeOrNil(o.lastSuccess),
		FailingSince: timeOrNil(o.failingSince),
		Error:        o.err,
	}
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}
