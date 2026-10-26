// The skill-asset platform's HTTP API (the surface the embedded console
// SPA in ee/skillsvc-web is built against). Mounted on woca-scheduler's
// management listener inside the ops console's credential gate — the
// console session is the credential, per-route scope checks apply.
package skillsvc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/projanvil/woca/internal/platform/httpx"
)

// NewHandler returns the platform's API surface over the store: assets,
// their immutable versions, and the mutable labels pointing at them.
// Routes are rooted at /api/skills — the contract the console SPA calls.
func NewHandler(st *Store) http.Handler {
	h := &apiHandler{st: st}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/skills", h.listAssets)
	mux.HandleFunc("POST /api/skills", h.createAsset)
	mux.HandleFunc("DELETE /api/skills/{id}", h.deleteAsset)
	mux.HandleFunc("GET /api/skills/{id}/versions", h.listVersions)
	mux.HandleFunc("POST /api/skills/{id}/versions", h.publishVersion)
	mux.HandleFunc("GET /api/skills/{id}/labels", h.labels)
	mux.HandleFunc("PUT /api/skills/{id}/labels/{label}", h.setLabel)
	return mux
}

type apiHandler struct{ st *Store }

func (h *apiHandler) listAssets(w http.ResponseWriter, r *http.Request) {
	assets, err := h.st.ListAssets(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if assets == nil {
		assets = []Asset{}
	}
	httpx.WriteJSON(w, http.StatusOK, assets)
}

func (h *apiHandler) createAsset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	a, err := h.st.CreateAsset(r.Context(), req.Name, req.Description)
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (h *apiHandler) deleteAsset(w http.ResponseWriter, r *http.Request) {
	if err := h.st.DeleteAsset(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *apiHandler) listVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := h.st.ListVersions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if versions == nil {
		versions = []Version{}
	}
	httpx.WriteJSON(w, http.StatusOK, versions)
}

func (h *apiHandler) publishVersion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	// The checksum is the content's identity: the server derives it, so the
	// idempotent-publish contract (same content, same version) holds
	// regardless of the caller.
	sum := sha256.Sum256([]byte(req.Body))
	v, err := h.st.PublishVersion(r.Context(), r.PathValue("id"), req.Body, hex.EncodeToString(sum[:]))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (h *apiHandler) labels(w http.ResponseWriter, r *http.Request) {
	labels, err := h.st.Labels(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, labels)
}

func (h *apiHandler) setLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if err := h.st.SetLabel(r.Context(), r.PathValue("id"), r.PathValue("label"), req.Version); err != nil {
		writeError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
