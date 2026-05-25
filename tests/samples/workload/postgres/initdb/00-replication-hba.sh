#!/usr/bin/env bash
# Runs once, during the primary's initdb. POSTGRES_HOST_AUTH_METHOD=trust adds
# the regular `host all all all trust` line; replication uses a separate pg_hba
# keyword, so add it explicitly. Closed bench network only.
set -e
{
  echo "# bench: allow streaming replication from any cluster node (trust)"
  echo "host replication all all trust"
} >> "$PGDATA/pg_hba.conf"
