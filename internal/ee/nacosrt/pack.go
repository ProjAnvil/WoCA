// Config-pack codec: the single artifact the config plane publishes to
// nacos (dataId woca/config.pack) and every role materializes from. The
// pack is the configuration's source of truth in flight — the role's
// local database is the materialization target, never an authority.
package nacosrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// packVersion guards the codec: a role that cannot understand a pack
// refuses it loudly instead of half-applying.
const packVersion = 2

// SkillRow / MCPRow mirror the config plane's managed tables (the
// operator-delivered skills and MCP servers that materialize at the
// lowest merge layer).
type SkillRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

type MCPRow struct {
	Name string `json:"name"`
	Spec string `json:"spec"`
}

// SandboxRow is one managed sandbox backend, carried in pack v2. The API
// key never rides the pack under this row — it travels separately in
// Pack.Secrets under "sandbox-api-key:<id>".
type SandboxRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Endpoint  string `json:"endpoint"`
	TimeoutMs int64  `json:"timeout_ms"`
	Enabled   bool   `json:"enabled"`
	// LastProbeAt is unix milliseconds, 0 = never probed.
	LastProbeAt    int64  `json:"last_probe_at"`
	LastProbeOK    bool   `json:"last_probe_ok"`
	LastProbeError string `json:"last_probe_error,omitempty"`
	// Options are the backend's execution parameters (design §17): the
	// executor reads them from the materialized row. Nil = the backend
	// carries none; pre-options rows and packs encode byte-identically.
	Options *BackendOptions `json:"options,omitempty"`
}

// Pack is the delivered configuration: settings domains (the
// settings.Settings JSON), secret values (decrypted for transport — the
// pack rides the internal registry network and re-encrypts at each
// materialization), managed skills/MCP, and the gateway policy entries.
type Pack struct {
	Version    int               `json:"version"`
	Generation string            `json:"generation"`
	Settings   settings.Settings `json:"settings"`
	Secrets    map[string]string `json:"secrets,omitempty"`
	Skills     []SkillRow        `json:"skills,omitempty"`
	MCP        []MCPRow          `json:"mcp,omitempty"`
	// Sandboxes are the managed sandbox backends (pack v2; the API keys
	// ride Secrets under "sandbox-api-key:<id>").
	Sandboxes []SandboxRow      `json:"sandboxes,omitempty"`
	Gateway   map[string]string `json:"gateway,omitempty"`
}

