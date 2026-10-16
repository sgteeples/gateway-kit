package transform

import (
	"testing"
	"time"
)

func TestVarsString(t *testing.T) {
	req := time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC)
	resp := req.Add(1500 * time.Millisecond)
	v := Vars{RequestTime: req, ResponseTime: resp, RoutePath: "/api/legacy"}

	tests := map[string]string{
		"$request_time":     "2026-03-04T05:06:07.890Z",
		"$response_time":    "2026-03-04T05:06:09.390Z",
		"$route_path":       "/api/legacy",
		"$literal:gateway":  "gateway",
		"$literal:$escaped": "$escaped",
		"plain value":       "plain value",
	}
	for in, want := range tests {
		if got := v.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
