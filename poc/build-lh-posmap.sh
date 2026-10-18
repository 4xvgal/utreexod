#!/usr/bin/env bash
# PoC: build a lab utreexod image that uses the LH (linear hashing) position map.
#
# The LH-enabled utreexo library is committed in-repo at ./utreexo-lh and wired
# via `replace github.com/utreexo/utreexo => ./utreexo-lh`, so Dockerfile.lab
# (COPY . /src) builds it without an absolute host path. This script refreshes
# that copy from the bench branch and builds the image.
#
#   bash poc/build-lh-posmap.sh
#   # then run the mainnet override (reuses the existing datadir volume):
#   UTREEXOD_LAB_PATH=/Volumes/mx500/utreexo-lab/utreexod docker compose \
#     -f docker/mainnet-lab/docker-compose.yml \
#     -f docker/mainnet-lab/docker-compose.lh-posmap.yml up -d --build
#
# The map is selected at runtime by UTREEXOD_POSMAP=lh (default: swiss).
set -euo pipefail

LAB=$(cd "$(dirname "$0")/.." && pwd)
BENCH=${BENCH:-/Volumes/mx500/utreexo-lab/bench-l1}
IMAGE=${IMAGE:-cw-utreexod-lh:poc}

echo "==> refresh in-repo LH library: $LAB/utreexo-lh"
rm -rf "$LAB/utreexo-lh"
mkdir -p "$LAB/utreexo-lh"
git -C "$BENCH" archive bench/l2-forest | tar -x -C "$LAB/utreexo-lh" \
  --exclude='internal/l2bench' \
  --exclude='internal/swisstable/bench_l1_test.go' \
  --exclude='internal/swisstable/bench_l1_alt_test.go'
git -C "$BENCH" show v0.18.0:go.mod > "$LAB/utreexo-lh/go.mod"
git -C "$BENCH" show v0.18.0:go.sum > "$LAB/utreexo-lh/go.sum"

echo "==> docker build -f Dockerfile.lab -t $IMAGE"
docker build -f "$LAB/Dockerfile.lab" -t "$IMAGE" "$LAB"
echo "DONE -> $IMAGE"
