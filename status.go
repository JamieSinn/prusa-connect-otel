package main

import (
	"log/slog"
	"sync"
	"time"
)

// status is what GET /status reports: whether the stream is being read and
// whether exports reach the collector. A failure is reported while it is the
// latest outcome, and cleared by the next success, so the route never shows a
// warning that has stopped being true.
//
// It is written from the follow goroutine and the SDK's export goroutines, and
// read by the route; mu guards every field.
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

// outcome is the latest result of something that is retried.
type outcome struct {
	lastSuccess time.Time
	failingFor  time.Time // zero while the latest attempt succeeded
	err         string
}

func newStatus(log *slog.Logger) *status {
	return &status{log: log, exports: map[string]*outcome{}}
}

// configuredFor records the collector the pipeline exports to.
func (s *status) configuredFor(endpoint, protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.configured, s.endpoint, s.protocol = true, endpoint, protocol
}

// window records a window read off the stream.
func (s *status) window(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastWindow = at
	s.settle("the event stream", &s.stream, nil, at)
}

// streamFailed records the stream or the fleet refusing the module.
func (s *status) streamFailed(err error, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.settle("the event stream", &s.stream, err, at)
}

// exported records one export. It runs on the SDK's export goroutines.
func (s *status) exported(signal string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.exports[signal]
	if !ok {
		o = &outcome{}
		s.exports[signal] = o
	}

	s.settle("the "+signal+" export", o, err, time.Now())
}

// settle applies one result and logs only a change between failing and
// succeeding, so a collector that is down for an hour writes two lines and not
// one per export.
func (s *status) settle(what string, o *outcome, err error, at time.Time) {
	if err == nil {
		if !o.failingFor.IsZero() {
			s.log.Info(what+" recovered", "failed_since", o.failingFor)
		}

		o.lastSuccess, o.failingFor, o.err = at, time.Time{}, ""

		return
	}

	if o.failingFor.IsZero() {
		o.failingFor = at
		s.log.Warn(what+" is failing", "error", err)
	}

	o.err = err.Error()
}

// statusReport is the body of GET /status.
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
	OK          bool       `json:"ok"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	FailingFor  *time.Time `json:"failing_since,omitempty"`
	Error       string     `json:"error,omitempty"`
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
		OK:          !o.lastSuccess.IsZero() && o.failingFor.IsZero(),
		LastSuccess: timeOrNil(o.lastSuccess),
		FailingFor:  timeOrNil(o.failingFor),
		Error:       o.err,
	}
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}

	return &t
}
