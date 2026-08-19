-- woca-enterprise PostgreSQL bootstrap (runs once on the primary's first
-- boot, before any woca service connects).
--
-- Application tables are NOT created here: woca's Go backends own their
-- schemas and create them idempotently on connect (wocadb.migrate →
-- settings / secrets / checkpoints / memory / session_state;
-- lgcp.ensureSchema → checkpoints*; store.NewSQL → memory). This file only
-- provisions objects a service must never create for itself: roles and
-- instance-level conventions.

-- Replication role for the hot-standby replica (password comes from the
-- entrypoint environment; the password is interpolated by 02-replication-role.sh).
-- (Kept in the .sh companion because SQL cannot read env vars.)

-- Instance conventions for the enterprise data plane.
ALTER DATABASE woca SET timezone TO 'UTC';
ALTER DATABASE woca SET default_transaction_isolation TO 'read committed';
