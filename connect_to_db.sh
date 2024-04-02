#!/bin/bash -e

docker compose exec -u postgres db psql -d bluesky_indexer
