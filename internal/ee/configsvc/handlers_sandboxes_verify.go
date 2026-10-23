// Verify: the full-chain liveness probe (design §4.3). Each registered
// backend can be probed end-to-end: provision a throwaway sandbox, run
// every executor surface once, record the outcome on the row, and destroy
// the sandbox. A failing step aborts the run; the probe's response never
// contains the stored key.
package configsvc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/providers"
	"github.com/projanvil/woca/internal/wocadb"
)

// verifyStepMax bounds each probe step; the row's timeout can only lower
// it further.
const verifyStepMax = 30 * time.Second

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

	// Step 1 — provision: providers.New covers the create step (the
	// constructor creates the sandbox); type dispatch happens inside.
	x, err := providers.New(providers.Config{
		Type:     row.Type,
		Endpoint: row.Endpoint,
		APIKey:   s.settings.GetSecret(sandboxKeySecret(id)),
		Timeout:  time.Duration(row.TimeoutMs) * time.Millisecond,
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
		for _, m := range matches {
			if m == "/workspace/.woca-verify" {
				return nil
			}
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
			// The kill endpoint error is advisory; the session is torn
			// down either way.
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
