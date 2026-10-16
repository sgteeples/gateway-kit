package upstream

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

var discardLogger = slog.New(slog.DiscardHandler)

// result is one scripted upstream outcome: a status, or an error.
type result struct {
	status int
	err    error
}

// scripted is a fake upstream that returns results in order (repeating the
// last one) and records what it was sent.
type scripted struct {
	mu      sync.Mutex
	results []result
	calls   int
	bodies  []string
}

func script(results ...result) *scripted { return &scripted{results: results} }

func (s *scripted) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.bodies = append(s.bodies, string(b))
	}
	r := s.results[min(s.calls, len(s.results)-1)]
	s.calls++
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{StatusCode: r.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func (s *scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
