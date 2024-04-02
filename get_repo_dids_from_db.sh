#!/bin/bash -e

docker compose exec -u postgres db psql bluesky_indexer --csv -c 'SELECT repo FROM gorm_db_jobs' > db_repos.csv
