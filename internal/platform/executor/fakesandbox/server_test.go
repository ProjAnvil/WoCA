// Package fakesandbox_test exercises the in-memory contract-v2 fake end
// to end at the HTTP level: create → exec → file surface → archive.
package fakesandbox_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func unb64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64 %q: %v", s, err)
	}
	return string(b)
}

type client struct {
	t    *testing.T
	base string
}

func (c *client) do(method, p string, body io.Reader, ct string) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+p, body)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("X-Woca-Sandbox-Contract", "2")
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (c *client) json(method, p string, body any) (int, map[string]any) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	code, data := c.do(method, p, rdr, "application/json")
	var out map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return code, out
}

func newClient(t *testing.T) (*client, string) {
	t.Helper()
	addr := fakesandbox.Addr(t)
	return &client{t: t, base: addr}, ""
}

func createSandbox(t *testing.T, c *client) string {
	t.Helper()
	code, out := c.json("POST", "/sandbox/create", map[string]string{"cwd": "/workspace"})
	if code != 200 {
		t.Fatalf("create = %d %v", code, out)
	}
	id, _ := out["sandbox_id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", out)
	}
	return id
}

func TestCreateExecDestroy(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)

	code, out := c.json("POST", "/sandbox/"+id+"/exec", map[string]any{
		"argv":       []string{"echo", "hi"},
		"cwd":        "/workspace",
		"env":        map[string]string{},
		"stdin":      "",
		"timeout_ms": 30000,
	})
	if code != 200 {
		t.Fatalf("exec = %d", code)
	}
	if out["exit_code"] != float64(0) {
		t.Fatalf("exit_code = %v", out["exit_code"])
	}
	if unb64(t, out["stdout"].(string)) != "hi\n" {
		t.Fatalf("stdout = %q", out["stdout"])
	}

	// destroy, then exec must 404
	if code, _ := c.json("DELETE", "/sandbox/"+id, nil); code >= 300 {
		t.Fatalf("destroy = %d", code)
	}
	if code, body := c.do("POST", "/sandbox/"+id+"/exec", strings.NewReader("{}"), "application/json"); code != 404 {
		t.Fatalf("exec after destroy = %d %s", code, body)
	}
}

func TestExecEnvStdinExitCode(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)

	code, out := c.json("POST", "/sandbox/"+id+"/exec", map[string]any{
		"argv":  []string{"sh", "-c", "echo $FAKEVAR; cat"},
		"env":   map[string]string{"FAKEVAR": "woca"},
		"stdin": b64("piped\n"),
	})
	if code != 200 {
		t.Fatalf("exec = %d", code)
	}
	if got := unb64(t, out["stdout"].(string)); got != "woca\npiped\n" {
		t.Fatalf("stdout = %q", got)
	}

	code, out = c.json("POST", "/sandbox/"+id+"/exec", map[string]any{
		"argv": []string{"sh", "-c", "exit 4"},
	})
	if code != 200 || out["exit_code"] != float64(4) {
		t.Fatalf("exit = %d %v (non-zero exit is a normal result)", code, out["exit_code"])
	}
}

