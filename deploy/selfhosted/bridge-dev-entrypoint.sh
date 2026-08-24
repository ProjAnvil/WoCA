#!/bin/sh
# Bridge dev container entrypoint. Since v1.7 the bridge issues no local
# keys: first boot enrolls via WOCA_EDGE_URL + WOCA_ENROLL_TOKEN (set by
# the compose file; the token comes from the tenant ops console), and
# users sign in with organization keys the ops console issued.
set -e

exec woca-bridge "$@"
