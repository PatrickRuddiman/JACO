#!/usr/bin/env bash
# Role-aware Postgres entrypoint for the bench.
#
#   PG_ROLE=primary (default)  -> stock entrypoint, streaming replication on.
#   PG_ROLE=replica            -> pg_basebackup from PG_PRIMARY_HOST, then run
#                                 as a hot standby streaming from the primary.
#
# Persistence is intentionally ephemeral (no volume): a fresh replica container
# re-syncs from the primary via base backup, which also exercises the cross-node
# replication path on every start. Closed bench network only (trust auth).
set -euo pipefail

ROLE="${PG_ROLE:-primary}"

if [ "$ROLE" = "replica" ]; then
  : "${PG_PRIMARY_HOST:?PG_PRIMARY_HOST required for a replica}"
  local_user="${POSTGRES_USER:-postgres}"
  if [ ! -s "$PGDATA/PG_VERSION" ]; then
    echo "[pg] replica: waiting for primary ${PG_PRIMARY_HOST}:5432"
    until pg_isready -h "$PG_PRIMARY_HOST" -p 5432 -U "$local_user" >/dev/null 2>&1; do
      sleep 2
    done
    echo "[pg] replica: taking base backup from primary"
    rm -rf "${PGDATA:?}"/*
    chown postgres:postgres "$PGDATA"
    # -R writes standby.signal + primary_conninfo so it boots as a streaming
    # standby. trust auth on the primary means no password is needed.
    # -c fast forces an immediate checkpoint so the backup starts at once instead
    # of waiting out a spread checkpoint (which stalls badly under write load).
    gosu postgres pg_basebackup -h "$PG_PRIMARY_HOST" -p 5432 -U "$local_user" \
      -D "$PGDATA" -Fp -Xs -c fast -R -P
    # Postgres refuses to start unless PGDATA is 0700/0750 and postgres-owned;
    # pg_basebackup into a pre-existing dir leaves the dir's own mode untouched.
    chown -R postgres:postgres "$PGDATA"
    chmod 0700 "$PGDATA"
    echo "[pg] replica: base backup complete"
  else
    echo "[pg] replica: existing data dir, resuming"
  fi
  echo "[pg] replica: starting hot standby"
  exec gosu postgres postgres
fi

echo "[pg] primary: starting with streaming replication enabled"
exec docker-entrypoint.sh postgres \
  -c wal_level=replica \
  -c max_wal_senders=10 \
  -c max_replication_slots=10 \
  -c wal_keep_size=256MB \
  -c hot_standby=on
