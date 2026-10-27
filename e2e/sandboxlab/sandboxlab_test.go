//go:build sandboxlab

// sandboxlab — the sandbox execution-chain e2e (plan task 27, design
// docs/superpowers/specs/2026-10-19-sandbox-console-design.md §6): one
// compose topology (docker-compose.yml beside this file) carrying the
// full off-host chain, driven end to end over the real HTTP surfaces —
//
//	configsvc registers + verifies + enables a contract-http backend
//	(the fakesandbox container) → a remote tenant (key minted on the
//	server's GW console API) creates a run → the worker claims it and
//	executes the scripted model's Write call inside the sandbox.
//
// No files move anywhere (the file-transfer ruling, spec
// docs/superpowers/specs/2026-10-26-woca-ee-sandbox-plane-design.md
// §5.3): no upload, no download, no sync — the run's products stay
// wherever the backend put them, and the host workspace is untouched.
// The mount semantics (products landing on the volume in place) are
// covered by the executor's backend tests and the ent demo, which run a
// mount-capable executor; this lab's contract fake has no mount, so the
// product assertion here is the negative one: nothing was copied back.
//
// Negative posture first: before any backend exists the same remote
// tenant gets 403 (the dynamic RunsOffHost gate, design §10.2 M-3).
//
// Gated behind the `sandboxlab` build tag so plain `go test ./...`
// never touches docker:
//
//	make sandboxlab
package sandboxlab

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const composeFile = "docker-compose.yml"

// workspaceEnv pins the host bind mount every compose interpolation
// needs (the shared /data the run executes against).
func workspaceEnv(ws string) map[string]string {
	return map[string]string{"SANDBOXLAB_WORKSPACE": ws}
}

// compose runs docker compose for this topology (the t-free composeE
// does the work; this wrapper marks test helpers).
func compose(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	return composeE(env, args...)
}

func composeE(env map[string]string, args ...string) (string, string, error) {
	full := append([]string{"compose", "-f", composeFile}, args...)
	cmd := exec.Command("docker", full...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func up(t *testing.T, ws string, services ...string) {
	t.Helper()
	args := append([]string{"up", "-d", "--build"}, services...)
	if out, errb, err := compose(t, workspaceEnv(ws), args...); err != nil {
		t.Fatalf("compose up %v: %v\n%s%s", services, err, out, errb)
	}
}

func down(t *testing.T, ws string) {
	t.Helper()
	_, _, _ = compose(t, workspaceEnv(ws), "down", "-v", "--remove-orphans", "--timeout", "20")
}

func logs(t *testing.T, ws string) string {
	t.Helper()
	out, _, _ := compose(t, workspaceEnv(ws), "logs", "--no-color", "--timestamps", "--tail", "60")
	return out
}

// execOut runs a command inside a service container and returns stdout
// (errors carry the captured output for probe retries).
func execOut(service string, args ...string) (string, error) {
	full := append([]string{"exec", "-T", service}, args...)
	out, errb, err := composeE(nil, full...)
	if err != nil {
		return out, fmt.Errorf("exec %s %v: %w\n%s%s", service, args, err, out, errb)
	}
	return out, nil
}

func execOK(t *testing.T, service string, args ...string) string {
	t.Helper()
	out, err := execOut(service, args...)
	if err != nil {
		t.Fatalf("exec %s %v: %v", service, args, err)
	}
	return out
}

// requireDocker skips unless a docker engine + compose are usable.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skipf("docker compose unavailable: %v", err)
	}
}

// requireTarballs points at the Makefile when the role packages the
// compose files bake into images are missing (make sandboxlab builds
// them; the fakemodel image needs no package).
func requireTarballs(t *testing.T, roles ...string) {
	t.Helper()
	for _, r := range roles {
		p := filepath.Join("..", "..", "build", "dist", r+".tar.gz")
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing — build the role packages first (make sandboxlab): %v", p, err)
		}
	}
}

// waitFor polls probe until done or timeout; the probe's message is the
// last-failure evidence in the fatal line.
func waitFor(t *testing.T, timeout time.Duration, desc string, probe func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, msg := probe()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (last state: %s)", desc, msg)
		}
		time.Sleep(3 * time.Second)
	}
}

// ───────────────── client-side probes (the curl container) ─────────────────

// curlCode runs one curl from the client container and returns the
// status code plus the response body.
func curlCode(t *testing.T, maxSec int, args ...string) (string, string) {
	t.Helper()
	full := append([]string{"curl", "-sS", "-m", fmt.Sprint(maxSec),
		"-o", "/tmp/sl.body", "-w", "%{http_code}"}, args...)
	out, err := execOut("client", full...)
	code := strings.TrimSpace(out)
	if err != nil && code == "" {
		return "000", err.Error()
	}
	body, _ := execOut("client", "cat", "/tmp/sl.body")
	return code, body
}

