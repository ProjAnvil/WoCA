package webfx

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/projanvil/woca/internal/platform/httpx"
)

// MaxBodyBytes caps Ctx.Bind decodes. Framework routes carry settings and
// small payloads; anything larger is a bug or an abuse.
const MaxBodyBytes = 32 << 20

// Ctx is the per-request helper handed to framework handlers. It wraps the
// plain net/http pair — no custom context type, no shadowing: c.Request()
// and c.ResponseWriter() are the originals, so Flusher/ResponseController
// semantics inside a handler are exactly net/http's.
type Ctx struct {
	w       http.ResponseWriter
	r       *http.Request
	values  map[string]any
	aborted bool
}

// Request returns the underlying request. Its Pattern field carries the
// matched ServeMux pattern (empty when unmatched).
func (c *Ctx) Request() *http.Request { return c.r }

// ResponseWriter returns the underlying response writer.
func (c *Ctx) ResponseWriter() http.ResponseWriter { return c.w }

// Aborted reports whether Abort was called.
func (c *Ctx) Aborted() bool { return c.aborted }

// JSON writes v as a UTF-8 JSON body.
func (c *Ctx) JSON(status int, v any) { httpx.WriteJSON(c.w, status, v) }

// Error writes the uniform {"error": msg} envelope.
func (c *Ctx) Error(status int, msg string) { httpx.WriteError(c.w, status, msg) }

// Abort writes the error envelope and marks the handler chain aborted.
func (c *Ctx) Abort(status int, msg string) {
	c.aborted = true
	c.Error(status, msg)
}

// Status writes a bare status code with no body.
func (c *Ctx) Status(status int) { c.w.WriteHeader(status) }

// Bind decodes a JSON request body into v, capped at MaxBodyBytes.
func (c *Ctx) Bind(v any) error {
	body := http.MaxBytesReader(c.w, c.r.Body, MaxBodyBytes)
	defer func() { _ = body.Close() }()
	return json.NewDecoder(body).Decode(v)
}

// Param returns the value of the {wildcard} in the matched pattern.
func (c *Ctx) Param(name string) string { return c.r.PathValue(name) }

// Query returns the first query-string value for key.
func (c *Ctx) Query(key string) string { return c.r.URL.Query().Get(key) }

// Set stores a per-request value.
func (c *Ctx) Set(key string, v any) {
	if c.values == nil {
		c.values = map[string]any{}
	}
	c.values[key] = v
}

// Get returns a per-request value set by Set.
func (c *Ctx) Get(key string) (any, bool) {
	v, ok := c.values[key]
	return v, ok
}

// Stream writes a streaming response: headers first, then the first flush
// before copying, then continuous flushing while the body produces — the
// SSE-safe ordering (headers-before-flush) the platform standardizes on.
func (c *Ctx) Stream(status int, contentType string, body io.Reader) error {
	c.w.Header().Set("Content-Type", contentType)
	c.w.WriteHeader(status)
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
	if rf, ok := c.w.(io.ReaderFrom); ok {
		_, err := rf.ReadFrom(body)
		return err
	}
	_, err := io.Copy(c.w, body)
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
	return err
}

// ToHTTP adapts a framework handler to a plain http.Handler.
func ToHTTP(h func(*Ctx)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(&Ctx{w: w, r: r})
	})
}

// FromHTTP adapts a plain http.Handler to a framework handler, for mixing
// existing handlers into Ctx-shaped code.
func FromHTTP(h http.Handler) func(*Ctx) {
	return func(c *Ctx) { h.ServeHTTP(c.w, c.r) }
}
