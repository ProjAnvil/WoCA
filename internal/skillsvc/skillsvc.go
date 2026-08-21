// Package skillsvc is the Phase 3 skill-asset platform v1 (design §6.4):
// assets with immutable versions and mutable labels (production/canary).
// Every publish creates a new version; a label points at a version;
// rollback is re-pointing the label. The store rides wocadb (sqlite for
// personal/tests, PG + tenant_id + RLS for the enterprise deployment).
package skillsvc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

// Asset is one versionable skill asset.
type Asset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Version is one immutable published version of an asset.
type Version struct {
	AssetID   string `json:"asset_id"`
	Version   int64  `json:"version"`
	Body      string `json:"body"`
	Checksum  string `json:"checksum"`
	CreatedAt int64  `json:"created_at"`
}

// ErrNotFound reports a missing asset/version/label.
var ErrNotFound = errors.New("skillsvc: not found")

// Store manages skill assets, versions, and labels on a woca database.
type Store struct {
	db      *sql.DB
	dialect wocadb.Dialect
}

// NewStore migrates the skill tables and returns a Store.
func NewStore(db *wocadb.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("skillsvc: nil database")
	}
	s := &Store{db: db.DB, dialect: db.Dialect}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ph(i int) string {
	if s.dialect == wocadb.DialectPostgres {
		return fmt.Sprintf("$%d", i+1)
	}
	return "?"
}

func (s *Store) migrate() error {
	ts := "INTEGER"
	if s.dialect == wocadb.DialectPostgres {
		ts = "BIGINT"
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS skill_assets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			created_at ` + ts + ` NOT NULL,
			updated_at ` + ts + ` NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS skill_versions (
			asset_id TEXT NOT NULL REFERENCES skill_assets(id) ON DELETE CASCADE,
			version INTEGER NOT NULL,
			body TEXT NOT NULL,
			checksum TEXT NOT NULL,
			created_at ` + ts + ` NOT NULL,
			PRIMARY KEY (asset_id, version)
		)`,
		`CREATE TABLE IF NOT EXISTS skill_labels (
			asset_id TEXT NOT NULL REFERENCES skill_assets(id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			version INTEGER NOT NULL,
			updated_at ` + ts + ` NOT NULL,
			PRIMARY KEY (asset_id, label)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("skillsvc: migrate: %w", err)
		}
	}
	// On postgres, add tenant_id + RLS like every business table.
	if s.dialect == wocadb.DialectPostgres {
		for _, table := range []string{"skill_assets", "skill_versions", "skill_labels"} {
			if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT COALESCE(NULLIF(current_setting('app.tenant_id', true), ''), 'local')`); err != nil {
				return fmt.Errorf("skillsvc: tenant column: %w", err)
			}
			if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ENABLE ROW LEVEL SECURITY`); err != nil {
				return fmt.Errorf("skillsvc: rls: %w", err)
			}
			if _, err := s.db.Exec(`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`); err != nil {
				return fmt.Errorf("skillsvc: force rls: %w", err)
			}
		}
	}
	return nil
}

func nowMilli() int64 { return time.Now().UnixMilli() }

func randID() string {
	return fmt.Sprintf("skl_%d_%d", time.Now().UnixNano(), time.Now().UnixMilli()%10000)
}

