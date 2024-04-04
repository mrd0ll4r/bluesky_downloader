# Bluesky Tools

Tools to Download and save data from the Bluesky Network.

## Installation/Building

Needs a recent version of Go to run.
Some things need a Postgres database to be accessible on `localhost:5434`.
This is currently done via docker compose, see [docker-compose.yml](docker-compose.yml).

Building the binaries can be done via `./build.sh`.

## Usage

### `bluesky-labeler-logger`

This logs all labels produced by a labeler.

Basic usage:
```
./bluesky-labeler-logger [flags] <DID>
Arguments:
      <DID>		DID of the labeler to subscribe to
Flags:
      --db string               DSN to connect to a postgres database (default "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer")
      --debug                   Whether to enable debug logging
      --entries-per-file uint   The number of events after which the output file is rotated (default 1000)
      --outdir string           Path to the base output directory (default "./labeler_logs")
```
Quit with `Ctrl-C`.

It needs a Postgres database running somewhere, see [init_db.sh](init_db.sh) for initial setup to use the default configuration.
This can be achieved by running `docker compose up`.

The tool connects to the firehose and logs all received messages to disk with a timestamp attached to them.
The files are saved to `outdir`, with subdirectories per DID.
Each log file is named after the Unix timestamp at which it was created.
Every `entries-per-file` entries, the log file is rotated and compressed.
On shutdown, the current file is compressed.

A cursor pointing to the last processed sequence number is saved in the Postgres database.
This is updated every 50 events, and when the program shuts down.
This should make it possible to restart without losing any events.
In case of a crash, at most 50 events could be duplicated at the end of the old and the beginning of the new log.

### `bluesky-firehose-logger`

This logs all messages broadcast by the firehose to disk.

Basic usage:
```
./bluesky-firehose-logger [flags]
Flags:
      --db string               DSN to connect to a postgres database (default "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer")
      --debug                   Whether to enable debug logging
      --entries-per-file uint   The number of events after which the output file is rotated (default 10000)
      --outdir string           Path to the base output directory (default "firehose_logs")
      --save-blocks             Whether to save binary block data for repo commits. This can take a lot of space.
```
Quit with `Ctrl-C`.

It needs a Postgres database running somewhere, see [init_db.sh](init_db.sh) for initial setup to use the default configuration.
This can be achieved by running `docker compose up`.

The tool connects to the firehose and logs all received messages to disk with a timestamp attached to them.
The files are saved to `outdir`.
Each log file is named after the Unix timestamp at which it was created.
Every `entries-per-file` entries, the log file is rotated and compressed.
On shutdown, the current file is compressed.

If `--save-blocks` is provided, the block data attached to each repo commit is saved in the output.
This is encoded as base64, and quite large.
For longer-running operations, unless absolutely required, it's recommended to turn this off.

A cursor pointing to the last processed sequence number is saved in the Postgres database.
This is updated every 50 events, and when the program shuts down.
This should make it possible to restart without losing any events.
In case of a crash, at most 50 events could be duplicated at the end of the old and the beginning of the new log.

### `bluesky-repo-downloader`

This goes and downloads all current repos for all known DIDs to disk.

Basic usage:
```
ENABLE_REPO_DISCOVERY=true go run ./cmd/bluesky-repo-downloader/main.go
```
Quit with `Ctrl-C`.

It needs a Postgres database running on `localhost:5434`, see [init_db.sh](init_db.sh) for initial setup.
This can be achieved by running `docker compose up`.

The environment variable `ENABLE_REPO_DISCOVERY` should be set to enable repo discovery.
This will make the downloader iterate through all repositories known to the BGS, and add a job for each of them.

The downloader then iterates through all enqueued jobs and downloads the latest revision of their repo.
Data is downloaded to a hardcoded `repos/` directory.
Subdirectories will be created for all the DIDs processed, by DID protocol, and sharded by DID value.
For example, the did `did:plc:ewvi7nxzyoun6zhxrhs64oiz` would, by default, be saved to
`repos/did/plc/ewvi/7nxzyoun6zhxrhs64oiz/repo_revisions/<revision>.json.gz`.
We also save the CAR file, as `<revision>.car.gz`.

The jobs are saved in Postgres, as is their state.
This means that it should be possible to stop and re-start processing at any point.

#### TODO

- We could easily implement downloading diffs to keep these up to date, maybe?

## License

GPL, see [LICENSE](LICENSE).