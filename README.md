# Bluesky Tools

Tools to Download and save data from the Bluesky Network.

## Installation/Building

Needs a recent version of Go to run.
Some things need a Postgres database to be accessible on `localhost:5434`.
This is currently done via docker compose, see [docker-compose.yml](docker-compose.yml).

Building the binaries can be done via `./build.sh`.

The systemd services files are intended to be installed per user.
Copy the files to ` ~/.config/systemd/user/` and run `systemctl --user daemon-reload`.


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
It uses a database to keep track of which revisions (actually which commit CID) we have for a given repo, to avoid re-downloading unchanged repos.

Basic usage:
```
./bluesky-repo-downloader [flags]
Flags:
      --db string       DSN to connect to a postgres database (default "postgres://bluesky_indexer:bluesky_indexer@localhost:5434/bluesky_indexer")
      --debug           Whether to enable debug logging
      --outdir string   Path to the base output directory (default "repos")
      --repoDiscovery   Whether to enable repo discovery
```
Quit with `Ctrl-C`.

It needs a Postgres database running somewhere, see [init_db.sh](init_db.sh) for initial setup to use the default configuration.
This can be achieved by running `docker compose up`.

If you enable repo discovery, it uses `sync.listRepos` on the BGS to get a list of all users and their current repo version.
If the version does not match the one we have downloaded, a job for the repo will be enqueued.

The downloader then iterates through all enqueued jobs and downloads the latest revision of their repo.
Data is downloaded to the `outdir` directory.
Subdirectories are created for DID schemata, and PLC DIDs are further divided as such:
The first four letters of the string-encoded DID value are used to index one subdirectory each.
Finally, the value is listed again.
For example, the DID `did:plc:ewvi7nxzyoun6zhxrhs64oiz` would be saved at
`<outdir>/did/plc/e/w/v/i/ewvi7nxzyoun6zhxrhs64oiz/repo_revisions/<revision>.json.gz`.
A copy of the raw car file of that revision is placed next to it as `<revision>.car.gz`.

It is possible to stop processing at any time via `SIGINT` and restart from where it left off.

#### TODO

- We could easily implement downloading diffs to keep these up to date, maybe?
    Not worth at the moment -- we'd still need to iterate over `sync.listRepos` to get revision mismatches and process each of those repos individually.

## License

GPL, see [LICENSE](LICENSE).
