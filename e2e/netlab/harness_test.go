//go:build netlab

// netlab — the network-layer docker-isolation integration tests. Each
// topology is a dedicated compose file in this directory; the tests boot
// the final distribution images across docker network zones and assert
// both the positive chain (frontend → relay/edge → tunnel → internal
// bridge) and the negative isolation (cross-zone addresses must be
// unreachable). Design: docs/superpowers/specs/2026-10-06-netlab-design.md.
//
// Gated behind the `netlab` build tag so plain `go test ./...` never
// touches docker:
//
//	make netlab-personal
//	make netlab-enterprise
package netlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	personalCompose   = "docker-compose.personal.yml"
	enterpriseCompose = "docker-compose.enterprise.yml"
	keepEnv           = "NETLAB_KEEP"
)

// stack is one compose topology. All commands run from the package dir;
// the compose build contexts (../../build/dist) resolve relative to the
// compose files themselves.
type stack struct {
	name string // display name for failure messages
	file string
}

// compose runs docker compose for this topology's file (the t-free
// variant composeE does the work; this wrapper marks test helpers).
func (s stack) compose(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	return s.composeE(env, args...)
}

// up brings services up (-d --build) with optional interpolated env
// (NETLAB_RELAY_KEY / NETLAB_ENROLL_TOKEN for the two-phase flows).
func (s stack) up(t *testing.T, env map[string]string, services ...string) {
	t.Helper()
	args := append([]string{"up", "-d", "--build"}, services...)
	if out, errb, err := s.compose(t, env, args...); err != nil {
		t.Fatalf("%s: compose up %v: %v\n%s%s", s.name, services, err, out, errb)
	}
}

// down is a best-effort clean slate (volumes included) — never fatal,
// it also serves as teardown.
func (s stack) down(t *testing.T) {
	t.Helper()
	_, _, _ = s.compose(t, nil, "down", "-v", "--remove-orphans", "--timeout", "20")
}

// logs dumps bounded container logs for connectivity troubleshooting
// when a chain assertion fails (--tail keeps a spammy role — e.g. the
// nacos config-pack polling — from flooding the dump).
func (s stack) logs(t *testing.T) string {
	t.Helper()
	out, _, _ := s.compose(t, nil, "logs", "--no-color", "--timestamps", "--tail", "80")
	return out
}

// execOut runs a command inside a service container and returns stdout.
func (s stack) execOut(t *testing.T, service string, args ...string) (string, error) {
	t.Helper()
	return s.execE(service, args...)
}

// execE is the t-free exec used by polling probes.
func (s stack) execE(service string, args ...string) (string, error) {
	full := append([]string{"exec", "-T", service}, args...)
	out, errb, err := s.composeE(nil, full...)
	if err != nil {
		return out, fmt.Errorf("exec %s %v: %w\n%s%s", service, args, err, out, errb)
	}
	return out, nil
}

// composeE is compose without a testing.T (returns buffers directly).
func (s stack) composeE(env map[string]string, args ...string) (string, string, error) {
	full := []string{"compose", "-f", s.file}
	full = append(full, args...)
	cmd := exec.Command("docker", full...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// execOK asserts an in-container command exits zero.
func (s stack) execOK(t *testing.T, service string, args ...string) {
	t.Helper()
	if _, err := s.execOut(t, service, args...); err != nil {
		t.Fatalf("%s: exec %s %v: %v", s.name, service, args, err)
	}
}

// lifecycle installs the shared teardown: full log dump on failure, then
// the zone-isolation rules are released while the bridges still exist,
// then down -v (kept up instead when NETLAB_KEEP=1 for manual 排查).
func (s stack) lifecycle(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s: failure log dump:\n%s", s.name, s.logs(t))
		}
		if os.Getenv(keepEnv) == "1" {
			t.Logf("%s: NETLAB_KEEP=1 — stack left running for troubleshooting", s.name)
			return
		}
		releaseIsolation()
		s.down(t)
	})
	// Clean slate of any earlier run's project/volumes.
	s.down(t)
}

// requireDocker skips the topology unless a docker engine + compose are
// usable, so a bare `go test -tags netlab` on a docker-less box fails
// with a clear skip rather than a stack trace.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skipf("docker compose unavailable: %v", err)
	}
}

