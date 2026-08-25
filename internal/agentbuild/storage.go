package agentbuild

import (
	"context"
	"path/filepath"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lgcppostgres "github.com/projanvil/langchain-golang/langgraph/checkpoint/postgres"
	lgcpsqlite "github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite"
	"github.com/projanvil/woca/internal/cpserde"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// openStorageBackends is BuildAgent's storage phase: checkpoint
// persistence and the memoryStore, each with its own postgres connection
// assembled from per-subsystem settings (host/port/db/user rows + a
// password secret — the console never stores a full DSN). A legacy
// postgres_url secret still wins when no per-subsystem host is
// configured, so older deployments keep working unchanged. Every failure
// degrades to a warning (and a nil backend) — the harness always starts.
func openStorageBackends(ctx context.Context, opts Options, dbSettings settings.Settings, st *settings.Store, log zerolog.Logger) (lgcheckpoint.Saver, *store.Store) {
	openPostgresFor := func(subsystem, host, port, dbname, user, passwordSecret string) *wocadb.DB {
		if st == nil {
			return nil
		}
		password := st.GetSecret(passwordSecret)
		if host == "" {
			if legacy := st.GetSecret(settings.SecretPostgresURL); legacy != "" {
				db, err := wocadb.Open("pg:" + legacy)
				if err != nil {
					log.Warn().Str("subsystem", subsystem).Err(err).Msg("postgres backend unavailable (legacy DSN)")
					return nil
				}
				return db
			}
			log.Warn().Str("subsystem", subsystem).Msg("postgres backend selected but no host configured")
			return nil
		}
		dsn := settings.PostgresDSN(host, port, dbname, user, password)
		db, err := wocadb.Open("pg:" + dsn)
		if err != nil {
			log.Warn().Str("subsystem", subsystem).Err(err).Msg("postgres backend unavailable")
			return nil
		}
		return db
	}
	// (The checkpoint saver opens its own connection from the assembled
	// DSN — see CheckpointPostgresDSN — so only the memoryStore needs a
	// shared handle here.)
	var memDB *wocadb.DB
	openMemoryPG := func() *wocadb.DB {
		if memDB != nil {
			return memDB
		}
		memDB = openPostgresFor("memoryStore", dbSettings.MemoryPGHost, dbSettings.MemoryPGPort,
			dbSettings.MemoryPGDB, dbSettings.MemoryPGUser, settings.SecretMemoryPGPassword)
		return memDB
	}
	localDB := func() *wocadb.DB {
		if st == nil {
			return nil
		}
		return st.DB()
	}

	// Checkpoint persistence uses the upstream langgraph/checkpoint
	// savers (sqlite/postgres companion modules) with woca's typed serde
	// (internal/cpserde). The legacy in-repo lgcp savers are gone; the
	// "file" backend semantics became "a sqlite database inside the
	// directory" (one checkpoints.db instead of per-thread JSON files).
	var checkpointer lgcheckpoint.Saver
	serializer := cpserde.NewSerializer()
	dir := opts.CheckpointDir
	if dir == "" {
		dir = dbSettings.CheckpointDir
	}
	backend := dbSettings.CheckpointBackend
	if backend == "" {
		backend = settings.CheckpointSQLite
	}
	switch {
	case dir != "":
		if s, err := lgcpsqlite.New(filepath.Join(dir, "checkpoints.db"), serializer); err == nil {
			checkpointer = s
		} else {
			log.Warn().Err(err).Msg("checkpoint dir backend unavailable")
		}
	case backend == settings.CheckpointFile:
		log.Warn().Msg("checkpoint backend 'file' requires checkpoint-dir; falling back to sqlite")
		if s, err := lgcpsqlite.New(checkpointsDefaultPath(), serializer); err == nil {
			checkpointer = s
		} else {
			log.Warn().Err(err).Msg("checkpoint sqlite backend unavailable")
		}
	case backend == settings.CheckpointPostgres:
		if st != nil {
			if dsn := settings.CheckpointPostgresDSN(dbSettings, st.GetSecret(settings.SecretCheckpointPGPassword)); dsn != "" {
				// The saver's tables live in the lgcp schema (their canonical
				// name "checkpoints" collides with woca's own KV table in
				// public; wocadb pre-creates the schema and its RLS
				// ownership policies).
				if s, err := lgcppostgres.NewFromConnString(ctx, dsnWithLgcpSchema(dsn), serializer); err == nil {
					checkpointer = s
				} else {
					log.Warn().Err(err).Msg("checkpoint postgres backend unavailable")
				}
			}
		}
	case backend == settings.CheckpointSQLite:
		if s, err := lgcpsqlite.New(checkpointsDefaultPath(), serializer); err == nil {
			checkpointer = s
		} else {
			log.Warn().Err(err).Msg("checkpoint sqlite backend unavailable")
		}
	}

	var memory *store.Store
	switch dbSettings.MemoryBackend {
	case settings.MemoryInMem:
		memory = store.NewMemory()
	case settings.MemoryPostgres:
		if db := openMemoryPG(); db != nil {
			memory = store.NewSQL(db.DB, string(db.Dialect), db.Path)
		}
	case settings.MemorySQLite:
		if db := localDB(); db != nil {
			memory = store.NewSQL(db.DB, string(db.Dialect), db.Path)
		}
	}
	return checkpointer, memory
}
