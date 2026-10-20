#!/usr/bin/env bash
# A/B benchmark for the position map in the cache-pressure regime, on a Lima VM.
#
#   bash poc/bench-posmap-ab.sh                                  # harness control: base vs base
#   bash poc/bench-posmap-ab.sh base ./utreexo-lh alt ./utreexo-lh-prefetch
#
# Why a VM and not the host: the bench is //go:build linux and needs a cgroup,
# and BENCH_DIR must live on the guest's ext4 -- a virtiofs/9p mount of
# /Users or /Volumes measures the wrong filesystem.
#
# Sizing: MAP_N entries of the EH prefix map cost ~26.25 B/entry, so 40M entries
# = ~1.05 GB of map file. A 256 MiB cgroup puts the map 4x over cache, which is
# the regime where access order matters at all (below it: 100% hits, 0% delta).
# The map is built once with no cap and then reused via BENCH_DIR, so the A/B
# runs only measure the lookup/delete pass.
set -euo pipefail

VM=${VM:-eh-ab}
MAP_N=${MAP_N:-40000000}
CGROUP=${CGROUP:-256M}
DELS=${DELS:-8192}
REPS=${REPS:-30}

HERE=$(cd "$(dirname "$0")" && pwd)
LABEL_A=${1:-base}
DIR_A=${2:-$HERE/../utreexo-lh}
LABEL_B=${3:-$LABEL_A}
DIR_B=${4:-$DIR_A}

limactl list "$VM" >/dev/null 2>&1 || limactl create --name="$VM" --tty=false \
	--cpus=4 --memory=4 --disk=40 template:ubuntu-24.04
limactl shell "$VM" -- true >/dev/null 2>&1 || limactl start "$VM" --tty=false

GH=$(limactl shell "$VM" -- bash -c 'echo $HOME')
EB="$GH/pmab"
SRC="$EB/src"
DATA="$EB/data"

echo "==> guest go + dirs ($GH)"
limactl shell "$VM" -- bash -c "
	set -e
	export PATH=\$PATH:/usr/local/go/bin
	if ! command -v go >/dev/null; then
		curl -sSL https://go.dev/dl/go1.25.3.linux-\$(dpkg --print-architecture).tar.gz \
			| sudo tar -C /usr/local -xz
	fi
	go version
	mkdir -p $EB $DATA"

for pair in "$LABEL_A:$DIR_A" "$LABEL_B:$DIR_B"; do
	label=${pair%%:*}
	dir=${pair#*:}
	echo "==> push $label from $dir"
	limactl shell "$VM" -- bash -c "rm -rf $SRC/$label && mkdir -p $SRC/$label"
	tar -C "$dir" --exclude='._*' --exclude=.DS_Store -cf - . \
		| limactl shell "$VM" -- bash -c "tar -C $SRC/$label -xf -"
	limactl shell "$VM" -- bash -c "
		export PATH=\$PATH:/usr/local/go/bin
		cd $SRC/$label && go test -c -o $EB/$label.test ."
done

BENCH='-test.run ^$ -test.bench ^BenchmarkProcessDeletionsReorder$'
ENVV="BENCH_DIR=$DATA BENCH_MAP=$MAP_N BENCH_DELS=$DELS GOMEMLIMIT=200MiB"

echo "==> build map once, uncapped (skipped if BENCH_DIR already holds one)"
limactl shell "$VM" -- bash -c "
	export PATH=\$PATH:/usr/local/go/bin
	ls $DATA/dir.dat >/dev/null 2>&1 && exit 0
	$ENVV $EB/$LABEL_A.test $BENCH -test.benchtime 1x | tail -3"

run() { # label rep
	echo "==> $1 (rep $2, cgroup $CGROUP)"
	limactl shell "$VM" -- bash -c "
		sudo sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches'
		sudo systemd-run --unit=pmab --wait --pipe --collect \
			--property=MemoryMax=$CGROUP --property=MemorySwapMax=0 \
			--property=RuntimeMaxSec=1800 \
			env $ENVV $EB/$1.test $BENCH -test.benchtime ${REPS}x 2>&1 \
			| grep -E 'ns/op|FAIL|panic' || true"
}

# A/B/B/A: run-to-run noise on this host flipped signs before (handoff 3.2).
run "$LABEL_A" 1
run "$LABEL_B" 1
run "$LABEL_B" 2
run "$LABEL_A" 2

echo "==> compare: expected delta is ~0 for base-vs-base; a real gain shows as"
echo "    lower ns/op AND lower majflt/op in B. Repeat once if within noise,"
echo "    and re-run with BENCH_MAP on tmpfs -- order must not matter there."
