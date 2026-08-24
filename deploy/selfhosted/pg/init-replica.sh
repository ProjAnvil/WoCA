#!/bin/sh
# woca-enterprise PostgreSQL replica bootstrap (postgres:16-alpine).
#
# On an empty PGDATA it takes a base backup from the primary with
# pg_basebackup -R (which writes standby.signal + primary_conninfo), then
# hands control to the stock docker entrypoint, which starts the server as
# a read-only hot standby. On subsequent starts PGDATA is already populated
# and the script degrades to a plain start, resuming streaming replication.
set -e

: "${WOCA_PG_PRIMARY_HOST:?WOCA_PG_PRIMARY_HOST is required}"
: "${WOCA_PG_REPL_PASSWORD:?WOCA_PG_REPL_PASSWORD is required}"
: "${PGDATA:=/var/lib/postgresql/data}"

mkdir -p "$PGDATA"
chmod 700 "$PGDATA" 2>/dev/null || true

if [ ! -s "$PGDATA/PG_VERSION" ]; then
    echo "woca-enterprise: replica PGDATA empty, taking base backup from $WOCA_PG_PRIMARY_HOST"
    until PGPASSWORD="$WOCA_PG_REPL_PASSWORD" pg_basebackup \
        -h "$WOCA_PG_PRIMARY_HOST" -p 5432 \
        -U woca_repl \
        -D "$PGDATA" \
        -Fp -Xs -P -R \
        --slot=woca_replica; do
        echo "woca-enterprise: base backup failed (primary not ready yet?), retrying in 3s"
        sleep 3
    done
    echo "woca-enterprise: base backup complete, starting as hot standby"
fi

# Keep the hot standby's replication-related settings at least as high as
# the primary's, or startup recovery aborts (and a promoted replica could
# not serve the same number of senders/slots).
exec docker-entrypoint.sh postgres \
    -c wal_level=replica \
    -c max_wal_senders=16 \
    -c max_replication_slots=16 \
    -c listen_addresses=*