// jsonField extracts one string field from a JSON document.
func jsonField(t *testing.T, raw, field string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("response not JSON (%s): %v", raw, err)
	}
	v, _ := doc[field].(string)
	if v == "" {
		t.Fatalf("response lacks %q: %s", field, raw)
	}
	return v
}

// mintTenantKey mints one tenant-admin key on the server's open-console
// GW listener (:8090, core-only) — the headless ops flow, same as the
// netlab enterprise topology.
func mintTenantKey(t *testing.T) string {
	t.Helper()
	raw := execOK(t, "server", "wget", "-qO-",
		"--header", "Content-Type: application/json",
		"--post-data", `{"remark":"sandboxlab","role":"tenant-admin"}`,
		"http://127.0.0.1:8090/api/tenant/users")
	return jsonField(t, raw, "key")
}

// tenantPost creates a run as the remote tenant (the server's API-key
// authn: Authorization Bearer <woca_ key> — the bridge-style login
// surface does not exist on woca-scheduler); returns status + body.
func tenantPost(t *testing.T, key string) (string, string) {
	t.Helper()
	return curlCode(t, 30,
		"-H", "Authorization: Bearer "+key,
		"-H", "Content-Type: application/json",
		"-d", `{"session_id":"s-e2e","prompt":"write the file","workspace":"/data"}`,
		"http://server:8080/api/runs")
}

// configPut/Post hit the open config console (no credentials set — the
// network is the boundary, netlab posture).
func configCall(t *testing.T, method, path, body string) (string, string) {
	t.Helper()
	args := []string{"-X", method, "-H", "Content-Type: application/json"}
	if body != "" {
		args = append(args, "-d", body)
	}
	args = append(args, "http://config:8080"+path)
	return curlCode(t, 30, args...)
}

