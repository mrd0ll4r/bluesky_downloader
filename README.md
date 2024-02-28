# bluesky_downloader
Tools to Download the Bluesky Network


## Installation

The `bluesky-repo-downloader` needs Go 1.22 to build.
Some things need a Postgres database to be accessible on `localhost:5434`.
This is currently done via docker compose.

## Usage

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