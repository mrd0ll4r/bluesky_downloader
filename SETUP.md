# Data Collection Setup

Are you interested in data and scripts used for our Bluesky papers?
Great! Thanks for the interest!

**The good news:** You can collect almost all the data we used yourself!
And it's not too difficult, even.  
**The bad news:** It will take some time to get it all set up.

This document outlines the general data collection setup we operate,
as well as tips on how to work with the collected data.

#### Prerequisites

You will need
- A (or multiple) servers with Linux and Docker installed.
    Hardware requirements depend on what you want to do.
    Getting snapshots of the whole network is currently the most resource-hungry
    of them all, and will require multiple TB of SSD storage.
- A few useful tools installed, such as `jq`, `parallel`, `mlr` (Miller)
- Be able to read and write Bash scripts
- Be able to read (and optionally) write SQL

## Mirror of `plc.directory`

This is the easiest one and should get you started.

We use an existing tool to reconstruct `plc.directory` from the exported logs of the public instance.
See [bsky-watch/plc-mirror](https://github.com/bsky-watch/plc-mirror) for the source code and setup instructions.

We host a public instance of this at https://plc-mirror.bsky.leobalduf.com.
This is provided without any uptime guarantees etc.
Query the public instance like so:
```bash
curl -s "https://plc-mirror.bsky.leobalduf.com/did:plc:nggqjgdkqhytcag6x7fhiyuv" | jq .
```

It is a good idea to set this up on a server somewhere and leave it to sync for a few days.
Once it's caught up, you can export a snapshot like so:
```bash
#!/bin/bash -e

#-- This first filters to only include the latest DID document state.
#-- We then extract DID and handle.
#-- It allocates a giant temporary table, so it's pretty slow and resource-hungry.
QUERY="COPY (SELECT t.did, t.cid, t.plc_timestamp, t.operation AS newest_op FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY did ORDER BY plc_timestamp DESC) as r FROM plc_log_entries WHERE nullified=FALSE) AS t WHERE t.r=1) TO STDOUT WITH CSV HEADER;"

docker compose exec -u postgres db psql bluesky_plc -c "$QUERY" |
  # CSV to JSON
  mlr --icsv --ojsonl cat |
  # Parse that ungodly quoted JSON object
  jq -c '(.newest_op = (.newest_op | fromjson))' |
  # Save to a file
  gzip -9 > "plc_mirror_dump_$(date -u +'%Y-%m-%d_%H-%M-%S')_UTC.json.gz"
```

## Feed Generator Output

Please see [our repo for that](https://github.com/bibo7086/bsky-feed-generators-tracker).

## Firehose Logs

We run an instance of `bluesky-firehose-logger` to log all events broadcast by
the Firehose.

- **Difficulty**: Easy
- **Code**: this repository ([see here](https://github.com/mrd0ll4r/bluesky_downloader?tab=readme-ov-file#bluesky-firehose-logger)).
- **Building**: Use the `build.sh` script, or `go build -v ./cmd/bluesky-firehose-logger/`
- **Prerequisites for running**: You need a Postgres database to persist the cursor.
    See the [docker compose setup](https://github.com/mrd0ll4r/bluesky_downloader/blob/master/docker-compose.yml) in our repo.
- **Notes**: We run this with `--save-blocks`, in order to later be able to decode the content of added or updated records.

We recommend you run this on a well-connected machine with lots of disk space
available (approx 100GB/day).
To replicate our setup, run with `--save-blocks`.
**TODO** Add systemd unit file and explanation.

#### Translating the logs

By default, the logs are saved as line-delimited JSON.
Block data is left untouched, and saved in the JSON objects as base64-encoded
strings.
This wastes a lot of space, and you can't easily read the records.
For that reason, it is probably preferable to **decode the block data** to a
more useful and compact format.

For that, we wrote a tool called `bluesky-extract-commits-from-firehose-logs`

- **Difficulty**: Medium?
- **Code**: this repository ([see here](https://github.com/mrd0ll4r/bluesky_downloader?tab=readme-ov-file#bluesky-extract-commits-from-firehose-logs)).
- **Building**: Use the `build.sh` script, or `go build -v ./cmd/bluesky-extract-commits-from-firehose-logs/`
- **Prerequisites for running**: Not strictly required, but it's very useful to have `jq` installed.

Using a script like this (`extract_commits_from_file.sh`):
```bash
#!/bin/bash -e

set -o pipefail

if [ $# != 1 ]; then
        echo "Need one argument: the input file"
        exit 2
fi

TMP_DIR=$(mktemp -d --suff="firehose")
mkdir -p "$TMP_DIR"
trap 'rm -rf -- "$TMP_DIR"' EXIT

INPUT_FILE=$1
TARGET_DIR_COMMITS="$(dirname $INPUT_FILE)/repo_commits"
TARGET_DIR_BAK="$(dirname $INPUT_FILE)/filtered_logs_bak"
mkdir -p "$TARGET_DIR_COMMITS"
mkdir -p "$TARGET_DIR_BAK"
TARGET_FILE="$TARGET_DIR_COMMITS/$(basename $INPUT_FILE)"
TARGET_FILE_LOG="$TARGET_DIR_COMMITS/$(basename $INPUT_FILE '.json.gz').log"
INPUT_FILE_BAK="$TARGET_DIR_BAK/$(basename $INPUT_FILE).bak"

TARGET_FILE_TMP="$TMP_DIR/commits.json.gz"
FILTERED_FILE_TMP="$TMP_DIR/no_commits.json.gz"
LOG_FILE_TMP="$TMP_DIR/translation.log"

echo "Working on $INPUT_FILE, will back up to $INPUT_FILE_BAK, write output to $TARGET_FILE, logs to $TARGET_FILE_LOG, filtered input to $INPUT_FILE, tmpdir is $TMP_DIR"

# Check if target file already exists
if [ -f "$TARGET_FILE" ]; then
        echo "Target file exists, skipping..."
        exit 0
fi

# Back up input file
mv "$INPUT_FILE" "$INPUT_FILE_BAK"

# Extract commits
zcat "$INPUT_FILE_BAK" | bluesky-extract-commits-from-firehose-logs 2>"$LOG_FILE_TMP" | gzip -9 > "$TARGET_FILE_TMP"

# Remove commits from original
zcat "$INPUT_FILE_BAK" | jq -c 'del(.repo_commit)' | gzip -9 > "$FILTERED_FILE_TMP"

# Move files
mv "$TARGET_FILE_TMP" "$TARGET_FILE.tmp"
mv "$FILTERED_FILE_TMP" "$INPUT_FILE.tmp"
mv "$LOG_FILE_TMP" "$TARGET_FILE_LOG.tmp"

# Atomically rename
mv "$TARGET_FILE.tmp" "$TARGET_FILE"
mv "$TARGET_FILE_LOG.tmp" "$TARGET_FILE_LOG"
mv "$INPUT_FILE.tmp" "$INPUT_FILE"

# Remove backup
rm "$INPUT_FILE_BAK"
```

... you can then translate all the logs like so (using GNU parallel):
```bash
find "$PATH_TO_LOG_FILES" -type f -name '*.json.gz' |
  # Sort for performance
  sort |
  parallel --will-cite "./extract_commits_from_file.sh \"{}\"" >> "/extract_commits.log"
```

## Labeler Logs

We wrote a tool to collect labeler events to JSON.
The tool connects to the endpoint of one labeler and backfills all events for that labeler.

- **Difficulty**: Easy
- **Code**: this repository ([see here](https://github.com/mrd0ll4r/bluesky_downloader?tab=readme-ov-file#bluesky-labeler-logger)).
- **Building**: Use the `build.sh` script, or `go build -v ./cmd/bluesky-labeler-logger/`
- **Prerequisites for running**: You need a Postgres database to persist the cursor.
  See the [docker compose setup](https://github.com/mrd0ll4r/bluesky_downloader/blob/master/docker-compose.yml) in our repo.

We provide **systemd service files** to run many instances of this, see [dist](dist).
You will need to adjust some paths depending on your setup.

In order to have logs from all labelers, we use a script to go through Firehose
logs and a copy of `plc.directory` on a daily basis.
The script extracts labeler service announcements and launches the service for
their DID.
Assuming you have a list of DIDs in a file called `all_labelers.dids`:
```bash
#!/bin/bash -e

num_current_instances=$(systemctl --user list-units --type=service | grep -F 'bluesky-labeler-logger@' | wc -l)
num_dids=$(cat all_labelers.dids | wc -l)

echo "I will attempt to start $num_dids service instances (currently instantiated: $num_current_instances)."

# Somewhat-dirty hack: We need to remove the failed services to retry...
systemctl --user stop bluesky-labeler-logger.target
systemctl --user reset-failed

while IFS=$'\n' read -r line; do
        echo "Starting service for $line..."
        systemctl --user start "bluesky-labeler-logger@$line.service"
done < all_labelers.dids
```

## Full-Network Snapshots

We use an existing tool to correctly do this: [uabluerail/indexer](https://github.com/uabluerail/indexer).
We maintain a branch with the diffs we made for our deplyment [here](https://github.com/mrd0ll4r/bsky-indexer/tree/personal-deployment).

- **Difficulty**: Medium to set up, but needs a lot of hardware.

We run this on a recent Debian with ZFS and Postgres.
Recent versions of the indexer use ScyllaDB on XFS.
This might be better, but this document describes our setup, not the optimal one.

Install a few useful packages:
```
sudo apt update
sudo apt install mosh \
    wireguard \
    htop \
    vim \
    git \
    build-essential \
    screen \
    jq
```

Add these to `/etc/apt/sources.list`:
```
deb https://deb.debian.org/debian/ bookworm main contrib non-free non-free-firmware
deb-src https://deb.debian.org/debian/ bookworm main contrib non-free non-free-firmware

deb http://security.debian.org/debian-security bookworm-security main contrib non-free non-free-firmware
deb-src http://security.debian.org/debian-security bookworm-security main contrib non-free non-free-firmware

# bookworm-updates, to get updates before a point release is made;
# see https://www.debian.org/doc/manuals/debian-reference/ch02.en.html#_updates_and_backports
deb https://deb.debian.org/debian/ bookworm-updates main contrib non-free non-free-firmware
deb-src https://deb.debian.org/debian/ bookworm-updates main contrib non-free non-free-firmware

# bookworm-backports, previously on backports.debian.org
deb https://deb.debian.org/debian/ bookworm-backports main contrib non-free non-free-firmware
deb-src https://deb.debian.org/debian/ bookworm-backports main contrib non-free non-free-firmware
```

Install ZFS:
```bash
sudo apt install linux-headers-amd64
sudo apt install -t stable-backports zfsutils-linux
```

Install Docker as per [here](https://docs.docker.com/engine/install/debian/):
```bash
for pkg in docker.io docker-doc docker-compose podman-docker containerd runc; do sudo apt-get remove $pkg; done

# Add Docker's official GPG key:
sudo apt-get update
# Maybe remove wrong libcurl4 version?
#sudo apt remove libcurl4
sudo apt-get install ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources:
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
```

Fix docker groups:
```bash
sudo groupadd docker
sudo usermod -aG docker $USER
```
Log out and back in.

Check if it works:
```bash
docker run hello-world
```

### ZFS

We run this on cloud infrastructure, so we're not worried about disks dying.
As such, we run a striped setup, adding 2TB block devices when we run out of storage.
Here is the initial setup for *one disk*, replace `DISK` with a **stable** path/UUID/... to the disk, e.g. something from `/dev/disk/by-path/`.

```bash
sudo zpool create -o ashift=12 -o autotrim=on -O recordsize=128k -O atime=off -O acltype=posixacl -O xattr=sa -O dnodesize=auto -O compression=lz4 -O normalization=formD -O relatime=on tank DISK 
sudo zfs create tank/bluesky_db -o mountpoint=/tank/bluesky_db -o recordsize=64k
sudo zfs create tank/bluesky_csv -o mountpoint=/tank/bluesky_csv -o recordsize=128k
sudo zfs create tank/bluesky_metrics -o mountpoint=/tank/bluesky_metrics -o recordsize=128k
```

Our machine has 32G of memory.
We saw situation where the ZFS ARC used too much of that, and the indexer services were killed due to OOM.
The optimal solution here would be to *buy more RAM*, but that's not an option for us.
As such, we adjust the ARC to take up less space.

Add this to `/etc/modprobe.d/zfs.conf`
```
# ARC size 16GB
options zfs zfs_arc_max=17179869184

# ARC: Keep 4GB of memory free at all times
options zfs zfs_arc_sys_free=4294967296
```

Same, but immediately:
```
echo 4294967296 | sudo tee /sys/module/zfs/parameters/zfs_arc_sys_free
echo 17179869184 | sudo tee /sys/module/zfs/parameters/zfs_arc_max
sudo update-initramfs -u -k all
```

### Indexer

```bash
cd
mkdir -p projects/bluesky
cd projects/bluesky
git clone -b personal-deployment https://github.com/mrd0ll4r/bsky-indexer.git indexer
```

Add this to `.env`:
```bash
# Sent in the User-Agent header.
CONTACT_INFO="your-email@example.com"

ATP_PLC_ADDR=https://plc-mirror.bsky.leobalduf.com
POSTGRES_PASSWORD='SomeSecureAndLongPassword'
DATA_DIR=/tank/bluesky_db
CSV_DIR=/tank/bluesky_csv
#COLLECTION_BLACKLIST=app.bsky.feed.like,app.bsky.feed.post,app.bsky.feed.repost

# Used only for PDS discovery.
JETSTREAM=wss://jetstream2.us-east.bsky.network

# IP address to expose HTTP ports on
METRICS_ADDR=127.0.0.1

# IP Address of the VPN interface, if you have one.
# This is where the Grafana and Postgres will be reachable on.
VPN_IP=10.0.24.123

# Grafana URL with username and password. Only needed if you're going to import the dashboard.
GRAFANA_URL="http://@$VPN_IP:9000"

GRAFANA_DATA_DIR=/tank/bluesky_metrics/grafana
PROMETHEUS_DATA_DIR=/tank/bluesky_metrics/prometheus
```

And this to `docker-compose.override.yml`:
```yaml
# See https://docs.docker.com/compose/multiple-compose-files/merge/ for how
# exactly these overrides get applied to the main file.
# tl;dr: strings and numbers get overwritten, lists get concatenated
services:
  # Expose PostgreSQL TCP port
  postgres:
    ports:
      - "$VPN_IP:5432:5432"
    command: [
      "-c", "max_connections=500",
      "-c", "max_parallel_workers_per_gather=8",
      "-c", "shared_buffers=10GB",
      "-c", "work_mem=1GB",
      "-c", "maintenance_work_mem=1GB",
      "-c", "vacuum_buffer_usage_limit=128MB",
      "-c", "autovacuum_vacuum_cost_delay=0",
      "-c", "max_wal_size=4GB"
      ]
    shm_size: '16gb'
    oom_score_adj: -200
    stop_grace_period: 24h

  # Remove ScyllaDB
  scylladb: !reset null

  update-db-schema:
    environment:
      UPDATE-DB-SCHEMA_SCYLLADB_ADDR: ""

  lister:
    environment:
      LISTER_SCYLLADB_ADDR: ""
    oom_score_adj: 100

  consumer:
    environment:
      CONSUMER_SCYLLADB_ADDR: ""
    oom_score_adj: 100

  pds-discovery:
    oom_score_adj: 100

  # Change the default number of indexer threads
  record-indexer:
    environment:
      INDEXER_WORKERS: 20
      INDEXER_SCYLLADB_ADDR: ""
    deploy:
      resources:
        limits:
          memory: 8G
    oom_score_adj: 200
  
  prometheus:
    image: prom/prometheus
    # needed if mounted in custom volume
    user: root
    volumes:
      - "./metrics/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml"
      - "${PROMETHEUS_DATA_DIR:?specify data dir in .env file}:/prometheus"
    restart: unless-stopped
    extra_hosts:
      - "host.docker.internal:host-gateway"
    ports:
      - "$VPN_IP:9090:9090"

  grafana:
    build:
      context: ./metrics/grafana
    user: root
    restart: unless-stopped
    extra_hosts:
      - "host.docker.internal:host-gateway"
    ports:
      - "$VPN_IP:9000:3000"
    volumes:
      - ${GRAFANA_DATA_DIR:?specify data dir in .env file}:/var/lib/grafana

  node-exporter:
    command:
      - '--path.procfs=/host/proc'
      - '--path.rootfs=/rootfs'
      - '--path.sysfs=/host/sys'
      - '--collector.filesystem.ignored-mount-points=^/(sys|proc|dev|host|etc)($$|/)'
    restart: unless-stopped
    expose:
      - 9100
    image: prom/node-exporter:v1.4.0
    volumes:
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
      - /:/rootfs:ro
    ports:
      - "127.0.0.1:9100:9100"
```

Build:
```bash
docker compose build
```

Start and configure the DB:
```bash
make start-db
make init-db
make create-readonly-user
make partition-db
```

Start grafana etc to upload the dashboard:
```
docker compose up node-exporter prometheus grafana
```
And import `dashboards/indexer.json`.

Start all the things:
```bash
docker compose up -d
```

### Snapshots

Once everything finishes downloading (this depends on the speed of your disks),
you can export data from the database.

We use materialized views for this, for example:
```sql
create materialized view export_follows
as select repos.did as "src",
  records.content ->> 'subject' as "dst",
  records.content ->> 'createdAt' as created_at
  from repos join records on repos.id = records.repo
  where records.collection = 'app.bsky.graph.follow'
  AND NOT records.deleted
with no data;
```
(see [this file](https://github.com/mrd0ll4r/bsky-indexer/blob/personal-deployment/create_export_views.sql) for all the views)

and then something like this to export the data:
```bash
#!/bin/sh

set -e
DATE=$(date -u '+%Y-%m-%d')
OUTDIR="/tank/bluesky_csv/exports/$DATE"
mkdir -p "$OUTDIR"

# ------------------------------ Write data timestamp ----------------------------------

echo "export_start" > "$OUTDIR/timestamp.csv"
date -Iseconds --utc >> "$OUTDIR/timestamp.csv"

################################################
# Follows
docker compose exec -iT postgres psql -U postgres -d bluesky <<- EOF
\timing
\echo Refreshing follows...
refresh materialized view export_follows;
EOF

echo "Writing .csv file..."
docker compose exec -it postgres psql -U postgres -d bluesky \
  -c "copy (select * from export_follows) to stdout with csv header;" | gzip -9 > "$OUTDIR/follows.csv.gz"

docker compose exec -iT postgres psql -U postgres -d bluesky -c "REFRESH MATERIALIZED VIEW export_follows WITH NO DATA;"
```

## Getting Historic Data from Us

We can not make this data public, since it contains PII.

You can (and should) set up the above data collection.
Most of it is not too difficult.
This will give you *almost* everything we have, and makes it easy for you to
work with the data.

The only thing it doesn't get you is historic Firehose data.
If you need this data for your research, please get in touch.
Please describe **in as much detail as possible** which subset(s) of the data
you need.
This usually contains:
- a time period for which you need data
- which collections (i.e. record types) you need the data from
- what you're planning to do with the data

Similarly, we can share a snapshot of a specific colleciton of records in the
database, if we happen to have it.

In ascending order of difficulty, this is what we can share for research purposes:
1. `plc.directory` snapshot: JSON, pretty easy
2. Labeler logs of specific labelers: CSV or JSON, not too big, pretty easy
3. Firehose data. If you know **exactly** what you need, this can be done.
4. Query exports from the snapshot database: If you have a *functional* query to execute against the Postgres database, we can export this data for you.
   This takes some time, and we need to turn off the indexer while exporting, due to limited hardware capacity, so please make sure your query **works**.
