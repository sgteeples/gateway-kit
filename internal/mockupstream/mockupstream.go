// Package mockupstream is a canned upstream server for tests and local demos.
//
// Every path echoes the request back as JSON, except:
//
//	GET /healthz         200, or 503 after SetHealthy(false)
//	?status=503          respond with that status
//	?delay=2s            wait before responding (honours client cancellation)
//	?fail_rate=0.5       fail with 503 at that probability (manual flaky demo)
//
// Tests can also call FailNext to make the next N requests fail.
package mockupstream

import (
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Server struct {
	Name string

	requests atomic.Int64
	healthy  atomic.Bool

	mu         sync.Mutex
	failNext   int
	failStatus int
}

func New(name string) *Server {
	s := &Server{Name: name}
	s.healthy.Store(true)
	return s
}

// Echo is the JSON body the mock returns for ordinary requests.
type Echo struct {
	Upstream string              `json:"upstream"`
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	Query    string              `json:"query"`
	Headers  map[string][]string `json:"headers"`
	Body     json.RawMessage     `json:"body,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)

	if r.URL.Path == "/healthz" {
		if s.healthy.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}

	// Headers a gateway might want to strip from responses.
	w.Header().Set("Server", "mockupstream/1.0")
	w.Header().Set("X-Powered-By", "mockupstream")

	q := r.URL.Query()
	if d, err := time.ParseDuration(q.Get("delay")); err == nil {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	if status, ok := s.takeFailure(); ok {
		writeJSON(w, status, map[string]string{"error": "injected failure", "upstream": s.Name})
		return
	}
	if rate, err := strconv.ParseFloat(q.Get("fail_rate"), 64); err == nil && rand.Float64() < rate {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "random failure", "upstream": s.Name})
		return
	}

	status := http.StatusOK
	if code, err := strconv.Atoi(q.Get("status")); err == nil {
		status = code
	}
	body, _ := io.ReadAll(r.Body)
	echo := Echo{
		Upstream: s.Name,
		Method:   r.Method,
		Path:     r.URL.Path,
		Query:    r.URL.RawQuery,
		Headers:  r.Header,
	}
	if len(body) > 0 {
		if json.Valid(body) {
			echo.Body = body
		} else {
			echo.Body, _ = json.Marshal(string(body))
		}
	}
	writeJSON(w, status, echo)
}

// FailNext makes the next n requests (other than /healthz) return status.
func (s *Server) FailNext(n, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext, s.failStatus = n, status
}

func (s *Server) takeFailure() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext <= 0 {
		return 0, false
	}
	s.failNext--
	return s.failStatus, true
}

func (s *Server) SetHealthy(ok bool) { s.healthy.Store(ok) }

// Requests is the total number of requests received, including /healthz.
func (s *Server) Requests() int64 { return s.requests.Load() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
