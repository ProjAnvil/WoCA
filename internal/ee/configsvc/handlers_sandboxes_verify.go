// Verify: the full-chain liveness probe (design §4.3). Each registered
// backend can be probed end-to-end: provision a throwaway sandbox, run
// every executor surface once, record the outcome on the row, and destroy
// the sandbox. A failing step aborts the run; the probe's response never
// contains the stored key.
//
// contract-http backends are probed through their routing proxy (spec
// §3.2): the endpoint of such a row IS the executor-proxy, and a probe
// that bypassed it would verify a path tenants never use. The probe first
// binds its stable workspace (source key verify:<backend-id>) via POST
// {endpoint}/internal/bindings, then runs the whole contract path —
// create→exec→…→destroy — workspace-routed exactly like a tenant session.
// The binding is deliberately never released: repeated probes of one
// backend reuse one workspace instead of accumulating garbage.
package configsvc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/providers"
	"github.com/projanvil/woca/internal/wocadb"
)

// verifyStepMax bounds each probe step; the row's timeout can only lower
// it further.
const verifyStepMax = 30 * time.Second

// verifyBindClient / verifyBindBodyLimit bound the allocate-or-bind call.
// Generic errors only (the endpoint's URL never crosses to the console
// response); the proxy's own refusal reasons pass through.
var (
	verifyBindClient   = &http.Client{Timeout: 10 * time.Second}
	errVerifyBindUnrep = errors.New("verify workspace binding unavailable")
	errVerifyBindBad   = errors.New("verify workspace binding response malformed")
)

const verifyBindBodyLimit = 4 << 10

// verifySourceKey is the probe's allocate-or-bind key: one stable
// workspace per backend row.
func verifySourceKey(backendID string) string { return "verify:" + backendID }

// verifyWorkspace binds (or rebinds) the probe's workspace through the
// backend's routing proxy. Fail-closed: a probe that cannot obtain a
// workspace does not fall back to an unrouted direct connection — the
// endpoint is the proxy, and an unrouted create would be refused there
// anyway.
func (s *Service) verifyWorkspace(ctx context.Context, row sandboxPayload) (string, error) {
	body, err := json.Marshal(map[string]string{"tenant_id": "console", "source_key": verifySourceKey(row.ID)})
	if err != nil {
		return "", errVerifyBindBad
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, row.Endpoint+"/internal/bindings", bytes.NewReader(body))
	if err != nil {
		s.log.Warn().Err(err).Str("backend_id", row.ID).Msg("sandbox verification: binding request build failed")
		return "", errVerifyBindUnrep
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := verifyBindClient.Do(req)
	if err != nil {
		s.log.Warn().Err(err).Str("backend_id", row.ID).Msg("sandbox verification: binding call failed")
		return "", errVerifyBindUnrep
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var errOut struct {
			Error string `json:"error"`
		}
		reason := ""
		if json.NewDecoder(io.LimitReader(resp.Body, verifyBindBodyLimit)).Decode(&errOut) == nil && errOut.Error != "" {
			reason = errOut.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("status %d", resp.StatusCode)
		}
		s.log.Warn().Str("backend_id", row.ID).Str("reason", reason).Int("status", resp.StatusCode).
			Msg("sandbox verification: workspace binding refused")
		return "", fmt.Errorf("verify workspace binding refused: %s", reason)
	}
	var out struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, verifyBindBodyLimit)).Decode(&out); err != nil || out.WorkspaceID == "" {
		s.log.Warn().Err(err).Str("backend_id", row.ID).Msg("sandbox verification: binding response undecodable")
		return "", errVerifyBindBad
	}
	return out.WorkspaceID, nil
}

// archiver is the optional archive surface of the contract family
// (*executor.Sandbox and the cfbridge provider implement it); providers
// without it simply skip the archive step.
type archiver interface {
	PutArchive(ctx context.Context, r io.Reader) error
	GetArchive(ctx context.Context) (io.ReadCloser, error)
}

type verifyStep struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

