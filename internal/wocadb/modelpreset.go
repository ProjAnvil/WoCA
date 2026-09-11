package wocadb

import "fmt"

// ModelPreset is one saved model configuration (design §16: the config
// plane authors named presets; there is deliberately no "active" flag —
// the consumer-side frontend picks or types the model it wants and asks
// the server to apply a preset when chosen). The preset's API key never
// lives in this row: it is stored encrypted in the secrets table under
// ModelPresetKeySecret(id) and copied into the provider slot on apply.
type ModelPreset struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	MaxTokens   int64   `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	BaseURL     string  `json:"base_url"`
	CreatedAt   int64   `json:"created_at"`
	UpdatedAt   int64   `json:"updated_at"`
}

// ModelPresetKeySecret is the secrets-table name of a preset's API key.
func ModelPresetKeySecret(id string) string { return "model_preset_key_" + id }

// SaveModelPreset upserts one preset row.
func (d *DB) SaveModelPreset(p ModelPreset) error {
	var query string
	switch d.Dialect {
	case DialectPostgres:
		query = `INSERT INTO model_presets (id, name, provider, model, max_tokens, temperature, base_url, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (id) DO UPDATE SET
				name = EXCLUDED.name, provider = EXCLUDED.provider, model = EXCLUDED.model,
				max_tokens = EXCLUDED.max_tokens, temperature = EXCLUDED.temperature,
				base_url = EXCLUDED.base_url, updated_at = EXCLUDED.updated_at`
	default:
		query = `INSERT INTO model_presets (id, name, provider, model, max_tokens, temperature, base_url, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				name = excluded.name, provider = excluded.provider, model = excluded.model,
				max_tokens = excluded.max_tokens, temperature = excluded.temperature,
				base_url = excluded.base_url, updated_at = excluded.updated_at`
	}
	_, err := d.Exec(query, p.ID, p.Name, p.Provider, p.Model, p.MaxTokens, p.Temperature, p.BaseURL, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("wocadb: save model_preset %s: %w", p.ID, err)
	}
	return nil
}

// ListModelPresets returns every preset, newest first.
func (d *DB) ListModelPresets() ([]ModelPreset, error) {
	rows, err := d.Query(`SELECT id, name, provider, model, max_tokens, temperature, base_url, created_at, updated_at FROM model_presets ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("wocadb: list model_presets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ModelPreset
	for rows.Next() {
		var p ModelPreset
		if err := rows.Scan(&p.ID, &p.Name, &p.Provider, &p.Model, &p.MaxTokens, &p.Temperature, &p.BaseURL, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteModelPreset removes one preset row.
func (d *DB) DeleteModelPreset(id string) error {
	if _, err := d.Exec(
		fmt.Sprintf(`DELETE FROM model_presets WHERE id = %s`, d.ph(0)), id,
	); err != nil {
		return fmt.Errorf("wocadb: delete model_preset %s: %w", id, err)
	}
	return nil
}
