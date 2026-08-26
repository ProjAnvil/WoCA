#!/bin/bash
# Creates the replication role used by the hot-standby replica (SQL cannot
# read environment variables, hence the shell wrapper).
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
	CREATE ROLE woca_repl WITH REPLICATION LOGIN PASSWORD '$${WOCA_PG_REPL_PASSWORD}';
	-- No dollar-quoting here: the unquoted heredoc would eat $do$ markers.
	SELECT pg_create_physical_replication_slot('woca_replica')
	WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = 'woca_replica');
EOSQL
echo "woca-enterprise: replication role woca_repl + slot woca_replica created"
