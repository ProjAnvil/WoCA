package relayclient

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatInjectsPingAfterIdle(t *testing.T) {
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, 60*time.Millisecond)
	defer hb.stop()
	hb.arm()
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		done <- string(data)
	}()
	time.Sleep(200 * time.Millisecond)
	_ = pw.Close()
	got := <-done
	if !strings.Contains(got, ": ping\n\n") {
		t.Fatalf("no heartbeat after idle: %q", got)
	}
	// Pings are standalone comment lines — never torn into data frames
	// (design §6.3 frame safety).
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line != "" && line != ": ping" {
			t.Fatalf("torn frame line %q in %q", line, got)
		}
	}
}

func TestHeartbeatResetsOnData(t *testing.T) {
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, 150*time.Millisecond)
	defer hb.stop()
	got := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		got <- string(data)
	}()
	hb.arm()
	// Frames flow more often than the interval: no ping may interleave.
	for i := 0; i < 3; i++ {
		if _, err := hb.Write([]byte("data: frame\n\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	_ = pw.Close()
	out := <-got
	if strings.Contains(out, ": ping") {
		t.Fatalf("ping injected despite traffic: %q", out)
	}
	if strings.Count(out, "data: frame") != 3 {
		t.Fatalf("frames mangled: %q", out)
	}
}

func TestHeartbeatUnarmedIsPassThrough(t *testing.T) {
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, 20*time.Millisecond)
	defer hb.stop()
	got := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		got <- string(data)
	}()
	// Non-SSE responses never arm: pure pass-through, zero overhead.
	if _, err := hb.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	_ = pw.Close()
	if out := <-got; out != "plain" {
		t.Fatalf("unarmed writer altered the stream: %q", out)
	}
}

func TestHeartbeatStopIsIdempotent(t *testing.T) {
	hb := newHeartbeatWriter(io.Discard, time.Second)
	hb.arm()
	hb.stop()
	hb.stop() // must not panic (execute defers stop on every path)
}