func TestFileSurface(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)
	base := "/sandbox/" + id

	// write
	if code, _ := c.json("POST", base+"/write", map[string]any{
		"path": "/workspace/a.go", "content": b64("line1\nTODO fix\n"), "mode": 0o644,
	}); code >= 300 {
		t.Fatalf("write = %d", code)
	}
	if code, _ := c.json("POST", base+"/write", map[string]any{
		"path": "/workspace/.hidden/h.go", "content": b64("TODO leak\n"), "mode": 0o644,
	}); code >= 300 {
		t.Fatalf("write hidden = %d", code)
	}

	// read
	code, out := c.json("GET", base+"/read?path=%2Fworkspace%2Fa.go", nil)
	if code != 200 || unb64(t, out["content"].(string)) != "line1\nTODO fix\n" {
		t.Fatalf("read = %d %v", code, out)
	}
	if code, _ := c.json("GET", base+"/read?path=%2Fworkspace%2Fmissing", nil); code != 404 {
		t.Fatal("read missing did not 404")
	}

	// mkdir + stat dir
	if code, _ := c.json("POST", base+"/mkdir", map[string]any{"path": "/workspace/pkg/sub"}); code >= 300 {
		t.Fatalf("mkdir = %d", code)
	}
	code, out = c.json("GET", base+"/stat?path=%2Fworkspace%2Fpkg%2Fsub", nil)
	if code != 200 || out["is_dir"] != true {
		t.Fatalf("stat dir = %d %v", code, out)
	}

	// stat file: mode and size round-trip
	code, out = c.json("GET", base+"/stat?path=%2Fworkspace%2Fa.go", nil)
	if code != 200 || out["mode"] != float64(0o644) || out["size"] != float64(len("line1\nTODO fix\n")) || out["is_dir"] != false {
		t.Fatalf("stat file = %d %v", code, out)
	}

	// glob
	code, out = c.json("POST", base+"/glob", map[string]any{"pattern": "/workspace/*.go"})
	if code != 200 {
		t.Fatalf("glob = %d", code)
	}
	matches, _ := out["matches"].([]any)
	if len(matches) != 1 || matches[0] != "/workspace/a.go" {
		t.Fatalf("glob matches = %v (hidden .go must not match a plain /workspace/*.go pattern anyway; a.go must)", matches)
	}

	// grep: finds a.go, skips hidden dirs
	code, out = c.json("POST", base+"/grep", map[string]any{"root": "/workspace", "pattern": "TODO", "max_matches": 200})
	if code != 200 {
		t.Fatalf("grep = %d", code)
	}
	gm, _ := out["matches"].([]any)
	if len(gm) != 1 {
		t.Fatalf("grep matches = %v, want only a.go (hidden dir skipped)", gm)
	}
	m := gm[0].(map[string]any)
	if m["path"] != "/workspace/a.go" || m["line"] != float64(2) || unb64(t, m["text"].(string)) != "TODO fix" {
		t.Fatalf("grep match = %v", m)
	}

	// unknown sandbox 404s with an error body
	code, body := c.do("GET", "/sandbox/nope/read?path=x", nil, "")
	if code != 404 || !bytes.Contains(body, []byte("error")) {
		t.Fatalf("unknown sandbox = %d %s", code, body)
	}
}

func TestSessionLifecycle(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)
	base := "/sandbox/" + id

	code, out := c.json("POST", base+"/start", map[string]any{
		"argv": []string{"cat"}, "cwd": "/workspace", "env": map[string]string{},
	})
	if code != 200 {
		t.Fatalf("start = %d %v", code, out)
	}
	sid := out["session_id"].(string)

	if code, _ := c.json("POST", base+"/sessions/"+sid+"/input", map[string]any{"data": b64("hello ")}); code >= 300 {
		t.Fatalf("input = %d", code)
	}
	if code, _ := c.json("POST", base+"/sessions/"+sid+"/input", map[string]any{"data": b64("world")}); code >= 300 {
		t.Fatalf("input2 = %d", code)
	}
	if code, _ := c.json("POST", base+"/sessions/"+sid+"/input", map[string]any{"eof": true}); code >= 300 {
		t.Fatalf("eof = %d", code)
	}

	// poll until done, following the offset cursor so chunks counted on an
	// earlier (not-yet-done) poll are never double-counted.
	var got string
	var done bool
	var exitCode float64
	offset := 0
	for i := 0; i < 100 && !done; i++ {
		code, out = c.json("GET", base+"/sessions/"+sid+"/output?offset="+strconv.Itoa(offset), nil)
		if code != 200 {
			t.Fatalf("output = %d", code)
		}
		done, _ = out["done"].(bool)
		exitCode, _ = out["exit_code"].(float64)
		for _, ev := range out["events"].([]any) {
			e := ev.(map[string]any)
			if e["stream"] == "stdout" {
				got += unb64(t, e["data"].(string))
			}
		}
		if next, ok := out["next_offset"].(float64); ok {
			offset = int(next)
		}
		if !done {
			timeSleep()
		}
	}
	if !done || exitCode != 0 {
		t.Fatalf("session done=%v exit=%v", done, exitCode)
	}
	if got != "hello world" {
		t.Fatalf("polled stdout = %q", got)
	}
}