// CreateAsset registers a new skill asset.
func (s *Store) CreateAsset(ctx context.Context, name, description string) (Asset, error) {
	if strings.TrimSpace(name) == "" {
		return Asset{}, errors.New("skillsvc: name is required")
	}
	now := nowMilli()
	a := Asset{ID: randID(), Name: name, Description: description, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO skill_assets (id, name, description, created_at, updated_at) VALUES (`+
			s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`, `+s.ph(4)+`)`,
		a.ID, a.Name, a.Description, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "23505") {
			return Asset{}, fmt.Errorf("skillsvc: asset %q already exists", name)
		}
		return Asset{}, err
	}
	return a, nil
}

// GetAsset returns one asset.
func (s *Store) GetAsset(ctx context.Context, id string) (Asset, error) {
	var a Asset
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM skill_assets WHERE id = `+s.ph(0), id).
		Scan(&a.ID, &a.Name, &a.Description, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

// ListAssets returns all assets ordered by name.
func (s *Store) ListAssets(ctx context.Context) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, created_at, updated_at FROM skill_assets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAsset removes an asset and cascades its versions and labels.
func (s *Store) DeleteAsset(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM skill_assets WHERE id = `+s.ph(0), id)
	return err
}

// PublishVersion creates a new immutable version of an asset. The version
// number is assigned sequentially; identical content is idempotent.
func (s *Store) PublishVersion(ctx context.Context, assetID, body, checksum string) (Version, error) {
	if body == "" {
		return Version{}, errors.New("skillsvc: body is required")
	}
	// Idempotent: same checksum for the asset returns the existing version.
	var existing Version
	err := s.db.QueryRowContext(ctx,
		`SELECT asset_id, version, body, checksum, created_at FROM skill_versions
		 WHERE asset_id = `+s.ph(0)+` AND checksum = `+s.ph(1), assetID, checksum).
		Scan(&existing.AssetID, &existing.Version, &existing.Body, &existing.Checksum, &existing.CreatedAt)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Version{}, err
	}

	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM skill_assets WHERE id = `+s.ph(0)+`)`, assetID).Scan(&exists); err != nil || !exists {
		return Version{}, ErrNotFound
	}

	var next int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM skill_versions WHERE asset_id = `+s.ph(0), assetID).Scan(&next); err != nil {
		return Version{}, err
	}
	now := nowMilli()
	v := Version{AssetID: assetID, Version: next, Body: body, Checksum: checksum, CreatedAt: now}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO skill_versions (asset_id, version, body, checksum, created_at) VALUES (`+
			s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`, `+s.ph(4)+`)`,
		v.AssetID, v.Version, v.Body, v.Checksum, v.CreatedAt)
	if err != nil {
		return Version{}, err
	}
	return v, nil
}

// ListVersions returns an asset's versions oldest-first.
func (s *Store) ListVersions(ctx context.Context, assetID string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT asset_id, version, body, checksum, created_at FROM skill_versions
		 WHERE asset_id = `+s.ph(0)+` ORDER BY version`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.AssetID, &v.Version, &v.Body, &v.Checksum, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetLabel points a label (production/canary) at a version. Publishing and
// rollback are the same operation.
func (s *Store) SetLabel(ctx context.Context, assetID, label string, version int64) error {
	if label == "" {
		return errors.New("skillsvc: label is required")
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM skill_versions WHERE asset_id = `+s.ph(0)+` AND version = `+s.ph(1)+`)`,
		assetID, version).Scan(&exists); err != nil || !exists {
		return ErrNotFound
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO skill_labels (asset_id, label, version, updated_at) VALUES (`+
			s.ph(0)+`, `+s.ph(1)+`, `+s.ph(2)+`, `+s.ph(3)+`)
		 ON CONFLICT(asset_id, label) DO UPDATE SET version = excluded.version, updated_at = excluded.updated_at`,
		assetID, label, version, nowMilli())
	return err
}

// ResolveLabel returns the version a label currently points at.
func (s *Store) ResolveLabel(ctx context.Context, assetID, label string) (Version, error) {
	var version int64
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM skill_labels WHERE asset_id = `+s.ph(0)+` AND label = `+s.ph(1),
		assetID, label).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, err
	}
	var v Version
	err = s.db.QueryRowContext(ctx,
		`SELECT asset_id, version, body, checksum, created_at FROM skill_versions
		 WHERE asset_id = `+s.ph(0)+` AND version = `+s.ph(1), assetID, version).
		Scan(&v.AssetID, &v.Version, &v.Body, &v.Checksum, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	return v, err
}

// Labels returns every label→version mapping of an asset.
func (s *Store) Labels(ctx context.Context, assetID string) (map[string]Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT label, version FROM skill_labels WHERE asset_id = `+s.ph(0), assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Collect (label, version) pairs, then fetch each version's body.
	type lv struct {
		label   string
		version int64
	}
	var pairs []lv
	for rows.Next() {
		var p lv
		if err := rows.Scan(&p.label, &p.version); err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]Version, len(pairs))
	for _, p := range pairs {
		v, err := s.resolveVersion(ctx, assetID, p.version)
		if err != nil {
			return nil, err
		}
		out[p.label] = v
	}
	return out, nil
}

func (s *Store) resolveVersion(ctx context.Context, assetID string, version int64) (Version, error) {
	var v Version
	err := s.db.QueryRowContext(ctx,
		`SELECT asset_id, version, body, checksum, created_at FROM skill_versions
		 WHERE asset_id = `+s.ph(0)+` AND version = `+s.ph(1), assetID, version).
		Scan(&v.AssetID, &v.Version, &v.Body, &v.Checksum, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	return v, err
}