func (s *Service) handleVerifySandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, ok, err := s.getSandbox(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown sandbox backend")
		return
	}
	start := time.Now()
	steps := []verifyStep{}

	// Step 1 — routing identity + provision. contract-http endpoints are
	// the executor-proxy: bind the probe's stable verify workspace first,
	// then construct the provider workspace-routed so the whole probe
	// rides the same path a tenant session does. Every other type keeps
	// the direct construction (cfbridge's endpoint is not
	// workspace-routed; the not-yet-registered enums fail in New below).
	workspace := ""
	if row.Type == "contract-http" && row.Endpoint != "" {
		ws, err := s.verifyWorkspace(r.Context(), row)
		if err != nil {
			steps = append(steps, verifyStep{Name: "create", Error: err.Error()})
			s.finishVerify(w, r, id, row, false, err.Error(), start, steps)
			return
		}
		workspace = ws
	}
	// providers.New covers the create step (the constructor creates the
	// sandbox); type dispatch happens inside.
	x, err := providers.New(providers.Config{
		Type:      row.Type,
		Endpoint:  row.Endpoint,
		APIKey:    s.settings.GetSecret(sandboxKeySecret(id)),
		Timeout:   time.Duration(row.TimeoutMs) * time.Millisecond,
		Workspace: workspace,
	})
	if err != nil {
		steps = append(steps, verifyStep{Name: "create", Error: err.Error()})
		s.finishVerify(w, r, id, row, false, err.Error(), start, steps)
		return
	}
	steps = append(steps, verifyStep{Name: "create", OK: true, LatencyMs: time.Since(start).Milliseconds()})
	// Best-effort teardown: contract-family executors destroy their
	// sandbox on Close; providers without Close are simply left alone
	// (their backends own lifecycle/TTL).
	defer func() {
		if c, ok := x.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}()

	// The per-step deadline is the row's own timeout capped at 30s.
	stepBudget := time.Duration(row.TimeoutMs) * time.Millisecond
	if stepBudget <= 0 || stepBudget > verifyStepMax {
		stepBudget = verifyStepMax
	}
	// runStep runs one probe step under its own deadline; a failure
	// aborts the remaining steps (fail-fast per design §4.3).
	runStep := func(name string, fn func(ctx context.Context) error) bool {
		st := verifyStep{Name: name}
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), stepBudget)
		defer cancel()
		if err := fn(ctx); err != nil {
			st.Error = err.Error()
		} else {
			st.OK = true
		}
		st.LatencyMs = time.Since(t0).Milliseconds()
		steps = append(steps, st)
		return st.OK
	}

	// Step 2 — exec: run a trivial command and check its output.
	if !runStep("exec", func(ctx context.Context) error {
		res, err := x.Exec(ctx, executor.ExecRequest{Name: "echo", Args: []string{"woca-verify"}})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("echo exited %d", res.ExitCode)
		}
		if !bytes.Contains(res.Stdout, []byte("woca-verify")) {
			return fmt.Errorf("echo output mismatch: %q", string(res.Stdout))
		}
		return nil
	}) {
		s.finishVerify(w, r, id, row, false, lastError(steps), start, steps)
		return
	}

	// Step 3 — file write/read round-trip.
	if !runStep("file", func(ctx context.Context) error {
		const p = "/workspace/.woca-verify"
		if err := x.WriteFile(ctx, p, []byte("woca"), 0o644); err != nil {
			return err
		}
		got, err := x.ReadFile(ctx, p)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, []byte("woca")) {
			return fmt.Errorf("read back %q", string(got))
		}
		return nil
	}) {
		s.finishVerify(w, r, id, row, false, lastError(steps), start, steps)
		return
	}

	// Step 4 — glob finds the file just written.
	if !runStep("glob", func(ctx context.Context) error {
		matches, err := x.Glob(ctx, "/workspace/.woca-*")
		if err != nil {
			return err
		}
		if slices.Contains(matches, "/workspace/.woca-verify") {
			return nil
		}
		return fmt.Errorf("glob missed the probe file: %v", matches)
	}) {
		s.finishVerify(w, r, id, row, false, lastError(steps), start, steps)
		return
	}

	// Step 5 — stat the probe file.
	if !runStep("stat", func(ctx context.Context) error {
		fi, err := x.Stat(ctx, "/workspace/.woca-verify")
		if err != nil {
			return err
		}
		if fi.IsDir() || fi.Size() != 4 {
			return fmt.Errorf("stat: dir=%v size=%d", fi.IsDir(), fi.Size())
		}
		return nil
	}) {
		s.finishVerify(w, r, id, row, false, lastError(steps), start, steps)
		return
	}

	// Step 6 — long process lifecycle: start, read output, kill.
	if !runStep("process", func(ctx context.Context) error {
		p, err := x.Start(ctx, executor.ExecRequest{
			Name: "sh", Args: []string{"-c", "echo woca-start; sleep 30"},
		}, executor.StartOptions{})
		if err != nil {
			return err
		}
		read := make(chan []byte, 1)
		go func() {
			buf := make([]byte, 64)
			n, _ := p.Stdout().Read(buf)
			read <- buf[:n]
		}()
		var out []byte
		select {
		case out = <-read:
		case <-time.After(3 * time.Second):
			_ = p.Kill()
			return fmt.Errorf("no process output within 3s")
		}
		if !bytes.Contains(out, []byte("woca-start")) {
			_ = p.Kill()
			return fmt.Errorf("process output %q", string(out))
		}
		if err := p.Kill(); err != nil {
			s.log.Warn().Err(err).Str("backend_id", id).Msg("sandbox verification: process kill failed")
		}
		return p.Wait()
	}) {
		s.finishVerify(w, r, id, row, false, lastError(steps), start, steps)
		return
	}

	// Step 7 — archive round-trip, only for providers carrying the
	// archive surface (the contract family does).
	if arch, ok := x.(archiver); ok {
		if !runStep("archive", func(ctx context.Context) error {
			var up bytes.Buffer
			zw := gzip.NewWriter(&up)
			tw := tar.NewWriter(zw)
			if err := tw.WriteHeader(&tar.Header{Name: "verify.txt", Mode: 0o644, Size: 4}); err != nil {
				return err
			}
			if _, err := tw.Write([]byte("woca")); err != nil {
				return err
			}
			_ = tw.Close()
			_ = zw.Close()
			if err := arch.PutArchive(ctx, bytes.NewReader(up.Bytes())); err != nil {
				return err
			}
			rc, err := arch.GetArchive(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = rc.Close() }()
			gr, err := gzip.NewReader(rc)
			if err != nil {
				return err
			}
			tr := tar.NewReader(gr)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				if hdr.Name == "verify.txt" {
					body, err := io.ReadAll(tr)
					if err != nil {
						return err
					}
					if !bytes.Equal(body, []byte("woca")) {
						return fmt.Errorf("archive body %q", string(body))
					}
					return nil
				}
			}
			return fmt.Errorf("verify.txt lost in archive round-trip")
		}) {
			s.finishVerify(w, r, id, row, false, lastError(steps), start, steps)
			return
		}
	}

	s.finishVerify(w, r, id, row, true, "", start, steps)
}