// requireTarballs points at the Makefile when the distribution packages
// the compose files bake into images are missing.
func requireTarballs(t *testing.T, roles ...string) {
	t.Helper()
	for _, r := range roles {
		p := filepath.Join("..", "..", "build", "dist", r+".tar.gz")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing — build the role packages first (make netlab-personal / netlab-enterprise): %v", p, err)
		}
	}
}

// waitFor polls probe until it reports done or the timeout elapses; the
// probe's message is the last-failure evidence in the fatal line.
func waitFor(t *testing.T, timeout time.Duration, desc string, probe func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, msg := probe()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s (last state: %s)", t.Name(), desc, msg)
		}
		time.Sleep(3 * time.Second)
	}
}

// containerIP resolves a container's address on one specific docker
// network — the target address for the cross-zone isolation probes.
func containerIP(t *testing.T, container, network string) string {
	t.Helper()
	tmpl := fmt.Sprintf(`{{range $k,$v := .NetworkSettings.Networks}}{{if eq $k "%s"}}{{$v.IPAddress}}{{end}}{{end}}`, network)
	out, err := exec.Command("docker", "inspect", "-f", tmpl, container).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", container, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		t.Fatalf("container %s has no address on network %s", container, network)
	}
	return ip
}

// ───────────────── client-side probes (the external web frontend) ─────────────────
// curl runs inside the client container, which shares ONLY the front
// network — a request from here proves the path across the boundary.

func (s stack) curlCode(t *testing.T, maxSec int, args ...string) (string, string) {
	t.Helper()
	full := append([]string{"curl", "-sS", "-m", fmt.Sprint(maxSec), "-o", "/tmp/netlab.body", "-w", "%{http_code}"}, args...)
	out, err := s.execOut(t, "client", full...)
	code := strings.TrimSpace(out)
	if err != nil && code == "" {
		return "000", err.Error()
	}
	body := s.readBody(t)
	return code, body
}

func (s stack) readBody(t *testing.T) string {
	t.Helper()
	out, err := s.execOut(t, "client", "cat", "/tmp/netlab.body")
	if err != nil {
		return ""
	}
	return out
}

// login performs the browser login POST from the client container; the
// cookie jar persists in the container for the tunneled follow-ups.
func (s stack) login(t *testing.T, base, key, jar string) (code int, body string) {
	t.Helper()
	args := []string{
		"curl", "-sS", "-m", "60", "-c", jar, "-b", jar,
		"-o", "/tmp/netlab.login", "-w", "%{http_code}",
		"-H", "Content-Type: application/json",
		"-d", `{"api_key":"` + key + `"}`,
		base + "/api/auth/login",
	}
	// A transport error still prints "000" via -w; the body read below
	// then carries whatever (if anything) came back.
	out, _ := s.execOut(t, "client", args...)
	body, _ = s.execOut(t, "client", "cat", "/tmp/netlab.login")
	var n int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n, body
}

// tunneledGet fetches an authenticated API path with the login jar.
func (s stack) tunneledGet(t *testing.T, base, path, jar string) (int, string) {
	t.Helper()
	args := []string{
		"curl", "-sS", "-m", "60", "-b", jar,
		"-o", "/tmp/netlab.get", "-w", "%{http_code}",
		base + path,
	}
	out, _ := s.execOut(t, "client", args...)
	var n int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	body, _ := s.execOut(t, "client", "cat", "/tmp/netlab.get")
	return n, body
}

// sseHeaders collects response headers of the SSE endpoint (curl exits
// nonzero when --max-time cuts the stream — the headers are the
// contract, same posture as ci/e2e-relay.sh).
func (s stack) sseHeaders(t *testing.T, base, jar string) string {
	t.Helper()
	out, _ := s.execOut(t, "client", "curl", "-sS", "-m", "8", "-D", "-", "-o", "/dev/null", "-b", jar, base+"/api/events")
	return out
}

// unreachableFromClient asserts a cross-zone address does NOT answer:
// docker drops inter-bridge traffic, so the probe must fail (timeout or
// refusal), never return an HTTP status.
func (s stack) unreachableFromClient(t *testing.T, url string) {
	t.Helper()
	out, err := s.execOut(t, "client", "curl", "-sS", "-m", "6", "-o", "/dev/null", "-w", "%{http_code}", url)
	if err == nil {
		t.Fatalf("isolation violated: client reached %s (status %q)", url, strings.TrimSpace(out))
	}
}

