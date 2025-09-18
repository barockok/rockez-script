#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SNAP_DIR="$ROOT_DIR/snapshots"
DEST_DIR="$ROOT_DIR/snapshots_copy"

rm -rf "$SNAP_DIR" "$DEST_DIR"
mkdir -p "$SNAP_DIR" "$DEST_DIR"

echo "Starting Elasticsearch via docker-compose..."
docker compose -f "$ROOT_DIR/docker-compose.yml" up -d

# Wait for ES
echo "Waiting for Elasticsearch to be ready..."
until curl -sSf http://localhost:9200 >/dev/null; do sleep 2; done
sleep 5

echo "Indexing seed data..."
curl -sS -H 'Content-Type: application/x-ndjson' --data-binary @"$ROOT_DIR/test/seed.json" http://localhost:9200/_bulk | jq .errors -r

echo "Registering local snapshot repo..."
curl -sS -X PUT http://localhost:9200/_snapshot/local_repo -H 'Content-Type: application/json' -d "{\n  \"type\": \"fs\",\n  \"settings\": {\n    \"location\": \"/snapshots\",\n    \"compress\": true\n  }\n}"

SNAP_NAME="snap-$(date +%s)"
echo "Creating snapshot $SNAP_NAME..."
curl -sS -X PUT "http://localhost:9200/_snapshot/local_repo/$SNAP_NAME?wait_for_completion=true" -H 'Content-Type: application/json' -d '{"indices":"orders-*,users-*"}' >/dev/null

echo "Copying snapshot repo locally using CLI with grep 'orders-' and renaming to 'orders-restored-$1'..."
"$ROOT_DIR/rockez-script" \
  --src-dir "$SNAP_DIR" \
  --dest-dir "$DEST_DIR" \
  --grep 'orders-.*' \
  --rename-pattern 'orders-(.*)' \
  --rename-replacement 'orders-restored-$1' \
  --timeframe "2000-01-01T00:00:00Z/2100-01-01T00:00:00Z"

echo "Registering copied repo as local_repo_copy..."
curl -sS -X PUT http://localhost:9200/_snapshot/local_repo_copy -H 'Content-Type: application/json' -d "{\n  \"type\": \"fs\",\n  \"settings\": {\n    \"location\": \"/snapshots_copy\",\n    \"compress\": true\n  }\n}"

# Find restore request json created by the tool
RESTORE_FILE=$(ls -1 "$DEST_DIR/restore-requests"/restore_*.json | tail -n 1)
if [[ -z "${RESTORE_FILE:-}" ]]; then
  echo "Restore request file not found" >&2
  exit 1
fi
echo "Using restore file: $RESTORE_FILE"

# Determine most recent snapshot name from the copied index.latest
IDX_LATEST=$(cat "$DEST_DIR/index.latest")
INDEX_FILE="$DEST_DIR/index-$IDX_LATEST"
SNAP_NAME_JSON=$(jq -r '.snapshots[-1].snapshot' "$INDEX_FILE")

echo "Restoring snapshot $SNAP_NAME_JSON with rename..."
curl -sS -X POST "http://localhost:9200/_snapshot/local_repo_copy/$SNAP_NAME_JSON/_restore" \
  -H 'Content-Type: application/json' \
  --data-binary @"$RESTORE_FILE"

echo "Waiting a moment for restore..."
sleep 5

echo "Verifying restored indices exist..."
curl -sS http://localhost:9200/_cat/indices?v

echo "Done."
