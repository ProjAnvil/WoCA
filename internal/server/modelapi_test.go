package server

// Tests for the model-catalog seams: an injected catalog backs the
// selection surface and the resolver authorizes (or refuses) applied
// models — deployments whose authoritative catalog lives on another
// plane (the private-cloud bridge) wire both.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

func newSeamServer(t *testing.T, catalog func(context.Context) ([]wocadb.ModelPreset, error), resolve serverResolver) *Server {
	t.Helper()
	st, err := settings.Open("sqlite:" + filepath.Join(t.TempDir(), "seam.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(Config{
		Auth:         nil,
		Static:       fstest.MapFS{},
		Logger:       zerolog.Nop(),
		WorkDir:      t.TempDir(),
		Settings:     st,
		ModelCatalog: catalog,
		ResolveModel: resolve,
	})
}

type serverResolver func(ctx context.Context, id, provider, model string) (wocadb.ModelPreset, string, bool)

func TestModelCatalogSeam(t *testing.T) {
	srv := newSeamServer(t, func(context.Context) ([]wocadb.ModelPreset, error) {
		return []wocadb.ModelPreset{{ID: "p1", Name: "anthro", Provider: "anthropic", Model: "claude-x"}}, nil
	}, nil)
	rec := httptest.NewRecorder()
	srv.handleListModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	var out []map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if rec.Code != http.StatusOK || len(out) != 1 || out[0]["model"] != "claude-x" {
		t.Fatalf("list = %d %v", rec.Code, out)
	}
}

func TestResolveModelSeamApply(t *testing.T) {
	srv := newSeamServer(t, nil, func(_ context.Context, id, _, _ string) (wocadb.ModelPreset, string, bool) {
		if id != "p1" {
			return wocadb.ModelPreset{}, "", false
		}
		return wocadb.ModelPreset{ID: "p1", Name: "anthro", Provider: "anthropic", Model: "claude-x", MaxTokens: 8192}, "sk-authorized", true
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/p1/apply", nil)
	req.SetPathValue("id", "p1")
	srv.handleApplyModel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body.String())
	}
	// The authorized credential landed in the provider's live slot; the
	// model settings reflect the preset.
	if key := srv.settings.GetSecret(settings.SecretAnthropicAPIKey); key != "sk-authorized" {
		t.Fatalf("provider key = %q", key)
	}
	if cfg := srv.settings.Load(); cfg.Provider != "anthropic" || cfg.Model != "claude-x" || cfg.MaxTokens != 8192 {
		t.Fatalf("cfg = %+v", cfg)
	}

	// Unknown model: refused before anything local is touched.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/models/other/apply", nil)
	req2.SetPathValue("id", "other")
	srv.handleApplyModel(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown apply = %d want 404", rec2.Code)
	}
}
