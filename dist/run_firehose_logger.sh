#!/bin/bash -e

# Sophisticated script to keep the firehose logger running.

while true; do
	echo "Running firehose logger..."
	./bluesky-firehose-logger --db 'postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer' --entries-per-file 100000 --save-blocks || true
	echo "Died, sleeping..."
	sleep 5
done
