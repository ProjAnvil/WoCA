package webfx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/httpx"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/rs/zerolog"
)

// RequestID propagates or mints an X-Request-Id: inbound values are kept
// only when short and plain (length ≤ 64, ASCII letters/digits/._-),
// otherwise a fresh one is generated. The id is echoed on the response and
// carried in the request context so Logger lines join the same request.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get("X-Request-Id"))
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
		})
	}
}

func sanitizeRequestID(v string) string {
	if len(v) == 0 || len(v) > 64 {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_':
		default:
			return ""
		}
	}
	return v
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

type requestIDKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id installed by the RequestID
// middleware, if any.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Logger emits one structured line per request: method, route (the matched
// ServeMux pattern via r.Pattern — a fixed vocabulary, never a raw path
// list), status, duration, bytes, and the request/tenant/run correlation
// already in the context. Bodies are never logged. Status maps to level:
// 5xx error, 4xx warn, else info.
func Logger(log zerolog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				route := r.Pattern
				if i := strings.IndexByte(route, ' '); i >= 0 {
					route = route[i+1:] // drop the method prefix: pure path vocabulary
				}
				if route == "" {
					route = "unmatched"
				}
				evt := log.With().
					Str("method", r.Method).
					Str("route", route).
					Str("path", r.URL.Path).
					Int("status", sw.status).
					Dur("duration", time.Since(start)).
					Int64("bytes_out", sw.bytes)
				if id := RequestIDFrom(r.Context()); id != "" {
					evt = evt.Str("request_id", id)
				}
				if tc, ok := tenancy.FromContext(r.Context()); ok && tc.TenantID != "" {
					evt = evt.Str("tenant_id", tc.TenantID)
				}
				if sc, ok := telemetry.RunFrom(r.Context()); ok && sc.RunID != "" {
					evt = evt.Str("run_id", sc.RunID)
				}
				l := evt.Logger()
				switch {
				case sw.status >= 500:
					l.Error().Msg("http request")
				case sw.status >= 400:
					l.Warn().Msg("http request")
				default:
					l.Info().Msg("http request")
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// Recovery contains handler panics: before anything was written it answers
// the uniform 500 envelope; once a response started (SSE mid-stream) it
// logs and drops the connection — a 500 can no longer be sent.
// http.ErrAbortHandler is re-panicked: net/http owns that sentinel.
func Recovery(log zerolog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && err == http.ErrAbortHandler {
					panic(rec)
				}
				log.Error().
					Str("method", r.Method).
					Str("path", r.URL.Path).
					Str("request_id", RequestIDFrom(r.Context())).
					Interface("panic", rec).
					Stack().
					Msg("http handler panic")
				if sw, ok := w.(*captureWriter); ok && sw.wroteHeader() {
					return // response already on the wire; nothing to replace
				}
				httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// captureWriter records status and bytes while staying transparent for
// streaming: Flusher, Hijacker, and io.ReaderFrom delegate to the
// underlying writer, and Unwrap lets http.NewResponseController reach the
// original through this layer (the SSE-over-middleware contract).
type captureWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *captureWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *captureWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.bytes += n
		if n > 0 {
			w.wrote = true
		}
		return n, err
	}
	n, err := io.Copy(w, r)
	w.bytes += n
	return n, err
}

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("webfx: underlying writer does not support hijacking")
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *captureWriter) wroteHeader() bool { return w.wrote }
