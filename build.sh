#!/bin/bash -e

go build -v ./cmd/bluesky-firehose-logger/
go build -v ./cmd/bluesky-labeler-logger/
go build -v ./cmd/bluesky-repo-downloader/
