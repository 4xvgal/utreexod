# A 구현/검증 — WAL mmap 캐시 dense packing

측정일 2026-10-20. 브랜치 `lab/a-wal-dense` (기준 `poc/eh-readcoalesce` abb97e42,
계측 코드는 `lab/flushstat-a` 커밋 d36be187에 보존). 라이브러리는 in-repo 사본
`./utreexo-lh`. 테스트/빌드는 Docker `golang:1.25-bookworm` (arm64), regtest는 OrbStack.

## 0. 한 줄

`UTREEXO_WAL_DENSE=1`로 켜는 엔트리 번호 주소지정 dense 캐시를 구현했다. 33.5M 엔트리에서
**48.9 B/entry = 1.53 GiB** (data 32 B + offset 8 B + index ~9 B)로, 실측 앵커
**100 B/entry = 3.36 GB** 대비 **~1.7 GB 회수**가 예상된다. 테스트(양방향 전체 스위트,
`-race`, 33.5M bigmem), regtest(A/B, 재기동 재사용, reorg) 모두 통과. 메인넷 A/B도 실행했다
(§6): 동일 write 집합에서 **anon −0.5~1.0 GB / page cache +0.42 GiB**. 단 창이 5M 엔트리로
묶여(=`--ibdmaxheight=306k` 상태의 한계) 앵커 스케일 −1.7 GB는 modern 높이에서 재측정 필요.

## 1. 게이트 (3.1) — mainnet bytes/entry

`docker volume inspect utreexod-mainnet-v3` → `no such volume`,
`docker inspect cw_utreexod_mainnet` → `no such object`,
`docker ps -a` 에도 없음. 이 OrbStack 컨텍스트에는 mainnet 스택이 없다(regtest 2개 +
이전 계측 컨테이너뿐). 규칙상 새로 IBD를 돌리지 않았고, 확정 수치를 그대로 인용한다.

| 근거 | entries | anon | B/entry | 판정 |
|---|---:|---:|---:|---|
| v5.3 mainnet smaps (확정) | 33.5 M | 3.36 GB | **100** | ≥ 90 → **진행** |

부수 정정(문서/테스트 주석 반영): 낭비 원인은 "행 간격 512 GiB"가 아니다.
`rowBaseOffset`(capBits=34)은 행마다 기하급수로 줄어 row 0 = 549 GB, row 34 = 64 B,
전체가 ~1.1 TB 안에 들어간다. 낭비는 **행 내부 위치 희소성**이다(row 1은 3.46억 leaf일 때
~11 GB인데 한 flush 창의 interior write는 수십만 개).

## 2. 최소 스파이크 (3.2) — `internal/mmapcache/dense.go`

구조: 샤드 64개, 샤드당 `{mu, data [][]byte(청크 4096엔트리), offs [][]uint64, idx []int32}`.
`idx`는 open addressing(선형 탐사, load ≤ 50%, `0`=empty, `slot+1` 저장), 8 B/entry.
data/offs/index는 전부 anon mmap(`MAP_NORESERVE`, demand paging)이라 Go heap 밖에 있고,
`Clear()`는 청크와 index 페이지를 `MADV_DONTNEED`로 커널에 돌려준다(index 길이는 유지 →
flush마다 rehash 반복 없음). 예산: `maxEntries = maxBytes/(entrySize+16)`.
같은 offset 재기록은 count 증가 없이 값만 갱신(journal dedupe 전제 유지).

```
Benchmark (2M ops, count=3, -cpu=1)   dense (ns/op)      sparse (ns/op)    비고
Put seq                                49.9 – 59.7       3.38 – 3.51     둘 다 hot
Put scatter (mainnet 형상)             52.0 – 56.5      110 – 318       dense 2–6x 빠름
Get seq (hit)                          45.6 – 47.1       2.12 – 2.14
```

