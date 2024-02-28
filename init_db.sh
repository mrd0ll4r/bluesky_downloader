#!/bin/bash
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
	CREATE USER bluesky_indexer;
	CREATE DATABASE bluesky_indexer;
    ALTER DATABASE bluesky_indexer OWNER TO bluesky_indexer;
    ALTER USER bluesky_indexer WITH ENCRYPTED PASSWORD 'bluesky_indexer';
EOSQL