func TestSessionKill(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)
	base := "/sandbox/" + id

	_, out := c.json("POST", base+"/start", map[string]any{"argv": []string{"sh", "-c", "sleep 60"}})
	sid := out["session_id"].(string)
	if code, _ := c.json("POST", base+"/sessions/"+sid+"/kill", map[string]any{}); code >= 300 {
		t.Fatalf("kill = %d", code)
	}
	done := false
	for i := 0; i < 100 && !done; i++ {
		_, out = c.json("GET", base+"/sessions/"+sid+"/output?offset=0", nil)
		done, _ = out["done"].(bool)
		if !done {
			timeSleep()
		}
	}
	if !done {
		t.Fatal("killed session never reported done")
	}
}

func timeSleep() { time.Sleep(20 * time.Millisecond) }

func TestArchiveRoundTrip(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)
	base := "/sandbox/" + id

	// Build a tar.gz: one file (0755), one dir, one symlink.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	writeHeader := func(name string, mode int64, typ byte, size int64, link string) {
		hdr := &tar.Header{Name: name, Mode: mode, Typeflag: typ, Size: size, Linkname: link}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	writeHeader("run.sh", 0o755, tar.TypeReg, int64(len("#!/sh\n")), "")
	if _, err := tw.Write([]byte("#!/sh\n")); err != nil {
		t.Fatal(err)
	}
	writeHeader("pkg", 0o755, tar.TypeDir, 0, "")
	writeHeader("pkg/data.bin", 0o600, tar.TypeReg, 3, "")
	if _, err := tw.Write([]byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	writeHeader("cur", 0o777, tar.TypeSymlink, 0, "run.sh")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if code, _ := c.do("PUT", base+"/archive", bytes.NewReader(buf.Bytes()), "application/gzip"); code >= 300 {
		t.Fatalf("put archive = %d", code)
	}

	// File landed under /workspace.
	code, out := c.json("GET", base+"/read?path=%2Fworkspace%2Frun.sh", nil)
	if code != 200 || unb64(t, out["content"].(string)) != "#!/sh\n" {
		t.Fatalf("read after archive = %d %v", code, out)
	}
	code, out = c.json("GET", base+"/stat?path=%2Fworkspace%2Frun.sh", nil)
	if code != 200 || out["mode"] != float64(0o755) {
		t.Fatalf("stat mode after archive = %d %v", code, out)
	}

	// GET the archive back and verify entries.
	code, data := c.do("GET", base+"/archive", nil, "")
	if code != 200 {
		t.Fatalf("get archive = %d", code)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	entries := map[string]*tar.Header{}
	contents := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = hdr
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			contents[hdr.Name] = string(b)
		}
	}
	if entries["run.sh"] == nil || entries["run.sh"].Mode != 0o755 || contents["run.sh"] != "#!/sh\n" {
		t.Fatalf("run.sh entry = %+v", entries["run.sh"])
	}
	if entries["pkg/data.bin"] == nil || entries["pkg/data.bin"].Mode != 0o600 || contents["pkg/data.bin"] != "\x00\x01\x02" {
		t.Fatalf("pkg/data.bin entry = %+v", entries["pkg/data.bin"])
	}
	if e := entries["cur"]; e == nil || e.Typeflag != tar.TypeSymlink || e.Linkname != "run.sh" {
		t.Fatalf("symlink entry = %+v", e)
	}
}

func TestGrepMaxMatches(t *testing.T) {
	c, _ := newClient(t)
	id := createSandbox(t, c)
	base := "/sandbox/" + id
	for i := 0; i < 5; i++ {
		if code, _ := c.json("POST", base+"/write", map[string]any{
			"path": fmt.Sprintf("/workspace/f%d.txt", i), "content": b64("needle\n"), "mode": 0o644,
		}); code >= 300 {
			t.Fatalf("write = %d", code)
		}
	}
	_, out := c.json("POST", base+"/grep", map[string]any{"root": "/workspace", "pattern": "needle", "max_matches": 2})
	gm, _ := out["matches"].([]any)
	if len(gm) != 2 {
		t.Fatalf("max_matches=2 returned %d matches", len(gm))
	}
}