**+10% 기준은 문자 그대로는 미달**(약 16x). 이유: sparse store의 3.4 ns는 키 조회가 없는
hot bitmap CAS+memcpy이고, dense profile은 90%가 `lookup`(index 페이지 1회 + offset 청크 1회
= 의존적 cache/TLB miss)이다. 인덱스 방식을 바꿔도 3.4 ns에는 못 간다. 절대값으로는
50–60 ns/op = 17–20M writes/s로, mainnet 실측 캐시 write rate(주기 flush 33k, size flush
260k 엔트리 / 수 분)의 4자리수 위 여유 → **캐시 CPU는 flush 시간의 0.1% 미만**.
반대로 실제 비용인 페이지 폴트는 dense가 제거한다(scatter에서 A 대비 2–6x 빠름).
→ 인덱스 방식 교체 없이 채택. (근거: `dense_test.go` bench + `pprof -top`)

## 3. 본 구현 (3.3)

- `internal/mmapcache/dense.go` — 신규 `Dense` (Store와 동일 API: Put/Get/ForEach/Clear/
  Overflowed/Count/Close/EntrySize).
- `cache_mmap.go` — `UTREEXO_WAL_DENSE=1`이면 dense, 아니면 기존 `mmapcache.Store`.
  `cacheImpl`은 그대로 concrete struct(두 포인터 중 하나만 non-nil) → escape analysis 유지.
  엔v는 `sync.OnceValue`로 1회만 읽고, 테스트는 `denseEnabled`를 override.
- `cached_rws.go` — 코드 변경 없음(예산 `entrySize+16`은 `NewDense`가 계산), 주석으로 문서화.
- `wal.go` / `forest.go` — **수정 없음**(API 유지 확인).
- 포맷 변경 없음, 캐시는 휘발성 → 기존 볼륨 그대로 재사용. 기본값 off.
- 의도된 변화: 같은 `--utreexoproofindexmaxmemory`에서 flush가 ~1.5x 자주(32 B 엔트리 기준
  32→48 B/entry). H3의 flush 고정비 ~50 ms 기준으로 mainnet(2.5–5분 주기)에서 무시 가능.

## 4. 테스트 (3.4)

전체 스위트를 **dense off / on 두 번** 돌렸다(동일 바이너리, env만 변경):

```
go test ./...                    (off)  utreexo 81.7s, eh 76.3s, mmapcache 0.4s, swisstable 0.5s  ok
UTREEXO_WAL_DENSE=1 go test ./... (on)  utreexo 67.0s, eh 65.0s, mmapcache 0.4s, swisstable 0.5s  ok
go vet ./...  clean,  gofmt -l  clean,  GOOS=darwin go build ./...  ok
UTREEXO_WAL_DENSE=1 go test -race -run 'TestDense|TestWAL|TestForestNoFlushBeforeWAL|TestCachedRWS|TestCacheStore'
                ok (2.9s)
go test -race ./internal/mmapcache  ok (4.0s)
```

기존 필수 테스트(`TestForestNoFlushBeforeWAL`, `TestWALCrashBeforeCommit`,
`cache_test.go`, `cached_rws_test.go`, `wal_test.go`) 전부 양방향 통과.

신규 (`internal/mmapcache/dense_test.go`):
`TestNewDense`, `TestDensePutGet`(chunk 경계 포함), `TestDensePutWrongSize`,
`TestDensePutOverwrite`(dedupe 계약), `TestDenseForEachOnce`(offset 중복 없이 1회, 마지막 값),
`TestDenseClear`/재사용, `TestDenseBudgetChargesIndex`(48 B/entry 예산 고정),
`TestDenseGrowsPastBudget`(블록 내 버스트: 드롭 없이 chunk/rehash 성장, Count 정확),
`TestDenseConcurrentShards`(16 goroutine + 같은 offset 재기록, `-race`),
`TestDenseRssDensity`(CI 밀도 모델: 200k 엔트리 mainnet 형상 → RSS/entry ≤ 64).