// lastError returns the failing step's error text ("" when none).
func lastError(steps []verifyStep) string {
	for _, st := range steps {
		if !st.OK {
			return st.Error
		}
	}
	return ""
}

// finishVerify persists the probe result on the row, logs, and answers.
func (s *Service) finishVerify(w http.ResponseWriter, r *http.Request, id string, row sandboxPayload, ok bool, errMsg string, start time.Time, steps []verifyStep) {
	total := time.Since(start).Milliseconds()
	// Persist the probe outcome (probe status is badge-only, design
	// §10.3 m-2 — it never gates RunsOffHost by itself).
	var okCol int64
	if ok {
		okCol = 1
	}
	if _, err := s.db.Exec(`UPDATE sandbox_backends SET last_probe_at = `+phI(s.db, 0)+
		`, last_probe_ok = `+phI(s.db, 1)+`, last_probe_error = `+phI(s.db, 2)+
		`, updated_at = `+phI(s.db, 3)+` WHERE id = `+phI(s.db, 4),
		wocadb.Now(), okCol, errMsg, wocadb.Now(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	s.sandboxLog("sandbox.verify", id, row.Type).Bool("ok", ok).Int64("latency_ms", total).Msg("sandbox backend verified")
	// The probe rides the pack (design §10.2 M-6): workers see the latest
	// health column without a second channel.
	if err := s.notifyConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               ok,
		"total_latency_ms": total,
		"steps":            steps,
	})
}
