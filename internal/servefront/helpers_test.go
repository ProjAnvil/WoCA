package servefront

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/settings"
)

// TestMain swaps in a no-op browser opener for the whole package, so no test
// can launch the real browser, whichever path it takes. Tests that need the
// opener opt in with stubBrowserOpen.
func TestMain(m *testing.M) {
	agentbuild.OpenBrowser = func(string) {}
	os.Exit(m.Run())
}

// isolateEnv points HOME and the woca env overrides at a temp directory so
// tests never touch the developer's real ~/.woca state. Browser opening is
// already inert: TestMain replaces OpenBrowser with a no-op. Returns the temp
// home.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WOCA_DB", filepath.Join(home, "woca.db"))
	t.Setenv("WOCA_PLUGIN_ROOT", filepath.Join(home, "plugins"))
	return home
}

// seedSettings opens the isolated settings DB, applies key=value pairs, and
// saves.
func seedSettings(t *testing.T, pairs ...[2]string) {
	t.Helper()
	st, err := settings.Open(os.Getenv("WOCA_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := st.Load()
	for _, p := range pairs {
		if err := settings.Apply(&cfg, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

func fakeOpenAI(t *testing.T, content string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeChunk := func(delta map[string]any, finish any) {
			chunk := map[string]any{
				"id":     "chatcmpl-1",
				"object": "chat.completion.chunk",
				"choices": []map[string]any{{
					"index":         0,
					"delta":         delta,
					"finish_reason": finish,
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		writeChunk(map[string]any{"role": "assistant", "content": content}, nil)
		writeChunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// baseOpts returns agentbuild.Options wired to an isolated environment with
// the OpenAI provider pointed at url and a temp workdir.
func baseOpts(t *testing.T, url string) agentbuild.Options {
	t.Helper()
	return agentbuild.Options{
		Provider: "openai",
		Model:    "gpt-test",
		APIKey:   "test-key",
		BaseURL:  url,
		Workdir:  t.TempDir(),
	}
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// stubBrowserOpen swaps agentbuild.OpenBrowser for the duration of t so no
// browser is launched. It records every URL and, when fetch is true, issues a
// GET so flows that need the URL visited still work.
func stubBrowserOpen(t *testing.T, fetch bool) *[]string {
	t.Helper()
	var got []string
	old := agentbuild.OpenBrowser
	agentbuild.OpenBrowser = func(url string) {
		got = append(got, url)
		if !fetch {
			return
		}
		resp, err := http.Get(url)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	t.Cleanup(func() { agentbuild.OpenBrowser = old })
	return &got
}
