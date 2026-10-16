// Package router matches request paths to configured routes.
package router

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/sgteeples/gateway-kit/internal/httperr"
)

// Route is one entry in the routing table. Handler runs the route's full
// pipeline; the router only decides which route applies.
type Route struct {
	Path    string   // normalized: leading slash, no trailing slash
	Methods []string // upper-case; empty means any method
	Handler http.Handler
}

// Router does longest-prefix matching on path-segment boundaries:
// "/api/users" matches "/api/users" and "/api/users/42" but not "/api/usersX".
type Router struct {
	routes []Route
}

func New(routes []Route) *Router {
	sorted := slices.Clone(routes)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Path) > len(sorted[j].Path) })
	return &Router{routes: sorted}
}

// Match returns the most specific route for path, or nil.
func (rt *Router) Match(path string) *Route {
	for i := range rt.routes {
		if matches(rt.routes[i].Path, path) {
			return &rt.routes[i]
		}
	}
	return nil
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := rt.Match(r.URL.Path)
	if route == nil {
		httperr.Write(w, &httperr.Error{Status: http.StatusNotFound, Code: "not_found", Message: "no route matches " + r.URL.Path})
		return
	}
	if len(route.Methods) > 0 && !slices.Contains(route.Methods, r.Method) {
		w.Header().Set("Allow", strings.Join(route.Methods, ", "))
		httperr.Write(w, &httperr.Error{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: r.Method + " is not allowed on " + route.Path})
		return
	}
	route.Handler.ServeHTTP(w, r)
}

func matches(prefix, path string) bool {
	if prefix == "/" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
