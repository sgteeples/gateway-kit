package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(name)) })
}

func TestMatch(t *testing.T) {
	rt := New([]Route{
		{Path: "/api", Handler: named("api")},
		{Path: "/api/users", Handler: named("users")},
	})
	tests := []struct {
		path string
		want string // "" = no match
	}{
		{"/api/users", "/api/users"},
		{"/api/users/42", "/api/users"},
		{"/api/usersX", "/api"},
		{"/api", "/api"},
		{"/api/orders", "/api"},
		{"/apiX", ""},
		{"/", ""},
	}
	for _, tt := range tests {
		got := rt.Match(tt.path)
		switch {
		case got == nil && tt.want != "":
			t.Errorf("Match(%q) = nil, want %s", tt.path, tt.want)
		case got != nil && got.Path != tt.want:
			t.Errorf("Match(%q) = %s, want %q", tt.path, got.Path, tt.want)
		}
	}
}

func TestRootRouteMatchesEverything(t *testing.T) {
	rt := New([]Route{{Path: "/", Handler: named("root")}})
	if rt.Match("/anything/at/all") == nil {
		t.Error("route \"/\" should match every path")
	}
}

func TestServeHTTPStatuses(t *testing.T) {
	rt := New([]Route{{Path: "/get-only", Methods: []string{"GET", "HEAD"}, Handler: named("ok")}})

	tests := []struct {
		method, path string
		wantStatus   int
		wantAllow    string
	}{
		{"GET", "/get-only", 200, ""},
		{"POST", "/get-only", 405, "GET, HEAD"},
		{"GET", "/missing", 404, ""},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.wantStatus {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantStatus)
		}
		if got := rec.Header().Get("Allow"); got != tt.wantAllow {
			t.Errorf("%s %s Allow = %q, want %q", tt.method, tt.path, got, tt.wantAllow)
		}
	}
}
