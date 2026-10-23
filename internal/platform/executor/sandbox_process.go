// sandbox_process.go implements the session half of contract v2
// (CONTRACT.md §3.3–3.6): Start launches a long-running process via
// POST /start, and sandboxProcess drives it by polling output with an
// offset cursor while forwarding stdin writes to the input endpoint.
package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// pollInterval is the pause between output polls when the session is idle.
const pollInterval = 200 * time.Millisecond

// pollRequestTimeout bounds a single output poll HTTP call.
const pollRequestTimeout = 10 * time.Second

// pollMaxFailures is the number of consecutive poll errors tolerated
// before the process is considered lost.
const pollMaxFailures = 5

// Start launches a long-running command in the sandbox per contract v2
// (POST /sandbox/{id}/start) and returns a Process handle driven by the
// output/input/kill session endpoints. Like Local.Start, the process is
// killed when ctx is canceled; pass context.Background() to keep it alive
// beyond the request.
func (s *Sandbox) Start(ctx context.Context, req ExecRequest, opts StartOptions) (Process, error) {
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := s.do(ctx, http.MethodPost, "/sandbox/"+s.sbxID+"/start", map[string]any{
		"argv": append([]string{req.Name}, req.Args...),
		"cwd":  req.Dir,
		"env":  envSliceToMap(req.Env),
	}, &out); err != nil {
		return nil, err
	}
	if out.SessionID == "" {
		return nil, errors.New("sandbox: no session id in start response")
	}

	stdinR, stdinW := io.Pipe()
	var stdoutR io.Reader
	var stdoutW io.WriteCloser
	if opts.Stdout == nil {
		pr, pw := io.Pipe()
		stdoutR, stdoutW = pr, pw
	}
	p := &sandboxProcess{
		s:       s,
		sid:     out.SessionID,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		opts:    opts,
		exitCh:  make(chan error, 1),
		stopCh:  make(chan struct{}),
	}
	go p.pumpStdin(stdinR)
	go p.pollOutput()
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = p.Kill()
			case <-p.stopCh:
			}
		}()
	}
	return p, nil
}

// sandboxProcess is a Process backed by a contract v2 session.
type sandboxProcess struct {
	s         *Sandbox
	sid       string
	stdinW    io.WriteCloser
	stdoutR   io.Reader // nil when StartOptions.Stdout redirected output
	stdoutW   io.WriteCloser
	opts      StartOptions
	exitCh    chan error
	stopCh    chan struct{}
	closeOnce sync.Once
}

func (p *sandboxProcess) Stdin() io.WriteCloser { return p.stdinW }
func (p *sandboxProcess) Stdout() io.Reader     { return p.stdoutR }

func (p *sandboxProcess) Wait() error {
	return <-p.exitCh
}

// Kill terminates the session. The wait result of a killed process is nil
// (the caller asked for termination); a failing kill endpoint still
// finishes the process so Wait cannot hang, and the endpoint error is
// returned for the caller to ignore.
func (p *sandboxProcess) Kill() error {
	err := p.s.do(context.Background(), http.MethodPost,
		p.sessionPath("/kill"), map[string]any{}, nil)
	_ = p.stdinW.Close()
	p.finish(nil)
	return err
}

// finish reports the process outcome exactly once and stops the background
// goroutines.
func (p *sandboxProcess) finish(err error) {
	p.closeOnce.Do(func() {
		if p.stdoutW != nil {
			_ = p.stdoutW.Close()
		}
		p.exitCh <- err
		close(p.stopCh)
	})
}

// pumpStdin forwards writes to Process.Stdin to the session input endpoint
// as base64 chunks. Closing Stdin signals EOF to the backend via an
// {"eof":true} input frame (CONTRACT.md §3.5).
func (p *sandboxProcess) pumpStdin(r *io.PipeReader) {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}
		n, err := r.Read(buf)
		if n > 0 {
			if sendErr := p.sendInput(buf[:n], false); sendErr != nil {
				return
			}
		}
		if err != nil {
			_ = p.sendInput(nil, true)
			return
		}
	}
}

// sendInput posts one input frame: a data chunk (base64) or an EOF marker.
func (p *sandboxProcess) sendInput(data []byte, eof bool) error {
	body := map[string]any{"eof": eof}
	if !eof {
		body["data"] = base64.StdEncoding.EncodeToString(data)
	}
	return p.s.do(context.Background(), http.MethodPost,
		p.sessionPath("/input"), body, nil)
}

// pollOutput polls the output endpoint with an offset cursor, routing
// stdout events to the pipe (or StartOptions.Stdout) and stderr events to
// StartOptions.Stderr; on done it finishes the process with the exit code.
func (p *sandboxProcess) pollOutput() {
	offset := 0
	failures := 0
	for {
		select {
		case <-p.stopCh:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), pollRequestTimeout)
		var out struct {
			Events []struct {
				Stream string `json:"stream"`
				Data   string `json:"data"`
			} `json:"events"`
			Done       bool `json:"done"`
			ExitCode   int  `json:"exit_code"`
			NextOffset int  `json:"next_offset"`
		}
		err := p.s.do(ctx, http.MethodGet,
			p.sessionPath("/output")+"?offset="+strconv.Itoa(offset), nil, &out)
		cancel()
		if err != nil {
			failures++
			if failures > pollMaxFailures {
				p.finish(fmt.Errorf("sandbox: polling session output: %w", err))
				return
			}
			if !p.sleep() {
				return
			}
			continue
		}
		failures = 0
		for _, ev := range out.Events {
			data, decErr := base64.StdEncoding.DecodeString(ev.Data)
			if decErr != nil {
				p.finish(fmt.Errorf("sandbox: session output not base64: %w", decErr))
				return
			}
			switch ev.Stream {
			case "stdout":
				if p.opts.Stdout != nil {
					_, _ = p.opts.Stdout.Write(data)
				} else if p.stdoutW != nil {
					if _, werr := p.stdoutW.Write(data); werr != nil {
						p.finish(werr)
						return
					}
				}
			case "stderr":
				if p.opts.Stderr != nil {
					_, _ = p.opts.Stderr.Write(data)
				}
			}
		}
		if out.NextOffset > offset {
			offset = out.NextOffset
		}
		if out.Done {
			if out.ExitCode == 0 {
				p.finish(nil)
			} else {
				p.finish(&ExitError{Code: out.ExitCode})
			}
			return
		}
		if !p.sleep() {
			return
		}
	}
}

// sleep waits one poll interval; false when the process finished meanwhile.
func (p *sandboxProcess) sleep() bool {
	select {
	case <-p.stopCh:
		return false
	case <-time.After(pollInterval):
		return true
	}
}

func (p *sandboxProcess) sessionPath(suffix string) string {
	return "/sandbox/" + p.s.sbxID + "/sessions/" + p.sid + suffix
}