// TestSandboxLab drives the whole chain (see the package comment for the
// phase split — the worker boots only after the config plane published
// provider settings, because its startup BuildAgent resolves the model
// from the shared settings tables).
func TestSandboxLab(t *testing.T) {
	requireDocker(t)
	requireTarballs(t, "woca-scheduler", "woca-config", "woca-worker", "woca-fakesandbox")
	ws := t.TempDir()

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("failure log dump:\n%s", logs(t, ws))
		}
		if os.Getenv("SANDBOXLAB_KEEP") == "1" {
			t.Logf("SANDBOXLAB_KEEP=1 — stack left running for troubleshooting")
			return
		}
		down(t, ws)
	})
	down(t, ws) // clean slate of any earlier run's project/volumes

	// Phase 1 — data plane + config plane + the two fakes. The worker
	// stays down until the model settings and the backend exist.
	up(t, ws, "pg", "nacos", "nacos-init", "server", "config", "fakesandbox", "fakemodel", "client")
	waitFor(t, 300*time.Second, "server never became healthy", func() (bool, string) {
		code, body := curlCode(t, 15, "http://server:8080/healthz")
		return code == "200", fmt.Sprintf("healthz = %s, body = %s", code, body)
	})
	waitFor(t, 90*time.Second, "config plane never became healthy", func() (bool, string) {
		code, body := curlCode(t, 15, "http://config:8080/api/health")
		return code == "200", fmt.Sprintf("health = %s, body = %s", code, body)
	})
	waitFor(t, 90*time.Second, "fakemodel never became healthy", func() (bool, string) {
		code, body := curlCode(t, 15, "http://fakemodel:8080/healthz")
		return code == "200", fmt.Sprintf("healthz = %s, body = %s", code, body)
	})

	// Remote tenant: mint the key on the GW console — it IS the business
	// API credential (Authorization Bearer, no login round trip).
	key := mintTenantKey(t)

	// N1 — no backend registered: the dynamic off-host gate fails closed
	// for a remote tenant (403, design §10.2 M-3), before anything about
	// workers even comes into play.
	if code, body := tenantPost(t, key); code != "403" {
		t.Fatalf("remote tenant POST /api/runs without a backend = %s (want 403), body: %s", code, body)
	}

	// Provider settings first — the worker's startup BuildAgent reads
	// them from the shared settings tables (anthropic rides the scripted
	// fakemodel container; the wire shape is pinned by fakemodel's own
	// in-process smoke test).
	code, body := configCall(t, "PUT", "/api/settings", `{
		"provider": "anthropic",
		"model": "fake-claude",
		"anthropic_base_url": "http://fakemodel:8080",
		"anthropic_api_key": "sandboxlab-model-key"
	}`)
	if code != "200" {
		t.Fatalf("settings PUT = %s, body: %s", code, body)
	}

	// Register the contract-http backend pointing at the fakesandbox
	// container, verify the full chain against it, then enable it.
	code, body = configCall(t, "POST", "/api/sandboxes", `{
		"name": "fake", "type": "contract-http",
		"endpoint": "http://fakesandbox:8080",
		"timeout_ms": 120000, "api_key": "sandboxlab-sb-key"
	}`)
	if code != "200" {
		t.Fatalf("backend registration = %s, body: %s", code, body)
	}
	backendID := jsonField(t, body, "id")
	if strings.Contains(body, "sandboxlab-sb-key") {
		t.Fatalf("registration leaks the api key: %s", body)
	}

	// Verify the full chain against the backend. A transient 404 is
	// possible while the create's pack publish settles (the row is
	// authoritative in the DB; a role applying the previous pack may
	// briefly lag) — retry until the probe is green.
	var verify struct {
		OK    bool `json:"ok"`
		Steps []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"steps"`
	}
	waitFor(t, 90*time.Second, "verify never went green", func() (bool, string) {
		code, body := configCall(t, "POST", "/api/sandboxes/"+backendID+"/verify", "")
		if code != "200" {
			return false, fmt.Sprintf("verify = %s, body = %s", code, body)
		}
		var v struct {
			OK    bool `json:"ok"`
			Steps []struct {
				Name string `json:"name"`
				OK   bool   `json:"ok"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return false, fmt.Sprintf("verify response not JSON: %v (%s)", err, body)
		}
		verify = v
		if !v.OK {
			return false, "verify not ok: " + body
		}
		return true, ""
	})
	for _, s := range verify.Steps {
		if !s.OK {
			t.Fatalf("verify step %q failed", s.Name)
		}
	}

	waitFor(t, 60*time.Second, "enable never landed", func() (bool, string) {
		code, body := configCall(t, "PUT", "/api/sandboxes/"+backendID, `{"enabled": true}`)
		return code == "200", fmt.Sprintf("enable = %s, body = %s", code, body)
	})
	code, body = configCall(t, "GET", "/api/sandboxes", "")
	if code != "200" || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("enabled backend not listed: %s %s", code, body)
	}

	// G2 — backend enabled, worker still down: run creation now passes
	// the sandbox gate but the dispatcher refuses to enqueue without a
	// live sandbox-capable worker (503). The server's own pack apply is
	// asynchronous, so 403 here just means "not yet".
	waitFor(t, 120*time.Second, "gate never flipped to the no-worker 503", func() (bool, string) {
		code, body := tenantPost(t, key)
		return code == "503", fmt.Sprintf("POST /api/runs = %s, body = %s", code, body)
	})

	// Phase 2 — the worker (settings + enabled backend are already in
	// the delivered pack; the capability heartbeat rewrites its ack row
	// every 10s, well inside the dispatcher's 15s live window).
	up(t, ws, "worker")

	// P1 — admission: retry until a live sandbox-capable worker answers.
	var runID string
	waitFor(t, 240*time.Second, "run creation never admitted", func() (bool, string) {
		code, body := tenantPost(t, key)
		if code != "200" {
			return false, fmt.Sprintf("POST /api/runs = %s, body = %s", code, body)
		}
		runID = jsonField(t, body, "run_id")
		return true, ""
	})

	// P2 — the run completes: the worker provisioned the sandbox under
	// the run's workspace identity and executed the scripted Write inside
	// it, moving no files in either direction. Follow the ledger SSE
	// replay until the terminal frame.
	var runErr string
	waitFor(t, 240*time.Second, "run never finished", func() (bool, string) {
		out, _ := execOut("client", "curl", "-sS", "-N", "-m", "8",
			"-H", "Authorization: Bearer "+key,
			"http://server:8080/api/runs/"+runID+"/events")
		if i := strings.Index(out, "event: error"); i >= 0 {
			runErr = out[i:]
			return true, ""
		}
		return strings.Contains(out, "event: done"), "no done frame yet, last: " + lastLine(out)
	})
	if runErr != "" {
		t.Fatalf("run errored:\n%s", runErr)
	}

	// The done frame only proves completion; the transfer ruling's
	// negative assertion is the artifact check: the run's product stays
	// backend-side (this lab's contract fake has no mount), so the host
	// workspace must NOT carry out.txt — nothing was uploaded or
	// downloaded. Volume-in-place products are asserted by the executor
	// backend tests and the ent demo (mount-capable executor).
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("host workspace carries out.txt (err=%v): the run moved files despite the no-transfer ruling", err)
	}

	t.Logf("sandboxlab green: tenant run %s executed through fakesandbox with zero file transfer", runID)
}

// lastLine keeps failure evidence short (SSE replays grow).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
