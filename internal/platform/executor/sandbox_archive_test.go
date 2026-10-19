package executor

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newArchiveFakeSandbox(t *testing.T) (*Sandbox, *[]byte) {
	t.Helper()
	var stored []byte
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	withMu := func(f func()) { <-mu; f(); mu <- struct{}{} }
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandbox_id":"sbx_arch"}`))
	})
	mux.HandleFunc("PUT /sandbox/sbx_arch/archive", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/gzip" {
			t.Errorf("archive content-type = %q, want application/gzip", ct)
		}
		if h := r.Header.Get("X-Woca-Sandbox-Contract"); h != "2" {
			t.Errorf("archive contract header = %q", h)
		}
		body, _ := io.ReadAll(r.Body)
		withMu(func() { stored = body })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /sandbox/sbx_arch/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Woca-Sandbox-Contract") != "2" {
			t.Errorf("get archive contract header = %q", r.Header.Get("X-Woca-Sandbox-Contract"))
		}
		withMu(func() {
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(stored)
		})
	})
	mux.HandleFunc("DELETE /sandbox/sbx_arch", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL, APIKey: "ak"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb, &stored
}

func TestSandboxArchiveRoundTrip(t *testing.T) {
	sb, stored := newArchiveFakeSandbox(t)

	// 1 KiB of random payload, gzipped — binary-safe bytes both ways.
	payload := make([]byte, 1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	var upload bytes.Buffer
	zw := gzip.NewWriter(&upload)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sb.PutArchive(t.Context(), bytes.NewReader(upload.Bytes())); err != nil {
		t.Fatalf("PutArchive: %v", err)
	}
	if !bytes.Equal(*stored, upload.Bytes()) {
		t.Fatalf("server stored %d bytes, want %d (byte-identical upload)", len(*stored), upload.Len())
	}

	rc, err := sb.GetArchive(t.Context())
	if err != nil {
		t.Fatalf("GetArchive: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, upload.Bytes()) {
		t.Fatalf("downloaded %d bytes, want byte-identical round-trip", len(got))
	}
}

func TestSandboxArchiveErrorBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sandbox_id":"sbx_archerr"}`))
	})
	mux.HandleFunc("PUT /sandbox/sbx_archerr/archive", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = w.Write([]byte(`{"error":"too big"}`))
	})
	mux.HandleFunc("DELETE /sandbox/sbx_archerr", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sb, _ := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	t.Cleanup(func() { _ = sb.Close() })

	err := sb.PutArchive(t.Context(), bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("PutArchive succeeded despite 507")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("too big")) {
		t.Fatalf("error = %v, want it to surface the backend message", err)
	}
}