신규 (`wal_dense_test.go`, linux):
`TestDenseCacheBudget`(flush 트리거가 예산 변화와 일치), `TestDenseWALCrashBeforeCommit`
(journal 미완성 → 폐기, 이전 상태 유지), `TestDenseWALForestRecovery`(commit 후 apply 전
kill → replay 후 루트/삭제 비트맵 일치), `TestDenseForestRecordRoundTrip`(record 모드 +
중간 flush + 재오픈), `TestDenseMatchesSparseOnDisk`(동일 시퀀스를 off/on으로 → **pending
(offset,value) 집합 동일** + flush 후 각 offset의 커밋 바이트 동일 = 기능 동일성).

bigmem (`-tags=bigmem`, CI 제외):
`TestDenseRssMainnetShape` — 33,500,000 엔트리(leaf 순차 + interior 산개, 5천만 write):

```
writes=50055986 entries=33500000 Rss=1.53 GiB bytes/entry=48.9 (model 48)
```

dense 목표(32 B/entry) + offset 8 B + index ~9 B = 48.9 B/entry. 회수량 =
3.36 GB − 1.64 GB ≈ **1.72 GB**. (컨테이너 규모 0.2M에서는 1.0–1.2x라 회귀 테스트로는
안 잡히므로, RSS 가드는 `-tags=bigmem`으로 분리하고 CI에는 위 밀도 모델만 남겼다.)

## 5. regtest (3.5)

스택: `poc/docker-compose.regtest-flushstat.yml` (project `flushstat`, `CACHE_MIB=8`,
`UTREEXO_EH_LOAD=0.80`, posmap eh, FLUSHSTAT/WALFLUSH 계측). 이미지 재빌드
(`Dockerfile.lab` → `cw-utreexod-flushstat:poc`). A/B 스위치는 compose에 추가한
`UTREEXO_WAL_DENSE: "${WAL_DENSE:-}"` (기본 off).

| 단계 | 구간 | flush 페이로드 | 엔트리/flush | 비고 |
|---|---|---|---|---|
| A off, 120블록×2000출력 | 449→569 (8m57s) | 11.89 / 11.90 MB | 264 k | 8 MiB/32 B = 262,144 ✓ |
| A off, 재기동 replay 449블록 | 449 | 3 × 11.89 MB | 264 k | 상태 재사용(재다운로드 없음) |
| B on, 재기동 replay 449블록 | 449 | 4 × 7.93 MB | 176 k | 8 MiB/48 B = 174,762 ✓ |
| B on, 120블록×2000출력 | 449→569 (9m02s) | 7.93 / 8.01 / 7.93 MB | 176 k | 주기 ~192 s (A: 295 s) |

- **에러 0** (유일한 WRN: `/root/.utreexod/utreexod.conf` 없음 — 무관).
- **graceful 재사용**: `compose stop` → `up -d` 후 height 유지, forest 재사용(플랫 인덱스만 replay).
- **delete/reorg**: dense on 상태에서 `invalidateblock`(569) → 2블록 추가 마이닝 →
  `REORGANIZE` 569→568→570 정상, reorg flush 1.44 MB 완료, 에러 없음.
- **기능 동일성(off vs on)**: regtest는 마이닝마다 블록이 달라 해시 비교가 무의미하므로,
  동일 시퀀스를 두 캐시로 replay하는 `TestDenseMatchesSparseOnDisk`로 대체(위 §4).
- RSS(컨테이너 규모): A 264 MB / B 274–309 MB (HWM 326 / 439 MB) — 예상대로 0.2M 규모에서는
  차이 없음(둘 다 ~1.0x). 이 스택으로 이득을 증명하지 않는다.

## 6. mainnet A/B (3.6) — 실행함 (스케일 제한)

### 6.1 환경 (새로 구성)

옛 `utreexod-mainnet-v3` 볼륨은 삭제되어 새로 만들었다(사용자 승인). 볼륨/컨테이너/문서:

- 볼륨 `utreexod-a-dense-mainnet`(+ 두 번째 복사본 `utreexod-a-dense-mainnet-b`,
  다른 에이전트용 클론 `utreexod-mainnet-clone-306k`) — **삭제 금지**, `poc/a-dense-volumes.md`.
