# One image, two roles (PG_ROLE=primary|replica). The wrapper entrypoint either
# delegates to the stock postgres entrypoint (primary, streaming enabled) or
# takes a pg_basebackup from the primary and starts a hot standby (replica).
FROM postgres:16
COPY docker-entrypoint-wrapper.sh /usr/local/bin/docker-entrypoint-wrapper.sh
COPY initdb/00-replication-hba.sh /docker-entrypoint-initdb.d/00-replication-hba.sh
RUN chmod +x /usr/local/bin/docker-entrypoint-wrapper.sh \
             /docker-entrypoint-initdb.d/00-replication-hba.sh
ENTRYPOINT ["docker-entrypoint-wrapper.sh"]
