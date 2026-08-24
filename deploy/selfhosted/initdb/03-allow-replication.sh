#!/bin/bash
# The postgres image's default pg_hba.conf only admits regular connections
# (`host all all all`); streaming replication connects to the pseudo
# database `replication`, which needs its own entry scoped to the pg
# segment.
set -e
echo "host replication woca_repl 172.28.10.0/24 scram-sha-256" >> "$PGDATA/pg_hba.conf"
echo "woca-enterprise: pg_hba replication entry added"
# Nothing to reload on first boot (server not up yet inside initdb phase).