- 이미지 `cw-utreexod-a-dense:poc` (`Dockerfile.lab`, dense 코드 + FLUSHSTAT/WALSTAT).
- compose `poc/docker-compose.a-dense-mainnet.yml`: `--proofonly --flatutreexoproofindex
  --prune=550 --utreexoproofindexmaxmemory=1024 --utxocachemaxsize=4096 --ibdmaxheight=<H>`,
  posmap `eh`, `mem_limit 11g`, `--connect=192.168.0.55:8333`(utreexo proof 브리지).
- 샘플러 `poc/a-dense-sample.sh` → `poc/a-dense-ab-off.csv`, `poc/a-dense-ab-on.csv`.

**높이 제한 IBD는 가능하다**: `--ibdmaxheight=N`(이미 `config.go`/`netsync/manager.go`에 있음,
lab proof-only 브리지 커밋에서 도입)이 N 초과 블록 연결을 중단한다. 실측: 0→260,000 까지
약 30분, 이어서 306,507까지 진행(`ibdmaxheight`로 정지, 이후 헤더만 받음).

### 6.2 앵커 창을 못 만든 문제 (중요)

| 조건 | WAL flush 창 엔트리 | 페이로드 |
|---|---:|---:|
| `--utxocachemaxsize=1024` | ~4.2 M | 190 MB |
| `--utxocachemaxsize=4096` | ~5.2-5.6 M | 234-251 MB |
| 앵커(=1 GiB WAL 예산 바인딩) | **33.5 M** | ~1.5 GB |

원인: 창은 WAL 예산이 아니라 **UTXO 캐시 flush**가 결정한다(utxo 캐시가 ~1.6 GiB/약 100만
엔트리에서 flush). 2013-2014 블록은 블록당 고유 오프셋이 ~200-500개라 5분(주기 flush) 동안
33.5M에 도달하지 못한다. 계측용 `UTREEXOD_UTXO_FLUSH_INTERVAL`(주기 5m→N) override를
추가했지만 창을 키우는 트리거가 시간이 아니라 UTXO 캐시 크기여서 효과 없음(코드는 유지).
→ 앵커 스케일은 modern 높이(≥600k) 블록 또는 `--prune=0` replay가 필요하다.

### 6.3 A/B 결과 (동일 블록 구간, 동일 플래그, dense off vs on)

같은 상태(296,507)에서 복제한 두 볼륨으로 **양쪽 모두 296,507→306,469**를 처리했다.
flush 페이로드가 양쪽에서 같은 값(예: `WALFLUSH bytes=251502986`)으로 나와 **동일한 write
집합을 처리했음**이 확인된다.

| 지표 (중앙값, 높이 298k-306k) | A (dense off) | B (dense on) | 차이 |
|---|---:|---:|---|
| cgroup anon | 3.81 GiB | 3.38 GiB | **−0.43 GiB** |
| cgroup file (page cache) | 1.89 GiB | 2.31 GiB | **+0.42 GiB** |
| VmRSS | 5.83 GiB | 5.20 GiB | −0.63 GiB |
| majflt/s | 842 | 830 | ±0 (이 규모에선 압력 없음) |

높이 매칭 delta(anon, MB): 298k 724 / 299k 987 / 300k 693 / 301k 794 / 302k 998 /
303k 524 / 304k 891 / 305k 686 / 306k 1001 → **중앙값 724 MB, 범위 0.5–1.0 GB**.

- 예상 모델(창 엔트리 × (100−48.9) B ≈ 284 MB)보다 큰데, sparse store가 추가로 **bitmap
  (1 bit/slot, 4 GiB 영역)** 페이지를 창마다 fault-in 하기 때문이다(≈ +0.5 GB). dense에는
  bitmap이 없다.
- flush 소요 시간은 이번 A/B에서 판정 근거로 쓰지 않았다: B run 중에 볼륨 클론 복사가 겹쳐
  `jsync`가 4 s → 10-11 s로 오염됐다(순수 dense 영향 아님).

### 6.4 판정

