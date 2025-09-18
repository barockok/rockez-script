## rockez-script

Simple Go CLI to prepare Elasticsearch restore from a GCS snapshot repository.

What it does:
- Copies the entire snapshot repository from a source GCS bucket to a destination GCS bucket (server-side copy).
- Reads repository metadata (`index.latest` and `index-N`) to pick a snapshot that matches a given timeframe and optional grep pattern.
- Generates a restore request JSON body in the destination bucket under `restore-requests/` that you can POST to Elasticsearch's restore API.

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

Run:
```bash
./rockez-script \
  --src-service-account=/path/to/src.json \
  --src-bucket=your-src-bucket \
  --dest-service-account=/path/to/dest.json \
  --dest-bucket=your-dest-bucket \
  --timeframe=2025-09-01T00:00:00Z/2025-09-10T23:59:59Z \
  --grep=index-prefix-
```

Notes:
- The repository is copied entirely to keep restore compatibility. The timeframe/grep are used to suggest a snapshot and indices filter for the restore API payload that is written as JSON.
- Ensure the destination repo is registered in Elasticsearch (via `PUT _snapshot/{name}`) pointing to the destination bucket before running restore.
