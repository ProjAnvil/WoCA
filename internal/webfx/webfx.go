// Package webfx is woca's web framework: the API shape of gin/fiber on the
// engine of the standard library. A Router wraps http.ServeMux (method
// patterns, {wildcards}, most-specific-wins, r.Pattern for low-cardinality
// labels), Use accepts plain net/http middleware so existing chains
// (gateway, consoleauth, obs) plug in unchanged, and handlers may be
// either func(*Ctx) for ergonomics or plain http.Handler for SSE/proxy
// surfaces that must not be rewritten.
//
// Edition boundary: this package depends only on the standard library,
// zerolog, and internal/platform — never on internal/ee.
package webfx

import (
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// Middleware is the standard net/http middleware signature. The gateway
// chain, console gates, and edgegw checks are already this shape.
type Middleware = func(http.Handler) http.Handler

// TLSConfig terminates TLS on the listener Run binds.
type TLSConfig struct {
	CertFile string
	KeyFile  string
}

// Options configures a Router. Zero values are valid: Logger defaults to
// a disabled logger, Grace and ReadHeaderTimeout take the defaults below.
type Options struct {
	// Logger backs the Logger/Recovery middleware and UseDefaults.
	Logger zerolog.Logger
	// Addr is the address Run binds (ignored when Serve is given a
	// listener).
	Addr string
	// Grace bounds the shutdown window once the run context is cancelled.
	// Default 3s (today's per-role actuals); long-SSE surfaces (edge) pass
	// 40s.
	Grace time.Duration
	// ReadHeaderTimeout bounds header reads. Default 15s.
	ReadHeaderTimeout time.Duration
	// TLS, when set, wraps the listener Run binds.
	TLS *TLSConfig
	// Wrap, when set, is applied outside the middleware chain after
	// assembly — the slot for probe planes (ee/obs.Wrap) whose health and
	// metrics endpoints must sit at the outermost edge, ahead of any
	// middleware.
	Wrap Middleware
}

const (
	defaultGrace            = 3 * time.Second
	defaultReadHeaderTimout = 15 * time.Second
)

// Router is the framework entry point: a route table plus an ordered
// middleware chain.
type Router struct {
	opts  Options
	mux   *http.ServeMux
	chain []Middleware
}

// New returns an empty Router.
func New(o Options) *Router {
	return &Router{opts: o, mux: http.NewServeMux()}
}

// Use appends net/http middleware, outermost first (registration order).
// Passing a nil middleware is a no-op so callers can compose
// conditionally without slicing.
func (r *Router) Use(mw ...Middleware) {
	for _, m := range mw {
		if m != nil {
			r.chain = append(r.chain, m)
		}
	}
}

// UseDefaults installs the framework's standard chain in order:
// RequestID, Logger, Recovery — backed by Options.Logger.
func (r *Router) UseDefaults() {
	r.Use(RequestID(), Logger(r.opts.Logger), Recovery(r.opts.Logger))
}

// Handle registers pattern (a stdlib ServeMux pattern, method prefix and
// {wildcards} included) with a plain http.Handler. SSE and proxy handlers
// register here unchanged.
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mux.Handle(pattern, h)
}

// HandleFunc registers pattern with a stdlib handler function.
func (r *Router) HandleFunc(pattern string, h http.HandlerFunc) {
	r.mux.Handle(pattern, h)
}

// Ctx registers pattern (stdlib pattern syntax, no method prefix) for
// every method with a framework handler.
func (r *Router) Ctx(pattern string, h func(*Ctx)) {
	r.mux.Handle(pattern, ToHTTP(h))
}

// GET registers h for GET pattern (and, following ServeMux semantics,
// HEAD).
func (r *Router) GET(pattern string, h func(*Ctx)) { r.mux.Handle("GET "+pattern, ToHTTP(h)) }

// POST registers h for POST pattern.
func (r *Router) POST(pattern string, h func(*Ctx)) { r.mux.Handle("POST "+pattern, ToHTTP(h)) }

// PUT registers h for PUT pattern.
func (r *Router) PUT(pattern string, h func(*Ctx)) { r.mux.Handle("PUT "+pattern, ToHTTP(h)) }

// DELETE registers h for DELETE pattern.
func (r *Router) DELETE(pattern string, h func(*Ctx)) { r.mux.Handle("DELETE "+pattern, ToHTTP(h)) }

// PATCH registers h for PATCH pattern.
func (r *Router) PATCH(pattern string, h func(*Ctx)) { r.mux.Handle("PATCH "+pattern, ToHTTP(h)) }

// Group scopes a prefix and a set of middleware to a subtree. Routes
// registered on the group get the group's middleware (outermost first)
// wrapped around them; other routes are unaffected.
func (r *Router) Group(prefix string, mw ...Middleware) *Group {
	return &Group{router: r, prefix: prefix, mw: mw}
}

// Group is a scoped route registrar created by Router.Group.
type Group struct {
	router *Router
	prefix string
	mw     []Middleware
}

// Handle registers a plain http.Handler under the group prefix with the
// group's middleware applied. pattern is a stdlib ServeMux pattern (with
// method prefix when needed); the group prefix is prepended to its path.
func (g *Group) Handle(pattern string, h http.Handler) {
	g.router.mux.Handle(g.prefix+pattern, Chain(h, g.mw...))
}

// HandleFunc registers a stdlib handler function under the group prefix.
func (g *Group) HandleFunc(pattern string, h http.HandlerFunc) {
	g.Handle(pattern, h)
}

// Ctx registers an all-methods framework handler under the group prefix.
func (g *Group) Ctx(pattern string, h func(*Ctx)) { g.Handle(pattern, ToHTTP(h)) }

// GET registers a framework handler for GET under the group prefix.
func (g *Group) GET(pattern string, h func(*Ctx)) { g.method("GET", pattern, h) }

// POST registers a framework handler for POST under the group prefix.
func (g *Group) POST(pattern string, h func(*Ctx)) { g.method("POST", pattern, h) }

// PUT registers a framework handler for PUT under the group prefix.
func (g *Group) PUT(pattern string, h func(*Ctx)) { g.method("PUT", pattern, h) }

// DELETE registers a framework handler for DELETE under the group prefix.
func (g *Group) DELETE(pattern string, h func(*Ctx)) { g.method("DELETE", pattern, h) }

// PATCH registers a framework handler for PATCH under the group prefix.
func (g *Group) PATCH(pattern string, h func(*Ctx)) { g.method("PATCH", pattern, h) }

func (g *Group) method(m, pattern string, h func(*Ctx)) {
	g.router.mux.Handle(m+" "+g.prefix+pattern, Chain(ToHTTP(h), g.mw...))
}

// SetWrap installs the outermost wrapper (Options.Wrap) after
// construction, for probe planes decided later than assembly.
func (r *Router) SetWrap(mw Middleware) { r.opts.Wrap = mw }

// Handler returns the assembled chain around the route table. ServeMux
// panics at registration time on conflicting patterns — a duplicate route
// is a startup error by construction.
func (r *Router) Handler() http.Handler {
	h := Chain(r.mux, r.chain...)
	if r.opts.Wrap != nil {
		h = r.opts.Wrap(h)
	}
	return h
}

// Chain wraps h with mw, outermost first. The one composition helper the
// framework needs; exported for callers (the server core) that mount the
// middleware around an existing handler rather than through a Router.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