- **방향·메커니즘 확인**: 같은 write 집합에서 anon −0.5~1.0 GB, 그만큼 page cache 증가,
  flush 횟수/페이로드 동일(추가 flush 비용 없음). 이 규모에서는 majflt 변화 없음.
- **앵커(−1.5 GB) 검증은 미완**: 창이 33.5M 엔트리여야 하고, 이 데이터(≤306k 높이)로는
  재현 불가. modern 높이에서 같은 A/B를 반복하면 `(100−48.9) B × 33.5M ≈ 1.7 GB`가 기대된다.
- 회수분은 그대로 page cache로 갔고(`file` +0.42 GiB), 압력이 걸리는 규모(mainnet 10 GB
  cgroup, forest 17 GB)에서는 이 차이가 majflt/blk-s로 나타날 것으로 본다.

## 7. 판정

- **구현 채택 (잠정)**. 근거: (a) 33.5M 엔트리에서 48.9 B/entry 실측, 실 mainnet A/B에서
  동일 write 집합에 anon −0.5~1.0 GB / page cache +0.42 GiB,
  (b) 기능 동일성(off/on 커밋 바이트 동일), (c) 전체 스위트 양방향 + `-race` + crash/replay 통과,
  (d) regtest A/B·재기동·reorg 무에러. 남은 조건: §6.2의 앵커 창(33.5M 엔트리)을 만드는
  modern 높이 A/B — 이 박스에서는 260k-306k 상태로 창이 5M에 묶였다.
- 리스크/한계: flush 빈도 +50%(고정비 ~50 ms/회), `ForEach` 방문 순서가 sparse와 다름
  (journal/apply는 순서 무관), dense 캐시는 offset을 정확 키로 취급(비정렬 offset은 miss —
  `cachedRWS`는 정렬 레코드만 쓴다).
- 기각 유지: v5.3a(interior 직접 쓰기), resident-page 캡, 컨테이너 규모 벤치로 이득 증명.

## 8. 재현 명령

```bash
# 테스트 (dense off / on)
docker run --rm -v "$PWD/utreexo-lh":/src -w /src -v utreexo-gomod:/go/pkg/mod \
  golang:1.25-bookworm sh -c 'go test -count=1 ./...'
docker run --rm -e UTREEXO_WAL_DENSE=1 ... (동일)

# 벤치
go test -run xxx -bench 'PutSeq|PutScatter|GetSeq' -benchtime=2000000x ./internal/mmapcache

# bigmem RSS 가드 (33.5M)
go test -tags=bigmem -run TestDenseRssMainnetShape -v ./internal/mmapcache

# regtest A/B
docker compose -p flushstat -f poc/docker-compose.regtest-flushstat.yml up -d   # A: off
python3 poc/flushstat-mine.py -n 120 -o 2000
WAL_DENSE=1 docker compose -p flushstat -f poc/docker-compose.regtest-flushstat.yml up -d  # B: on
python3 poc/flushstat-mine.py -n 120 -o 2000
```

## 9. 이번 세션에서 추가한 계측 (lab 브랜치, 기본값 불변)

- `blockchain/utxocache.go`: `UtxoFlushPeriodicInterval` const → var (측정 빌드가 주기를 바꿀 수
  있게), `blockchain/indexers/utreexobackend.go` init에서 `UTREEXOD_UTXO_FLUSH_INTERVAL` 파싱.
- `poc/docker-compose.a-dense-mainnet.yml`, `poc/a-dense-sample.sh`, `poc/a-dense-volumes.md`,
  A/B 원자료 `poc/a-dense-ab-off.csv` / `poc/a-dense-ab-on.csv`.

## 10. 이번 세션에서 만난 환경 문제

`utreexo-lh/internal/eh/load_sweep_test.go`(untracked, 세션 중 외부에서 생성됨, 이 작업과
무관)가 `off / m.bucketSize()` (int64/uint64) 타입 오류로 `./internal/eh` 빌드를 깨뜨린다.
손대지 않았고, 위 테스트 실행은 `go test -overlay`로 그 파일만 빈 파일로 대체해 돌렸다.
