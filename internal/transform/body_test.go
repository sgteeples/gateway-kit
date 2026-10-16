package transform

import (
	"encoding/json"
	"testing"
	"time"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var testVars = Vars{
	RequestTime:  time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	ResponseTime: time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC),
	RoutePath:    "/api/legacy",
}

func TestMappingFromSpec(t *testing.T) {
	b := body{mapping: map[string]string{
		"user.id":        "userId",
		"user.name":      "userName",
		"meta.source":    "$literal:gateway",
		"meta.timestamp": "$request_time",
	}}
	got := jsonString(t, b.apply(mustDecode(t, `{"userId": 42, "userName": "ada", "unmapped": true}`), testVars))
	want := `{"meta":{"source":"gateway","timestamp":"2026-03-04T05:06:07.000Z"},"user":{"id":42,"name":"ada"}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMappingEdgeCases(t *testing.T) {
	b := body{mapping: map[string]string{
		"email":   "profile.contact.email", // nested source
		"missing": "does.not.exist",        // left out, not null
		"big":     "id",                    // large integers keep full precision
		"tags":    "tags",                  // non-scalar values copied whole
	}}
	src := `{"profile": {"contact": {"email": "a@b.c"}}, "id": 9007199254740993, "tags": ["x", "y"]}`
	got := jsonString(t, b.apply(mustDecode(t, src), testVars))
	want := `{"big":9007199254740993,"email":"a@b.c","tags":["x","y"]}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestEnvelopeFromSpec(t *testing.T) {
	b := body{envelope: map[string]any{
		"data": "$body",
		"gateway_metadata": map[string]any{
			"served_at": "$response_time",
			"route":     "$route_path",
		},
	}}
	tests := []struct{ name, raw, want string }{
		{"json", `{"id": 1}`, `{"data":{"id":1},"gateway_metadata":{"route":"/api/legacy","served_at":"2026-03-04T05:06:08.000Z"}}`},
		{"not json", `plain text`, `{"data":"plain text","gateway_metadata":{"route":"/api/legacy","served_at":"2026-03-04T05:06:08.000Z"}}`},
		{"empty", ``, `{"data":null,"gateway_metadata":{"route":"/api/legacy","served_at":"2026-03-04T05:06:08.000Z"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonString(t, b.apply(parseResponse([]byte(tt.raw)), testVars)); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestIsJSON(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"application/vnd.api+json":        true,
		"text/plain":                      false,
		"":                                false,
	} {
		if got := isJSON(ct); got != want {
			t.Errorf("isJSON(%q) = %v", ct, got)
		}
	}
}
