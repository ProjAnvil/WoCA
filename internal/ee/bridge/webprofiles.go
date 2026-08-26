package bridge

// Remote-profile storage for the bridge's embedded web: the saved list of
// remote origins (other bridges or access points) the local web client can
// be pointed at. The web form's counterpart of the native app's connection
// profiles — persisted in the bridge's settings database (sqlite by
// default) so it survives restarts and browser changes; the login state
// itself stays with each origin's own session cookie.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/platform/authz"
)

// Settings keys on the bridge-local settings table.
const (
	KeyWebProfiles     = "web.remote_profiles"
	KeyWebProfilesLast = "web.remote_profiles.last"
)

// WebProfile is one saved remote origin.
type WebProfile struct {
	ID      string `json:"id"`
	URL     string `json:"url"` // origin, normalized without a trailing slash
	Label   string `json:"label,omitempty"`
	AddedAt int64  `json:"added_at"`
}

// WebProfiles is the persisted list plus the last-used pointer.
type WebProfiles struct {
	Profiles []WebProfile `json:"profiles"`
	Last     string       `json:"last,omitempty"`
}

// LoadWebProfiles reads the saved remotes (empty list when unset).
func LoadWebProfiles(kv *LocalKV) WebProfiles {
	var out WebProfiles
	if raw := kv.Get(KeyWebProfiles); raw != "" {
		_ = json.Unmarshal([]byte(raw), &out.Profiles)
	}
	out.Last = kv.Get(KeyWebProfilesLast)
	if out.Profiles == nil {
		out.Profiles = []WebProfile{}
	}
	return out
}

// saveWebProfiles persists the list and the last-used pointer.
func saveWebProfiles(kv *LocalKV, p WebProfiles) error {
	data, err := json.Marshal(p.Profiles)
	if err != nil {
		return err
	}
	if err := kv.Set(KeyWebProfiles, string(data)); err != nil {
		return err
	}
	if p.Last == "" {
		return kv.Delete(KeyWebProfilesLast)
	}
	return kv.Set(KeyWebProfilesLast, p.Last)
}

// normalizeRemoteOrigin validates and normalizes a user-entered remote
// (http/https URL → origin, no trailing slash). Empty string = invalid.
func normalizeRemoteOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/")
}

func randomProfileID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "p_" + hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return "p_" + hex.EncodeToString(b[:])
}

// WebProfileRoutes mounts the remote-profile CRUD. Remotes are user
// session-lifecycle state (the web form's connection list — the same
// class as login/logout), not server settings: reads at sessions:read,
// mutations at sessions:write, so every interactive member manages their
// own list while settings:write stays admin-only.
func WebProfileRoutes(mux *http.ServeMux, kv *LocalKV, scoped func(scope string) func(http.HandlerFunc) http.Handler) {
	read := scoped(authz.ScopeSessionsRead)
	write := scoped(authz.ScopeSessionsWrite)

	mux.Handle("GET /api/profiles", read(http.HandlerFunc(handleGetProfiles(kv))))
	mux.Handle("POST /api/profiles", write(http.HandlerFunc(handleAddProfile(kv))))
	mux.Handle("POST /api/profiles/{id}/use", write(http.HandlerFunc(handleUseProfile(kv))))
	mux.Handle("DELETE /api/profiles/{id}", write(http.HandlerFunc(handleDeleteProfile(kv))))
}

func handleGetProfiles(kv *LocalKV) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBridge(w, http.StatusOK, LoadWebProfiles(kv))
	}
}

func handleAddProfile(kv *LocalKV) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL   string `json:"url"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			writeJSONBridge(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		origin := normalizeRemoteOrigin(req.URL)
		if origin == "" {
			writeJSONBridge(w, http.StatusBadRequest, map[string]string{"error": "invalid url (want http(s)://host[:port])"})
			return
		}
		p := LoadWebProfiles(kv)
		for _, existing := range p.Profiles {
			if existing.URL == origin {
				// Idempotent add: return the existing entry instead of
				// duplicating the origin.
				writeJSONBridge(w, http.StatusOK, existing)
				return
			}
		}
		profile := WebProfile{ID: randomProfileID(), URL: origin, Label: strings.TrimSpace(req.Label), AddedAt: time.Now().UnixMilli()}
		p.Profiles = append(p.Profiles, profile)
		if err := saveWebProfiles(kv, p); err != nil {
			writeJSONBridge(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
		writeJSONBridge(w, http.StatusOK, profile)
	}
}

func handleUseProfile(kv *LocalKV) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p := LoadWebProfiles(kv)
		for _, existing := range p.Profiles {
			if existing.ID == id {
				p.Last = id
				if err := saveWebProfiles(kv, p); err != nil {
					writeJSONBridge(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
					return
				}
				writeJSONBridge(w, http.StatusOK, existing)
				return
			}
		}
		writeJSONBridge(w, http.StatusNotFound, map[string]string{"error": "unknown profile"})
	}
}

func handleDeleteProfile(kv *LocalKV) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p := LoadWebProfiles(kv)
		kept := p.Profiles[:0]
		found := false
		for _, existing := range p.Profiles {
			if existing.ID == id {
				found = true
				continue
			}
			kept = append(kept, existing)
		}
		if !found {
			writeJSONBridge(w, http.StatusNotFound, map[string]string{"error": "unknown profile"})
			return
		}
		p.Profiles = kept
		if p.Last == id {
			p.Last = ""
		}
		if err := saveWebProfiles(kv, p); err != nil {
			writeJSONBridge(w, http.StatusInternalServerError, map[string]string{"error": "persist failed"})
			return
		}
		writeJSONBridge(w, http.StatusOK, LoadWebProfiles(kv))
	}
}
