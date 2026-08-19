package agentbuild

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/settings"
)

// isolateEnv points HOME and the woca env overrides at a temp directory so
// tests never touch the developer's real ~/.woca state, and disables browser
// opening. Returns the temp home.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WOCA_DB", filepath.Join(home, "woca.db"))
	t.Setenv("WOCA_PLUGIN_ROOT", filepath.Join(home, "plugins"))
	t.Setenv("WOCA_NO_BROWSER", "1")
	return home
}

// seedSettings opens the isolated settings DB, applies key=value pairs, and
// saves. Tests use it to pin cheap defaults (e.g. memory-engine=none) before
// BuildAgent.
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

// fakeOpenAI starts an OpenAI-compatible chat-completions server that always
// replies with content, handling both plain and streaming requests. Returns
// its base URL.
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

// baseOpts returns Options wired to an isolated environment with the OpenAI
// provider pointed at url and a temp workdir.
func baseOpts(t *testing.T, url string) Options {
	t.Helper()
	return Options{
		Provider: "openai",
		Model:    "gpt-test",
		APIKey:   "test-key",
		BaseURL:  url,
		Workdir:  t.TempDir(),
	}
}

// fakeOpenBinary installs a fake `open` that fetches the URL (following
// redirects) instead of opening a browser, so OAuth flows complete in
// tests, and clears WOCA_NO_BROWSER so the open path runs.
func fakeOpenBinary(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexec /usr/bin/curl -s -o /dev/null -L \"$1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "open"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("WOCA_NO_BROWSER", "")
}
