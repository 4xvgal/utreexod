# H2 (posmap 프리페치) A/B 결과

측정일 2026-10-20. 브랜치 `poc/eh-readcoalesce` + H2 계측 코드.
환경: OrbStack VM 12 vCPU / 12.6 GB, Docker `golang:1.25-bookworm`,
EH 맵 40M 엔트리 = **ctrl 1.39 GB** (27.9 B/entry) + dir 4.7 MB, `BENCH_DEL=8192`,
`--memory=256m`(웜 구간) / `512m`(콜드 시도), 지표 `ns/op`, `majflt/op`, `minflt/op`.

## 0. 한 줄

웜 캐시 구간에서 프리페치는 **−15%** (33.8/34.2 ms → 38.9/40.5 ms,
`minflt/op` 134 → 134 로 동일, `majflt/op` 0). 즉 순수 syscall 오버헤드만 남는다.
정작 필요한 콜드 구간은 **이 환경에서 만들 수 없다**(아래 2). → **H2 탈락**.

## 1. 웜 구간 (A/B/B/A, 20x)

| run | 프리페치 | ns/op | majflt/op | minflt/op |
|---|---|---:|---:|---:|
| A1 | – | 33.79 ms | 0 | 133.8 |
| B1 | ✓ | 38.91 ms | 0 | 133.3 |
| B2 | ✓ | 40.55 ms | 0 | 134.2 |
| A2 | – | 34.19 ms | 0 | 136.2 |

- 차이 +5 ms/iteration = `madvise` 8192회 × ~0.6 µs. 정확히 syscall 비용.
- 페이지 폴트 수가 같다 = 읽기 순서를 바꿔도 얻을 게 없다(이미 웜).

## 2. 콜드 구간을 못 만든 이유 (중요)

1. `FADV_DONTNEED`는 **매핑된 페이지를 드롭하지 않는다** → 벤치가 같은 프로세스에서
   도는 한 첫 반복 이후로는 항상 웜. (`DropCache()`만으로는 효과 없음)
2. `munmap → fsync → FADV_DONTNEED → remap`(`ResetCache`)까지 해도 `majflt/op = 0`.
   파일이 OrbStack VM 디스크(= macOS의 APFS 파일) 위에 있어, VM 캐시를 비워도
   **macOS 페이지 캐시가 받아준다**. 즉 이 환경의 "디스크 읽기"는 실제 I/O가 아니다.
3. 실제 콜드가 관측된 건 **맵을 막 빌드한 직후의 한 번뿐**:
   `5x, BENCH_DROP` → **360 ms/op, 586.8 majflt/op, 6128 minflt/op**.
   이게 우리가 원하는 구간인데 재현이 안 된다.
4. 콜드 구간을 확실히 만들려면 맵이 **VM RAM(12.6 GB)보다 커야** 한다
   → 12.6 GB / 27.9 B ≈ **4.5억 엔트리**, 현 빌드 속도(40M/4m48s)로 **약 55분 + 12 GB 디스크**.
   또는 mainnet(10 GB cgroup, forest 17 GB)에서만 자연 발생.

## 3. 부수 발견: 벤치 맵 파일이 더러워지면 결과가 오염된다

같은 `BENCH_DIR`를 여러 코드 버전/중단된 런이 공유하면 **tombstone이 파일에 남는다**
(측정 중 `tomb=7116`, `count=40,000,000`). 그 상태에서 반복하면
`processDeletions: delhash ... not found in position map`로 벤치가 죽는다
(delete→restore 라운드트립 자체는 정상: `TestEHDeleteThenSetReinserts` 20k 엔트리 PASS).
→ **A/B 세션마다 `BENCH_DIR`를 새로 빌드**해야 한다. `poc/bench-posmap-ab.sh`는
`$EB/data`를 재사용하므로, 재현 시 `rm -rf` 후 다시 빌드할 것.

## 4. 판정

- 웜: −15% → 손해. 콜드: 측정 불가.
- 메커니즘상 프리페치가 이길 수 있는 조건은 "정렬된 연속 접근의 간격 < readahead 창"
  (EH는 1.39 GB / 51.8만 버킷 = 평균 간격 ~86 KB, 128 KB 창에 아슬아슬).
  이 조건은 **실제 느린 디스크 + 커널 readahead**에서만 확인 가능하고, 여기선 확인 불가.
- 따라서 **H2는 보류**(탈락 아님, 판정 불가). 지금 환경에서 결론을 내려면 4.5억 엔트리
  맵을 한 번 빌드해 두고 A/B 하는 방법뿐이며, 그 비용(≈1시간 + 12 GB)은
  handoff 후보 A(WAL 캐시 dense packing, 회수 2.5–6 GB)보다 우선순위가 낮다.

## 5. 계측 코드

- `internal/eh/eh.go` — `Prefetch()` (FADV_WILLNEED, 같은 페이지 dedupe),
  `ResetCache()` (munmap+sync+DONTNEED+remap, 벤치 전용)
- `internal/eh/fadvise_linux.go` / `fadvise_other.go` — `adviseWillNeed`
- `positionmap_eh.go` — `ehPM.Prefetch` / `ehPM.ResetCache`
- `forest_record.go` — `UTREEXO_PREFETCH=1`일 때 정렬된 순서로 프리페치
- `posmap_delete_bench_test.go` — `BENCH_DROP`, `BENCH_RESET`
- `internal/eh/reinsert_test.go` — delete→Set→Get 라운드트립 회귀 테스트

재현:

```
docker volume create pmmap
docker run --rm -v "$PWD/utreexo-lh":/src -w /src -v utreexo-gomod:/go/pkg/mod \
  -v pmmap:/bench golang:1.25-bookworm go test -c -o /bench/pm.test .
# 맵 1회 빌드 (캡 없이)
docker run --rm -v pmmap:/bench -e BENCH_DIR=/bench -e BENCH_MAP=40000000 \
  golang:1.25-bookworm /bench/pm.test -test.run '^$' \
  -test.bench '^BenchmarkProcessDeletionsReorder$' -test.benchtime 1x
# A/B
docker run --rm --memory=256m --memory-swap=256m -v pmmap:/bench \
  -e BENCH_DIR=/bench -e BENCH_MAP=40000000 -e BENCH_DELS=8192 -e GOMEMLIMIT=180MiB \
  golang:1.25-bookworm /bench/pm.test -test.run '^$' \
  -test.bench '^BenchmarkProcessDeletionsReorder$' -test.benchtime 20x
# B는 위에 -e UTREEXO_PREFETCH=1 추가
```
