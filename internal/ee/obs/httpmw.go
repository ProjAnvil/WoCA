package obs

import (
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/telemetry"
	"github.com/projanvil/woca/internal/webfx"
)

// Span attribute keys (OTel HTTP semconv; woca.* for correlation keys
// with no convention yet).
const (
	attrHTTPMethod     = "http.request.method"
	attrHTTPRoute      = "http.route"
	attrHTTPStatusCode = "http.response.status_code"
	attrServiceName    = "service.name"
	attrRequestID      = "woca.http.request_id"
	attrTenantID       = "woca.tenant_id"
)

// HTTPMiddleware records one telemetry span per request: the HTTP-side
// counterpart of model.Observe (chat spans) and the tool registry (tool
// spans). Attributes carry the matched route pattern (fixed cardinality,
// read from r.Pattern after the mux ran), the response status, duration,
// the service's registry identity (WOCA_SERVICE_NAME — in full-stack
// deployments the nacos service name; spans export to the
// nacos-resolved collector via obs.Setup), and the request id for
// joining with webfx request log lines. The middleware is safe both
// inside and outside the webfx RequestID middleware: it reads the id
// from the context first, falling back to the echoed X-Request-Id
// response header after the handler returned.
func (o *Obs) HTTPMiddleware() webfx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sp := telemetry.Start(r.Context(), "http", telemetry.KindServer).
				Set(attrHTTPMethod, r.Method).
				Set(attrServiceName, o.serviceName)
			if tc, ok := tenancy.FromContext(r.Context()); ok && tc.TenantID != "" {
				sp = sp.Set(attrTenantID, tc.TenantID)
			}
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			defer func() {
				route := r.Pattern
				if i := strings.IndexByte(route, ' '); i >= 0 {
					route = route[i+1:] // drop the method prefix: pure path vocabulary
				}
				if route == "" {
					route = "unmatched"
				}
				sp.Set(attrHTTPRoute, route).
					Set(attrHTTPStatusCode, sw.status).
					Set("http.duration_ms", time.Since(start).Milliseconds())
				if id := webfx.RequestIDFrom(r.Context()); id != "" {
					sp = sp.Set(attrRequestID, id)
				} else if id := w.Header().Get("X-Request-Id"); id != "" {
					// RequestID ran inside (e.g. server core chain) and
					// echoed the id on the way out.
					sp = sp.Set(attrRequestID, id)
				}
				var err error
				if sw.status >= 500 {
					err = errHTTPStatus{code: sw.status}
				}
				sp.End(r.Context(), err)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

type errHTTPStatus struct{ code int }

func (e errHTTPStatus) Error() string { return "http " + http.StatusText(e.code) }
