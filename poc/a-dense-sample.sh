#!/usr/bin/env bash
# Sample an A/B run: height, RSS, cgroup anon/file, faults, block I/O.
#   bash poc/a-dense-sample.sh <seconds> <csv> [container]
# Total cgroup anon is the metric: the WAL cache region is anon and may share a
# VMA with the Go heap arena, so per-mapping smaps attribution is unreliable.
set -u
DUR=${1:-900}
OUT=${2:-/tmp/a-dense.csv}
C=${3:-cw_utreexod_a_dense}
[ -f "$OUT" ] || echo "ts,height,vm_rss_kb,cg_anon_kb,cg_file_kb,majflt,minflt,io_read_bytes,io_write_bytes" > "$OUT"
end=$(( $(date +%s) + DUR ))
while [ "$(date +%s)" -lt "$end" ]; do
  h=$(docker logs --tail 60 "$C" 2>&1 | grep -oE "height [0-9]+" | tail -1 | awk '{print $2}')
  vals=$(docker exec "$C" sh -c '
    awk "/^VmRSS:/{print \$2}" /proc/1/status
    awk "/^anon /{print \$2} /^file /{print \$2}" /sys/fs/cgroup/memory.stat
    awk "{print \$12}" /proc/1/stat
    awk "{print \$10}" /proc/1/stat
    grep -E "^read_bytes" /proc/1/io | awk "{print \$2}"
    grep -E "^write_bytes" /proc/1/io | awk "{print \$2}"
  ' | paste -sd, -)
  echo "$(date +%H:%M:%S),${h:-0},$vals" >> "$OUT"
  sleep 30
done
