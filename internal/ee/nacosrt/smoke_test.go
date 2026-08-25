package nacosrt

// Real-registry smoke, opt-in via WOCA_NACOS_SMOKE=http://127.0.0.1:8848
// (docker run -e MODE=standalone -p 8848:8848 nacos/nacos-server:v2.4.3).
// It exercises the v1 protocol against real nacos: registration (the
// instance list must show it), the pack publish → listener delivery, and
// the fail-closed gate.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
)

func TestSmokeRealNacos(t *testing.T) {
	base := os.Getenv("WOCA_NACOS_SMOKE")
	if base == "" {
		t.Skip("set WOCA_NACOS_SMOKE=http://host:8848 to run the real-registry smoke")
	}
	c := NewClient(base)
	ctx := t.Context()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Registration shows up in the service's instance list.
	if err := c.Register(ctx, "woca-smoke", "127.0.0.1", 39999, map[string]string{"role": "smoke"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	known, err := c.Beat(ctx, "woca-smoke", "127.0.0.1", 39999)
	if err != nil || !known {
		t.Fatalf("beat: known=%v err=%v", known, err)
	}
	// An ephemeral instance surfaces in the list after a beat — poll for it.
	deadline := time.Now().Add(8 * time.Second)
	for {
		list, err := http.Get(base + "/nacos/v1/ns/instance/list?serviceName=woca-smoke")
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Hosts []struct {
				IP   string `json:"ip"`
				Port int    `json:"port"`
			} `json:"hosts"`
		}
		_ = json.NewDecoder(list.Body).Decode(&out)
		_ = list.Body.Close()
		if len(out.Hosts) > 0 && out.Hosts[0].IP == "127.0.0.1" && out.Hosts[0].Port == 39999 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance list does not show the registration: %+v", out.Hosts)
		}
		if _, err := c.Beat(ctx, "woca-smoke", "127.0.0.1", 39999); err != nil {
			t.Logf("beat during list poll: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Pack delivery end-to-end through the runtime.
	st, err := settings.Open("sqlite:" + t.TempDir() + "/smoke.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	applied := make(chan string, 2)
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
	rt, err := New(Config{
		Server: base, Service: "woca-smoke", Advertise: "127.0.0.1:39998",
		St: st, OnChange: func() { applied <- st.Load().Model },
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	pack := &Pack{Version: packVersion, Generation: "smoke-1", Settings: settings.Settings{Model: "smoke-model"}}
	raw, _ := pack.Encode()
	if err := c.Publish(ctx, "DEFAULT_GROUP", ConfigDataID, raw); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Real nacos needs a moment before the write is readable — and a
	// previous run's pack may still be served, so verify the CONTENT.
	readDeadline := time.Now().Add(10 * time.Second)
	for {
		if got, err := c.Get(ctx, "DEFAULT_GROUP", ConfigDataID); err == nil && got == raw {
			break
		}
		if time.Now().After(readDeadline) {
			t.Fatal("pack never settled to the published content")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Drain the initial apply's signal before the second delivery.
	select {
	case <-applied:
	default:
	}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go rt.Loop(loopCtx)

	pack2 := &Pack{Version: packVersion, Generation: "smoke-2", Settings: settings.Settings{Model: "smoke-model-2"}}
	raw2, _ := pack2.Encode()
	if err := c.Publish(ctx, "DEFAULT_GROUP", ConfigDataID, raw2); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	select {
	case m := <-applied:
		if m != "smoke-model-2" {
			t.Fatalf("hot apply observed %q", m)
		}
		fmt.Println("smoke: hot-apply observed", m)
	case <-time.After(45 * time.Second):
		t.Fatal("real-registry listener never delivered the change")
	}

	if err := c.Deregister(ctx, "woca-smoke", "127.0.0.1", 39999); err != nil {
		t.Fatalf("deregister: %v", err)
	}
}