// reachableFromClient asserts the opposite for front-network targets.
func (s stack) reachableFromClient(t *testing.T, url string, wantCode int) {
	t.Helper()
	code, body := s.curlCode(t, 15, url)
	if code != fmt.Sprint(wantCode) {
		t.Fatalf("client → %s: status %s (want %d), body: %s", url, code, wantCode, body)
	}
}

// jsonField extracts one string field from a JSON document produced by
// an in-container command (used for the headless credential minting).
func jsonField(t *testing.T, raw, field string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("mint response not JSON (%s): %v", raw, err)
	}
	v, _ := doc[field].(string)
	if v == "" {
		t.Fatalf("mint response lacks %q: %s", field, raw)
	}
	return v
}

// ───────────────── zone isolation (the DOCKER-USER netguard) ─────────────────
// Docker 28/29 dropped the inter-bridge DOCKER-ISOLATION-STAGE rules:
// the new DOCKER-FORWARD chain ACCEPTS any traffic entering from a
// bridge, so containers on different user-defined networks can reach
// each other by address — empirically confirmed on this project's
// engines (29.4), including traffic INTO `internal` networks. The zone
// matrix the netlab topologies stand for (front × wan × core pairwise
// isolated; every allowed conversation shares one bridge) is therefore
// enforced explicitly in DOCKER-USER, docker's documented policy hook.
// On engines that still isolate bridges by default these rules are
// inert no-ops.

const guardImage = "woca-netlab-iptables:e2e"

var guardRules []string // "brA>brB" pairs inserted this run, for teardown

// buildGuardImage bakes the iptables toolbox once per test binary.
func buildGuardImage(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "build", "-q", "-t", guardImage, "iptables")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guard image build: %v\n%s", err, out)
	}
}

// bridgeName maps a docker network to its host-namespace bridge
// interface (br-<first 12 of the network id>).
func bridgeName(t *testing.T, network string) string {
	t.Helper()
	out, err := exec.Command("docker", "network", "inspect", "-f", "{{.Id}}", network).Output()
	if err != nil {
		t.Fatalf("network inspect %s: %v", network, err)
	}
	id := strings.TrimSpace(string(out))
	if len(id) < 12 {
		t.Fatalf("network %s: unexpected id %q", network, id)
	}
	return "br-" + id[:12]
}

// isolate installs both-direction DROP rules between each network pair.
// Idempotent (-C before -I); call only after the networks exist. The
// inserted pairs are recorded for releaseIsolation at teardown.
func isolate(t *testing.T, pairs ...[2]string) {
	t.Helper()
	buildGuardImage(t)
	for _, p := range pairs {
		a, b := bridgeName(t, p[0]), bridgeName(t, p[1])
		script := fmt.Sprintf(
			`iptables -C DOCKER-USER -i %s -o %s -j DROP 2>/dev/null || iptables -I DOCKER-USER 1 -i %s -o %s -j DROP; `+
				`iptables -C DOCKER-USER -i %s -o %s -j DROP 2>/dev/null || iptables -I DOCKER-USER 1 -i %s -o %s -j DROP`,
			a, b, a, b, b, a, b, a)
		cmd := exec.Command("docker", "run", "--rm", "--privileged", "--net", "host",
			"--entrypoint", "sh", guardImage, "-c", script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolate %s×%s: %v\n%s", p[0], p[1], err, out)
		}
		guardRules = append(guardRules, a+">"+b, b+">"+a)
		t.Logf("zone isolation armed: %s × %s (%s, %s)", p[0], p[1], a, b)
	}
}

// releaseIsolation removes the rules isolate installed (best effort —
// called before the networks go away so the rule specs still resolve).
func releaseIsolation() {
	for _, r := range guardRules {
		parts := strings.SplitN(r, ">", 2)
		if len(parts) != 2 {
			continue
		}
		script := fmt.Sprintf(`iptables -D DOCKER-USER -i %s -o %s -j DROP 2>/dev/null || true`, parts[0], parts[1])
		_ = exec.Command("docker", "run", "--rm", "--privileged", "--net", "host",
			"--entrypoint", "sh", guardImage, "-c", script).Run()
	}
	guardRules = nil
}
