## rockez-script

Simple Go CLI to prepare Elasticsearch restore from a GCS snapshot repository.

What it does:
- Copies the entire snapshot repository from a source GCS bucket to a destination GCS bucket (server-side copy).
- Reads repository metadata (`index.latest` and `index-N`) to pick a snapshot that matches a given timeframe and optional grep pattern.
- Generates a restore request JSON body in the destination bucket under `restore-requests/` that you can POST to Elasticsearch's restore API.

It also supports local directories as source and/or destination, so you can copy from/to the filesystem instead of GCS.

Inputs (flags):
- `--src-service-account`: path to source bucket service account JSON.
- `--src-bucket`: source GCS bucket name.
- `--dest-service-account`: path to destination bucket service account JSON.
- `--dest-bucket`: destination GCS bucket name.
- `--timeframe`: RFC3339 start/end, e.g. `2025-09-01T00:00:00Z/2025-09-10T23:59:59Z`.
- `--grep`: regex or plain text to match snapshot name or contained index names.

Build:
```bash
go build -o rockez-script
```

Run (GCS -> GCS):
```bash
./rockez-script \
  --src-service-account=/path/to/src.json \
  --src-bucket=your-src-bucket \
  --dest-service-account=/path/to/dest.json \
  --dest-bucket=your-dest-bucket \
  --timeframe=2025-09-01T00:00:00Z/2025-09-10T23:59:59Z \
  --grep=index-prefix-
```

Run (Local -> Local):
```bash
./rockez-script \
  --src-dir=/path/to/src-repo \
  --dest-dir=/path/to/dest-repo \
  --timeframe=2025-09-01T00:00:00Z/2025-09-10T23:59:59Z \
  --grep='myindex-.*'
```

Run (Local -> GCS) or (GCS -> Local): provide the appropriate pair for the side you use (dir or bucket+service-account), but not both for the same side.

Notes:
- The repository is copied entirely to keep restore compatibility. The timeframe/grep are used to suggest a snapshot and indices filter for the restore API payload that is written as JSON.
- Ensure the destination repo is registered in Elasticsearch (via `PUT _snapshot/{name}`) pointing to the destination bucket before running restore.

### Local test with Docker Compose

This repo includes a simple local verification using Docker Compose and a shell script.

Requirements: Docker, Docker Compose, `jq`.

Steps:
```bash
go build -o rockez-script
bash test/run.sh
```

What it does:
- Starts a single-node Elasticsearch with a mounted local snapshot repo at `./snapshots`.
- Indexes seed documents from `test/seed.json`.
- Creates a snapshot of indices (`orders-*`, `users-*`).
- Runs the CLI to copy the repo to `./snapshots_copy`, generating a restore request that filters with `--grep 'orders-.*'` and renames to `orders-restored-$1`.
- Registers the copied repo and restores it into Elasticsearch.
- Prints `_cat/indices` so you can verify `orders-restored-*` indices exist.
