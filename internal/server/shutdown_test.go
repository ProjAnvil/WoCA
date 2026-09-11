package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"
)

// Cancellation with a connection stuck pre-request (accepted, never sent a
// byte) must still be a clean stop: the 3s grace window elapses, the server
// force-closes, and ServeStarted reports nil — a shutdown-deadline error is
// not a serve failure.
func TestServeStartedCancelWithStuckConn(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: t.TempDir()})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler()}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	// A client that connects and never sends a request keeps the connection
	// in StateNew, which http.Server.Shutdown waits on.
	stuck, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeStarted(ctx, hs, serveErr) }()

	// Let the server register the stuck connection before cancelling.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeStarted with stuck conn = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeStarted did not return after cancel with stuck conn")
	}
}

// Cancellation with an in-flight slow request returns once the grace window
// elapses instead of blocking on the handler, and still reports nil.
func TestServeStartedCancelWithSlowRequest(t *testing.T) {
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: t.TempDir()})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // force-close cancels the request context
	})
	slow := &http.Server{Handler: mux}
	serveErr := make(chan error, 1)
	go func() { serveErr <- slow.Serve(ln) }()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the handler start

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ServeStarted(ctx, slow, serveErr) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeStarted with slow request = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeStarted did not return after cancel with slow request")
	}
}
