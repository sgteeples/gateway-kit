package transform

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/httperr"
	"github.com/sgteeples/gateway-kit/internal/netx"
)

// body applies a BodyTransform to a decoded JSON value: first the mapping
// (if any), then the envelope (if any). The same logic serves both requests
// and responses.
type body struct {
	mapping  map[string]string
	envelope map[string]any
}

func newBody(t *config.BodyTransform) body {
	return body{mapping: t.Mapping, envelope: t.Envelope}
}

func (b body) apply(v any, vars Vars) any {
	if len(b.mapping) > 0 {
		v = applyMapping(v, b.mapping, vars)
	}
	if b.envelope != nil {
		v = fill(b.envelope, v, vars)
	}
	return v
}

// applyMapping builds a new object holding only the mapped fields. Sources are
// dot paths into src or $variables. A missing source field is left out rather
// than written as null.
func applyMapping(src any, mapping map[string]string, vars Vars) map[string]any {
	out := make(map[string]any, len(mapping))
	for dest, source := range mapping {
		if strings.HasPrefix(source, "$") {
			setPath(out, dest, vars.String(source))
		} else if v, ok := getPath(src, source); ok {
			setPath(out, dest, v)
		}
	}
	return out
}

func getPath(v any, path string) (any, bool) {
	for _, key := range strings.Split(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = obj[key]; !ok {
			return nil, false
		}
	}
	return v, true
}

func setPath(obj map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, key := range keys[:len(keys)-1] {
		child, ok := obj[key].(map[string]any)
		if !ok {
			child = make(map[string]any)
			obj[key] = child
		}
		obj = child
	}
	obj[keys[len(keys)-1]] = v
}

// fill walks an envelope template, replacing "$body" with the original body
// and resolving other variables.
func fill(tmpl, original any, vars Vars) any {
	switch t := tmpl.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = fill(child, original, vars)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = fill(child, original, vars)
		}
		return out
	case string:
		if t == config.VarBody {
			return original
		}
		return vars.String(t)
	default:
		return t
	}
}

// decodeJSON decodes with UseNumber so large integers survive the round trip.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, io.ErrUnexpectedEOF // trailing data after the JSON value
	}
	return v, nil
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
}

// RequestBody rewrites JSON request bodies before they are forwarded.
// Requests with no body, or with a non-JSON Content-Type, pass through
// untouched. A body that claims to be JSON but doesn't parse gets 400 rather
// than being forwarded in a shape the upstream won't expect.
func RequestBody(t *config.BodyTransform, routePath string) func(http.Handler) http.Handler {
	b := newBody(t)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength == 0 || !isJSON(r.Header.Get("Content-Type")) {
				next.ServeHTTP(w, r)
				return
			}
			raw, err := netx.ReadLimited(r.Body, netx.MaxBodyBytes)
			if err != nil {
				httperr.Write(w, httperr.From(err))
				return
			}
			parsed, err := decodeJSON(raw)
			if err != nil {
				httperr.Write(w, &httperr.Error{Status: http.StatusBadRequest, Code: "invalid_json", Message: "request body is not valid JSON", Err: err})
				return
			}
			out, err := json.Marshal(b.apply(parsed, Vars{RequestTime: RequestTime(r.Context()), RoutePath: routePath}))
			if err != nil {
				httperr.Write(w, &httperr.Error{Status: http.StatusInternalServerError, Code: "internal_error", Err: err})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(out))
			r.ContentLength = int64(len(out))
			r.Header.Del("Content-Length")
			next.ServeHTTP(w, r)
		})
	}
}

// ResponseBody rewrites upstream response bodies. It buffers the body (up to
// netx.MaxBodyBytes; larger responses pass through unchanged with a warning),
// so routes using it give up streaming. Upstream error responses are wrapped
// too, keeping their status, so clients of the route always see one shape;
// the gateway's own errors never reach this layer.
func ResponseBody(t *config.BodyTransform, routePath string, logger *slog.Logger, next http.RoundTripper) http.RoundTripper {
	b := newBody(t)
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		// Without the client's Accept-Encoding, Go's transport asks for gzip
		// itself and decompresses it, so we always get plain bytes to parse.
		req.Header.Del("Accept-Encoding")

		resp, err := next.RoundTrip(req)
		if err != nil || !hasBody(req, resp) {
			return resp, err
		}
		if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
			logger.Warn("response body is encoded; passing it through without the transform", "content_encoding", ce)
			return resp, nil
		}

		raw, err := io.ReadAll(io.LimitReader(resp.Body, netx.MaxBodyBytes+1))
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
		if len(raw) > netx.MaxBodyBytes {
			logger.Warn("response body too large to transform; passing it through unchanged", "limit_bytes", netx.MaxBodyBytes)
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}
			return resp, nil
		}
		resp.Body.Close()

		out, err := json.Marshal(b.apply(parseResponse(raw), Vars{
			RequestTime:  RequestTime(req.Context()),
			ResponseTime: time.Now(),
			RoutePath:    routePath,
		}))
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
		resp.Header.Set("Content-Type", "application/json")
		// The body changed, so validators computed over the original are wrong.
		resp.Header.Del("ETag")
		resp.Header.Del("Content-MD5")
		return resp, nil
	})
}

// parseResponse decodes a JSON body. A non-JSON body becomes a string and an
// empty one becomes null, so "$body" always has something sensible to hold.
func parseResponse(raw []byte) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if v, err := decodeJSON(raw); err == nil {
		return v
	}
	return string(raw)
}

func hasBody(req *http.Request, resp *http.Response) bool {
	return req.Method != http.MethodHead &&
		resp.StatusCode >= 200 &&
		resp.StatusCode != http.StatusNoContent &&
		resp.StatusCode != http.StatusNotModified
}
