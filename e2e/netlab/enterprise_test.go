//go:build netlab

package netlab

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestEnterpriseEdgeChain — enterprise private-cloud network topology: the
// public web frontend (client, front network only) → the stateless edge
// access point → the outbound tunnel → the remote-site bridge (wan network
// only); the edge also forwards the machine channel to the private-cloud
// core (internal network). Enrollment (bridge→edge→core entrygw), login
// (answered at the bridge, verified at the core) and the business API must
// all cross the three network boundaries; the core and bridge direct
// addresses must be unreachable from the public side, and the bridge must
// be unreachable from the core's direct address too.
func TestEnterpriseEdgeChain(t *testing.T) {
	requireDocker(t)
	requireTarballs(t, "woca-scheduler", "woca-config", "woca-edge", "woca-bridge")
	st := stack{name: "netlab enterprise", file: enterpriseCompose}
	st.lifecycle(t)

	// Phase 1 — the private core + edge (pg/nacos ride along as the
	// fail-closed dependencies of the registry-coupled roles).
	st.up(t, nil, "server", "config", "edge", "client")
	// The networks exist now — arm the zone matrix: front × wan × core
	// pairwise isolated; every allowed conversation (client↔edge,
	// bridge↔edge, edge↔core) shares exactly one bridge.
	isolate(t,
		[2]string{"woca-netlab-ee-front", "woca-netlab-ee-wan"},
		[2]string{"woca-netlab-ee-front", "woca-netlab-ee-core"},
		[2]string{"woca-netlab-ee-wan", "woca-netlab-ee-core"},
	)
	waitFor(t, 300*time.Second, "core server never became healthy", func() (bool, string) {
		return st.probeInContainer("server", "http://127.0.0.1:8080/healthz")
	})
	waitFor(t, 90*time.Second, "config plane never became healthy", func() (bool, string) {
		return st.probeInContainer("config", "http://127.0.0.1:8080/api/health")
	})
	waitFor(t, 90*time.Second, "edge never became healthy", func() (bool, string) {
		return st.probeInContainer("edge", "http://127.0.0.1:8080/api/health")
	})

	// P1 — the frontend reaches the edge's public surface (the only
	// published entry of the private cloud).
	st.reachableFromClient(t, "http://edge:8080/api/health", 200)

	// Headless credential minting over the server's open-console
	// management listener (:8090, core-only) — the ops flow without the
	// console roles: one-time enrollment token + one tenant-admin key.
	enrollRaw, err := st.execOut(t, "server", "wget", "-qO-",
		"--header", "Content-Type: application/json", "--post-data", "",
		"http://127.0.0.1:8090/api/gw/enrollments")
	if err != nil {
		t.Fatalf("mint enrollment token: %v", err)
	}
	enrollToken := jsonField(t, enrollRaw, "enrollment_token")
	keyRaw, err := st.execOut(t, "server", "wget", "-qO-",
		"--header", "Content-Type: application/json",
		"--post-data", `{"remark":"netlab","role":"tenant-admin"}`,
		"http://127.0.0.1:8090/api/tenant/users")
	if err != nil {
		t.Fatalf("mint tenant user key: %v", err)
	}
	userKey := jsonField(t, keyRaw, "key")
	t.Logf("core minted: enroll %s…, user key %s…", enrollToken[:14], userKey[:10])

	// Phase 2 — the bridge on the far side of wan, enrolling outbound
	// through the edge (real /gw/enroll flow across both boundaries).
	st.up(t, map[string]string{"NETLAB_ENROLL_TOKEN": enrollToken}, "bridge")

	// P5 — registration surfaced: the edge inventory lists the bridge live.
	waitFor(t, 180*time.Second, "bridge never enrolled/live at the edge", func() (bool, string) {
		code, body := st.curlCode(t, 15, "http://edge:8080/api/edge/bridges")
		if code != "200" {
			return false, fmt.Sprintf("inventory = %s: %s", code, body)
		}
		var bridges []struct {
			BridgeID string `json:"bridge_id"`
			Live     bool   `json:"live"`
		}
		if err := json.Unmarshal([]byte(body), &bridges); err != nil {
			return false, fmt.Sprintf("inventory not JSON: %v (%s)", err, body)
		}
		for _, b := range bridges {
			if b.Live {
				return true, ""
			}
		}
		return false, "no live bridge yet: " + body
	})

	// P4 — the SPA itself rides the tunnel: GET / parks at the edge
	// until the bridge answers with its embedded frontend.
	waitFor(t, 120*time.Second, "SPA through the edge tunnel never loaded", func() (bool, string) {
		code, body := st.curlCode(t, 45, "http://edge:8080/")
		return code == "200" && strings.Contains(strings.ToLower(body), "<html"), fmt.Sprintf("GET / = %s, body head = %.120s", code, body)
	})

	// P2 — login (bridge answers, core validates over the machine
	// channel), then a tunneled business API round trip.
	waitFor(t, 150*time.Second, "login through the edge never succeeded", func() (bool, string) {
		code, body := st.login(t, "http://edge:8080", userKey, "/tmp/netlab.jar")
		return code == 200 && strings.Contains(body, `"token"`), fmt.Sprintf("login = %d, body = %s", code, body)
	})
	waitFor(t, 150*time.Second, "tunneled /api/sessions never answered", func() (bool, string) {
		code, body := st.tunneledGet(t, "http://edge:8080", "/api/sessions", "/tmp/netlab.jar")
		return code == 200 && strings.HasPrefix(strings.TrimSpace(body), "["), fmt.Sprintf("GET = %d, body = %s", code, body)
	})

	// P3 — SSE through the edge tunnel keeps the flush contract.
	headers := st.sseHeaders(t, "http://edge:8080", "/tmp/netlab.jar")
	for _, want := range []string{"text/event-stream", "x-accel-buffering: no"} {
		if !strings.Contains(strings.ToLower(headers), want) {
			t.Fatalf("SSE headers through the edge lack %q:\n%s", want, headers)
		}
	}

	// E2 — a wrong org key must surface as 401 at the public boundary
	// (the rejection travels core → bridge → edge → client).
	code, body := st.login(t, "http://edge:8080", "woca_netlab-bogus-key", "/tmp/netlab.bad.jar")
	if code != 401 {
		t.Fatalf("bogus-key login = %d (want 401), body: %s", code, body)
	}

	// N1 — the bridge's wan address must not be reachable from the
	// public side.
	bridgeIP := containerIP(t, "woca-netlab-ee-bridge", "woca-netlab-ee-wan")
	st.unreachableFromClient(t, "http://"+bridgeIP+":8080/api/health")

	// N2 — the private core must not be reachable from the public side
	// (neither the API nor the GW listener).
	serverIP := containerIP(t, "woca-netlab-ee-server", "woca-netlab-ee-core")
	st.unreachableFromClient(t, "http://"+serverIP+":8080/healthz")
	st.unreachableFromClient(t, "http://"+serverIP+":8090/gw/hello")

	// N3 — single control-plane peer: the bridge must not reach the core
	// directly either, only through the edge.
	if _, err := st.execE("bridge", "wget", "-q", "-T", "5", "-O", "/dev/null", "http://"+serverIP+":8090/gw/hello"); err == nil {
		t.Fatalf("isolation violated: bridge reached the core directly at %s:8090", serverIP)
	}

	t.Logf("enterprise edge chain green: client(front) → edge → tunnel → bridge(wan %s); core(%s) unreachable from both", bridgeIP, serverIP)
}