// ExportPack snapshots the config plane's stores into a Pack. Generation
// is the config plane's fingerprint at export time — subscribers ack it,
// so the config plane's status view stays meaningful.
func ExportPack(ctx context.Context, st *settings.Store, db *wocadb.DB) (*Pack, error) {
	p := &Pack{
		Version:    packVersion,
		Generation: st.Generation(),
		Settings:   st.Load(),
	}
	if names := st.SecretNames(); len(names) > 0 {
		p.Secrets = map[string]string{}
		for _, n := range names {
			p.Secrets[n] = st.GetSecret(n)
		}
	}
	// Managed skills & MCP: the operator-delivered set.
	rows, err := db.Query(`SELECT name, description, body FROM managed_skills ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("nacosrt: export skills: %w", err)
	}
	for rows.Next() {
		var r SkillRow
		if err := rows.Scan(&r.Name, &r.Description, &r.Body); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("nacosrt: export skills: %w", err)
		}
		p.Skills = append(p.Skills, r)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("nacosrt: export skills: %w", err)
	}
	mrows, err := db.Query(`SELECT name, spec FROM managed_mcp ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("nacosrt: export mcp: %w", err)
	}
	for mrows.Next() {
		var r MCPRow
		if err := mrows.Scan(&r.Name, &r.Spec); err != nil {
			_ = mrows.Close()
			return nil, fmt.Errorf("nacosrt: export mcp: %w", err)
		}
		p.MCP = append(p.MCP, r)
	}
	if err := mrows.Close(); err != nil {
		return nil, fmt.Errorf("nacosrt: export mcp: %w", err)
	}
	// Sandbox backends (v2): the managed inventory without keys (design
	// §10.2 M-5: secrets ride the pack's Secrets map, collected wholesale
	// by SecretNames above). Options ride the row as a decoded struct —
	// the storage TEXT column is an implementation detail of each side.
	srows, err := db.Query(`SELECT id, name, type, endpoint, timeout_ms, enabled,
		last_probe_at, last_probe_ok, last_probe_error, options FROM sandbox_backends ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("nacosrt: export sandboxes: %w", err)
	}
	{
		for srows.Next() {
			var r SandboxRow
			var enabled, probeOK sql.NullInt64
			var probeAt sql.NullInt64
			var probeErr sql.NullString
			var optsText string
			if err := srows.Scan(&r.ID, &r.Name, &r.Type, &r.Endpoint, &r.TimeoutMs,
				&enabled, &probeAt, &probeOK, &probeErr, &optsText); err != nil {
				_ = srows.Close()
				return nil, fmt.Errorf("nacosrt: export sandboxes: %w", err)
			}
			r.Enabled = enabled.Valid && enabled.Int64 != 0
			r.LastProbeAt = probeAt.Int64
			r.LastProbeOK = probeOK.Valid && probeOK.Int64 != 0
			r.LastProbeError = probeErr.String
			opts, err := DecodeOptionsText(optsText)
			if err != nil {
				_ = srows.Close()
				return nil, fmt.Errorf("nacosrt: export sandbox %s: %w", r.ID, err)
			}
			r.Options = opts
			p.Sandboxes = append(p.Sandboxes, r)
		}
		if err := srows.Close(); err != nil {
			return nil, fmt.Errorf("nacosrt: export sandboxes: %w", err)
		}
	}
	// Gateway policy (edge SPKI pin, bridge posture) rides the same pack:
	// the ops console edits it, servers materialize it.
	gw := map[string]string{}
	grows, err := db.Query(`SELECT key, value FROM gw_settings`)
	if err != nil {
		return nil, fmt.Errorf("nacosrt: export gateway: %w", err)
	}
	for grows.Next() {
		var k, v string
		if err := grows.Scan(&k, &v); err != nil {
			_ = grows.Close()
			return nil, fmt.Errorf("nacosrt: export gateway: %w", err)
		}
		gw[k] = v
	}
	if err := grows.Close(); err != nil {
		return nil, fmt.Errorf("nacosrt: export gateway: %w", err)
	}
	if len(gw) > 0 {
		p.Gateway = gw
	}
	return p, nil
}

// Publisher is the delivery seam: nacosrt.Client implements it; tests
// inject fakes. Consumers (the config plane, the ops console's gateway
// policy writes) depend on this, not on the protocol client.
type Publisher interface {
	Publish(ctx context.Context, group, dataID, content string) error
}

// PublishPack is the single producer-side step: snapshot the stores and
// deliver the pack. A failure means the save failed — no partial truth.
func PublishPack(ctx context.Context, p Publisher, st *settings.Store, db *wocadb.DB) error {
	pack, err := ExportPack(ctx, st, db)
	if err != nil {
		return fmt.Errorf("nacosrt: config export: %w", err)
	}
	content, err := pack.Encode()
	if err != nil {
		return err
	}
	return p.Publish(ctx, "DEFAULT_GROUP", ConfigDataID, content)
}

// Encode serializes the pack for publish.
func (p *Pack) Encode() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("nacosrt: pack encode: %w", err)
	}
	return string(raw), nil
}

// DecodePack parses a delivered payload, refusing any version other than
// the current one (fresh deployments only — no v1 compatibility path).
func DecodePack(content string) (*Pack, error) {
	var p Pack
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return nil, fmt.Errorf("nacosrt: pack decode: %w", err)
	}
	if p.Version != packVersion {
		return nil, fmt.Errorf("nacosrt: pack version %d not understood (want %d)", p.Version, packVersion)
	}
	return &p, nil
}
