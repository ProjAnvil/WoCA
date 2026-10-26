package webfx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRouterMethodRouting(t *testing.T) {
	r := New(Options{})
	r.GET("/api/things/{id}", func(c *Ctx) { c.JSON(200, map[string]string{"id": c.Param("id")}) })
	r.POST("/api/things", func(c *Ctx) { c.Status(201) })

	for _, tc := range []struct {
		method, path, wantBody string
		wantStatus             int
	}{
		{"GET", "/api/things/abc", `{"id":"abc"}`, 200},
		{"POST", "/api/things", "", 201},
		{"GET", "/api/nope", "404 page not found\n", 404},
		{"PUT", "/api/things", "Method Not Allowed\n", 405},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, tc.wantStatus)
		}
		if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s %s: body = %q, want to contain %q", tc.method, tc.path, rec.Body.String(), tc.wantBody)
		}
	}
}

func TestRouterPrecedenceLiteralOverWildcard(t *testing.T) {
	r := New(Options{})
	r.GET("/api/things/edit", func(c *Ctx) { c.Status(200) })
	r.GET("/api/things/{id}", func(c *Ctx) { c.Status(299) })
	req := httptest.NewRequest("GET", "/api/things/edit", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("literal must win over wildcard: status = %d", rec.Code)
	}
}

func TestRouterDuplicatePatternPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate pattern must panic at registration (startup error)")
		}
	}()
	r := New(Options{})
	r.GET("/api/x", func(c *Ctx) {})
	r.GET("/api/x", func(c *Ctx) {})
}

func TestGroupScopesMiddleware(t *testing.T) {
	r := New(Options{})
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, req)
			})
		}
	}
	r.Use(mk("global"))
	g := r.Group("/api/v1", mk("groupA"), mk("groupB"))
	g.GET("/things", func(c *Ctx) { c.Status(200) })
	r.GET("/outside", func(c *Ctx) { c.Status(200) })

	req := httptest.NewRequest("GET", "/api/v1/things", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if got := strings.Join(order, ","); got != "global,groupA,groupB" {
		t.Fatalf("group middleware order = %q, want global,groupA,groupB", got)
	}

	order = nil
	req = httptest.NewRequest("GET", "/outside", nil)
	r.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if got := strings.Join(order, ","); got != "global" {
		t.Fatalf("group middleware leaked outside the group: %q", got)
	}
}

func TestChainOrderOutermostFirst(t *testing.T) {
	var order []string
	mk := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), mk("a"), mk("b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got := strings.Join(order, ","); got != "a,b" {
		t.Fatalf("chain order = %q, want a,b (registration order, outermost first)", got)
	}
}

func TestCtxJSONAndErrorEnvelopes(t *testing.T) {
	r := New(Options{})
	r.GET("/ok", func(c *Ctx) { c.JSON(200, map[string]int{"n": 1}) })
	r.GET("/err", func(c *Ctx) { c.Error(409, "boom") })
	h := r.Handler()

	req := httptest.NewRequest("GET", "/ok", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "charset=utf-8") {
		t.Errorf("JSON content type = %q, want explicit utf-8", ct)
	}
	var v map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v["n"] != 1 {
		t.Errorf("JSON body = %q err=%v", rec.Body.String(), err)
	}

	req = httptest.NewRequest("GET", "/err", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"error":"boom"`) {
		t.Errorf("error envelope = %d %q", rec.Code, rec.Body.String())
	}
}

func TestCtxBindCapsBody(t *testing.T) {
	r := New(Options{})
	r.POST("/bind", func(c *Ctx) {
		var v map[string]string
		if err := c.Bind(&v); err != nil {
			c.Error(400, err.Error())
			return
		}
		c.JSON(200, v)
	})
	h := r.Handler()

	body := bytes.Repeat([]byte("a"), MaxBodyBytes+1)
	req := httptest.NewRequest("POST", "/bind", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("oversized body: status = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest("POST", "/bind", strings.NewReader(`{"k":"v"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"k":"v"`) {
		t.Fatalf("normal bind: %d %q", rec.Code, rec.Body.String())
	}
}

func TestCtxValuesQueryStream(t *testing.T) {
	r := New(Options{})
	r.GET("/s", func(c *Ctx) {
		c.Set("marker", "on")
		if v, ok := c.Get("marker"); !ok || v != "on" {
			t.Error("Set/Get round-trip failed")
		}
		if c.Query("q") != "1" {
			t.Errorf("Query = %q, want 1", c.Query("q"))
		}
		_ = c.Stream(200, "text/event-stream", strings.NewReader("data: x\n\n"))
	})
	req := httptest.NewRequest("GET", "/s?q=1", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "data: x\n\n" {
		t.Fatalf("stream body = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("stream content type = %q", ct)
	}
}

func TestAdaptersRoundTrip(t *testing.T) {
	r := New(Options{})
	// An existing stdlib handler mounts directly.
	r.Handle("GET /plain", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	// A Ctx handler wraps an existing handler via FromHTTP.
	r.GET("/mixed", FromHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("from-stdlib"))
	})))
	h := r.Handler()

	req := httptest.NewRequest("GET", "/plain", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("plain handler: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/mixed", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "from-stdlib" {
		t.Fatalf("mixed adapter body = %q", rec.Body.String())
	}
}

func TestUseNilMiddlewareIsNoop(t *testing.T) {
	r := New(Options{})
	r.Use(nil)
	r.GET("/x", func(c *Ctx) { c.Status(200) })
	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("nil middleware broke the chain: %d", rec.Code)
	}
}

func TestOptionsWrapSitsOutermost(t *testing.T) {
	var order []string
	r := New(Options{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			order = append(order, "outer")
			next.ServeHTTP(w, req)
		})
	}})
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			order = append(order, "chain")
			next.ServeHTTP(w, req)
		})
	})
	r.GET("/x", func(c *Ctx) { c.Status(200) })
	r.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if got := strings.Join(order, ","); got != "outer,chain" {
		t.Fatalf("order = %q, want outer,chain", got)
	}
}

// TestStaysVocabularyFree is the webfx companion of the gateway vocabulary
// test: the framework is edition-neutral infrastructure and must not name
// any edition's routes or cookies. Tests are excluded — fixtures may use
// realistic paths.
func TestStaysVocabularyFree(t *testing.T) {
	banned := []string{
		"/api/auth/login", // bridge login (declared by cmd/woca-bridge)
		"/api/native",     // native-client surface (cmd/woca-scheduler)
		"woca_session",    // bridge session cookie (ee/bridge owns it)
		"/gw/",            // machine channel (ee/entrygw owns it)
		"/tunnel/",        // bridge tunnels (ee/edgegw owns it)
	}
	for _, name := range []string{"webfx.go", "ctx.go", "middleware.go", "lifecycle.go", "spa.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range banned {
			if strings.Contains(string(src), token) {
				t.Errorf("webfx/%s knows edition vocabulary: %q", name, token)
			}
		}
	}
}
