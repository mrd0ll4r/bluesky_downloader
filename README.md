# Bluesky Tools

Tools to download and save data from the Bluesky Network.
For **very detailed setup instructions** instructions, please see [SETUP](SETUP.md).
For **a first overview**, please read this README.

These were used for data collection for the paper **Looking AT the Blue Skies of Bluesky**,
which was presented at **IMC'24** ([link](https://dl.acm.org/doi/10.1145/3646547.3688407), [arXiv](https://arxiv.org/abs/2408.12449)).
If you use these tools or datasets for academic work, please cite our publication:
```
@inproceedings{10.1145/3646547.3688407,
author = {Balduf, Leonhard and Sokoto, Saidu and Ascigil, Onur and Tyson, Gareth and Scheuermann, Bj\"{o}rn and Korczy\'{n}ski, Maciej and Castro, Ignacio and Kr\'{o}l, Michaundefined},
title = {Looking AT the Blue Skies of Bluesky},
year = {2024},
isbn = {9798400705922},
publisher = {Association for Computing Machinery},
address = {New York, NY, USA},
url = {https://doi.org/10.1145/3646547.3688407},
doi = {10.1145/3646547.3688407},
abstract = {The pitfalls of centralized social networks, such as Facebook and Twitter/X, have led to concerns about control, transparency, and accountability. Decentralized social networks have emerged as a result with the goal of empowering users. These decentralized approaches come with their own trade-offs, and therefore multiple architectures exist. In this paper, we conduct the first large-scale analysis of Bluesky, a prominent decentralized microblogging platform. In contrast to alternative approaches (e.g. Mastodon), Bluesky decomposes and opens the key functions of the platform into subcomponents that can be provided by third party stakeholders. We collect a comprehensive dataset covering all the key elements of Bluesky, study user activity and assess the diversity of providers for each sub-components.},
booktitle = {Proceedings of the 2024 ACM on Internet Measurement Conference},
pages = {76–91},
numpages = {16},
keywords = {bluesky, decentralized social networks, social network analysis},
location = {Madrid, Spain},
series = {IMC '24}
}
```

## Installation/Building

This needs a recent version of Go to run.
Some things need a Postgres database to be accessible on `localhost:5434`.
This is currently done via docker compose, see [docker-compose.yml](docker-compose.yml).

Building the binaries can be done via `./build.sh`.

The `dist/` directory contains a few useful scripts and systemd service files.
The systemd services files for labeler loggers are intended to be installed per user.
Adjust paths as needed and copy the files to ` ~/.config/systemd/user/`
and run `systemctl --user daemon-reload`.
After starting the services, you can list them with `systemctl --user list-units --type=service`.

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
The database stores the latest cursor per labeler DID.

The tool connects to the labeler endpoint specified in the DID document of the given account
and logs all received messages to disk with a timestamp attached to them.
The files are saved to `outdir`, with subdirectories per DID.
Each log file is named after the Unix timestamp at which it was created.
Every `entries-per-file` entries, the log file is rotated and compressed.
On shutdown, the current file is compressed.
During compression, a `<filename>.gzip.tmp` file is created and written to.
This file is renamed to `<filename>.gzip` once compression finishes.
This is useful when using `rsync` to move log files periodically:
You can specify `--exclude='*.json' --exclude='*.tmp'` to only move completed, compressed logs.

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
      --firehose string         Firehose to connect to (default "wss://bsky.network")
      --outdir string           Path to the base output directory (default "firehose_logs")
      --reset-cursor            Reset stored cursor to current cursor reported by the remote
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
Compression behaves the same as the Labeler logger.

If `--save-blocks` is provided, the block data attached to each repo commit is saved in the output.
This is encoded as base64, and quite large.
For longer-running operations, unless absolutely required, it's recommended to turn this off.

A cursor pointing to the last processed sequence number is saved in the Postgres database.
This is updated every 500 events, and when the program shuts down.
This should make it possible to restart without losing any events.
In case of a crash, at most 500 events could be duplicated at the end of the old and the beginning of the new log.

If `--reset-cursor` is provided, the persisted cursor is not used.
Instead, the stream of events from the Firehose is consumed without providing a cursor, i.e., from the current point on forwards.

The logger contains a liveness check.
If no messages from the Firehose are received and/or processed in five minutes, the logger automatically shuts down.
This is a clean shutdown, i.e., the sequence number of the last processed event is persisted.
On next startup, playback resumes from this cursor.

**Note**: Due to an earlier off-by-one error, clean restarts may have resulted in one event overlapping between the two runs.
Also, around November 22nd, the Firehose experienced some problems and emitted duplicate events.
In any case, it's probably a good idea to *a)* filter out duplicate sequence numbers and *b)* not rely on the Firehose to have all events of all types during those days.

### `bluesky-extract-commits-from-firehose-logs`

This extracts commits from Firehose logs and prints them as JSON.
The output format is similar, but not equal to the Jetstream output.
This reads the blocks of the commit.
As such, the Firehose logs **must include blocks**.
This currently **does not verify signatures**, because that's (probably?) already done at the level of the official Firehose.

Basic usage:
```
./bluesky-extract-commits-from-firehose-logs [flags]
Flags:
      --debug   Whether to enable debug logging
```

It reads Firehose logs as JSON via `stdin` and outputs the extracted and translated commits as JSON to `stdout`.
In case it completely fails to read the commit CAR file, it only includes basic info (DID, timestamp, commit CID) and adds an error message and the original binary block data instead of the commit content.
In case it decodes the CAR file but fails to parse any of the commit operations, it includes all operations, potentially annotated with errors, and the original binary block data.
Similarly, if the commit contains links to blobs, the link CIDs are included, as well as the original block data.

## License

GPL, see [LICENSE](LICENSE).
