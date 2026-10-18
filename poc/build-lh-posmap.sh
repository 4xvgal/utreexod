#!/usr/bin/env bash
# PoC: build a lab utreexod that uses the LH (linear hashing) position map.
#
# Materializes the LH-enabled utreexo library from the bench branch, then
# cross-compiles the lab fork against it (linux/arm64) and builds the PoC image.
#
#   bash poc/build-lh-posmap.sh
#   colima start --vm-type vz --cpu 4 --memory 6 --disk 40
#   docker compose -f <coconut_wallet>/docker/regtest/docker-compose.posmap-poc.yml up -d
#
# The position map is selected at runtime by UTREEXOD_POSMAP=lh (default swiss).
set -euo pipefail

BENCH=${BENCH:-/Volumes/mx500/utreexo-lab/bench-l1}
LH=${LH:-/Volumes/mx500/utreexo-lab/utreexo-lh}
POC=${POC:-/Volumes/mx500/utreexo-lab/poc}
LAB=$(cd "$(dirname "$0")/.." && pwd)

echo "==> materialize LH utreexo -> $LH"
git -C "$BENCH" rev-parse --verify bench/l2-forest >/dev/null
rm -rf "$LH"; mkdir -p "$LH"
git -C "$BENCH" archive bench/l2-forest \
  | tar -x -C "$LH" \
      --exclude='internal/l2bench' \
      --exclude='internal/swisstable/bench_l1_test.go' \
      --exclude='internal/swisstable/bench_l1_alt_test.go'
# pristine v0.18.0 dependency graph (the LH files add no deps)
git -C "$BENCH" show v0.18.0:go.mod > "$LH/go.mod"
git -C "$BENCH" show v0.18.0:go.sum > "$LH/go.sum"

echo "==> cross-compile lab utreexod (linux/arm64) -> $POC/utreexod"
mkdir -p "$POC"
cd "$LAB"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$POC/utreexod" .

cat > "$POC/Dockerfile" <<'DOCKER'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY utreexod /usr/local/bin/utreexod
ENTRYPOINT ["utreexod"]
DOCKER

echo "==> docker image cw-utreexod-lh:poc"
docker build -t cw-utreexod-lh:poc "$POC"
echo "DONE"
