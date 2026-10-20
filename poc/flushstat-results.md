# H3 계측 결과 — flush 정지의 구성

측정일 2026-10-20. 브랜치 `poc/eh-readcoalesce` + FLUSHSTAT 계측(아래 "계측 코드").
환경: OrbStack 12 vCPU/12.6 GB, Docker, bitcoind 30.2 regtest, utreexod `--flatutreexoproofindex
--proofonly --prune=550 --utreexoproofindexmaxmemory=8`, posmap `eh`.
스택: `poc/docker-compose.regtest-flushstat.yml` (전용 볼륨/컨테이너 이름).

## 0. 한 줄

flush 정지의 **CPU 부분은 싸고 예측 가능**하다(11.7 MB 페이로드 → 총 69 ms).
mainnet의 47.8–58 s는 **apply 단계의 랜덤 write I/O**이며, regtest로는 재현되지 않는다
(포지션맵에서 캐시 구간을 못 만드는 것과 같은 이유). 따라서 "flush 쪼개기"는
처리량 이득이 아니라 **꼬리 정지(latency) 이득**만 준다.

## 1. 측정값 (n=8 flush)

| 트리거 | WAL 바이트 | 엔트리≈ | total | drain | flatSync | mainDB | utreexo |
|---|---:|---:|---:|---:|---:|---:|---:|
| periodic | 90 | 2 | 30 ms | 0 | 15 ms | 9 ms | 5 ms |
| periodic | 684,613 | 15 k | 50 ms | 0 | 15 ms | 3 ms | 31 ms |
| periodic | 1,486,460 | 33 k | 78 ms | 0 | 18 ms | 5 ms | 54 ms |
| periodic | 1,776,989 | 39 k | 67 ms | 0 | 11 ms | 5 ms | 51 ms |
| periodic | 1,442,329 | 32 k | 48 ms | 0 | 11 ms | 3 ms | 34 ms |
| periodic | 1,442,149 | 32 k | 59 ms | 0 | 13 ms | 5 ms | 41 ms |
| periodic | 1,442,284 | 32 k | 78 ms | 0 | 33 ms | 5 ms | 40 ms |
| **size (8 MiB)** | **11,714,485** | **260 k** | **69 ms** | 0 | 11 ms | 3 ms | 55 ms |

WAL 내부 분해 (serialize / journal write / journal fsync / apply / file fsync):

| 바이트 | ser | jwrite | jsync | apply | fsync | total |
|---:|---:|---:|---:|---:|---:|---:|
| 90 | 0 | 1 ms | 0 | 4 ms | 0 | 4 ms |
| 1.48 MB | 6 ms | 2 ms | 14 ms | 26 ms | 0 | 48 ms |
| 11.7 MB | 14 ms | 6 ms | 15 ms | 13 ms | 0 | 48 ms |

## 2. 해석

1. **갱신 가능한 비용은 serialize + journal write**: 합계 ~1.7 GB/s (11.7 MB → 20 ms).
   1 GiB 외삽 시 ≈ 1.3 s + 0.5 s.
2. **fsync는 고정 비용**: journal fsync 14–21 ms, flatSync(7개 파일) 11–33 ms,
   mainDB 3–9 ms. 페이로드와 무관 → flush 1회당 **~50 ms 고정 overhead**.
3. **apply는 여기서 바이트에 비례하지 않는다**: 1.4 MB에서 5–26 ms, 11.7 MB에서 13 ms.
   대상 파일이 page cache에 다 들어 있어 syscall 비용만 남기 때문(~900 MB/s).
   mainnet에서 apply가 40 s+인 것은 17 GB `forest_data.dat` + 10 GB cgroup에서
   **랜덤 write가 page cache를 넘어설 때** 발생한다 — regtest로 재현 불가.
4. **drain=0**: 이 속도에서는 proof pipeline이 계속 비어 있다. mainnet의 58 s 중
   drain 몫은 이 계측으로 분리되지 않는다.
5. **트리거 2개**: `--utreexoproofindexmaxmemory=8` → 캐시 엔트리 262,144개(≈11.7 MB 직렬화)에서
   size 트리거. 그보다 짧은 간격이면 5분 주기(`UtxoFlushPeriodicInterval`)가 먼저 걸려
   페이로드가 1.4 MB 수준에 머문다. 즉 **주기 flush의 페이로드는 캐시 예산이 결정**한다.

## 3. H3 판정

- flush를 8 × 128 MiB로 쪼개면: 총 시간은 그대로(같은 바이트를 같은 I/O로 씀) +
  고정 overhead 7 × 50 ms ≈ **350 ms 추가**, 대신 최장 정지는 ≈ 1/8.
  → **처리량 이득 ~0, 꼬리 지연 이득 큼.**
- 처리량을 올리려면 flush를 쪼개는 게 아니라 **쓸 바이트를 줄여야** 한다
  (= handoff 후보 A: WAL mmap cache dense packing, 밀도 3–12% → 2.5–6 GB 회수).
- 규칙 하나 추가 발견: 페이로드는 "블록 크기"가 아니라 **고유 오프셋 수**로 결정된다
  (캐시가 파일 오프셋 키 맵이라 같은 노드 반복 write는 합쳐짐). 2000출력 블록 120개를
  넣어도 주기 flush 페이로드는 1.44 MB에서 포화했다.

## 4. 계측 코드 (측정 전용, 브랜치에만 존재)

- `blockchain/indexers/utreexobackend.go` — `FlatUtreexoProofIndex.Flush`에
  drain / flatSync / mainDB / utreexo 구간 타이머 + `FLUSHSTAT` 로그
- `utreexo-lh/wal.go` — `UTREEXO_WALSTAT=1`일 때 `WALFLUSH bytes=… ser/jwrite/jsync/apply/fsync`
- `config.go` — `UTREEXOD_ALLOW_SMALL_PROOFINDEX_CACHE=1`이면 `--utreexoproofindexmaxmemory` 하한 250 → 8 MiB
- `poc/docker-compose.regtest-flushstat.yml`, `poc/flushstat-mine.py`

재현:

```
docker build -f Dockerfile.lab -t cw-utreexod-flushstat:poc .    # DOCKER_BUILDKIT=0 if buildx state is blocked
docker compose -p flushstat -f poc/docker-compose.regtest-flushstat.yml up -d
python3 poc/flushstat-mine.py -n 120 -o 2000
docker logs cw_utreexod_flushstat 2>&1 | grep -E 'FLUSHSTAT|WALFLUSH'
```

`-o` 출력은 오프라인 생성 bech32 주소를 재사용한다(지갑 소유 불필요, 주소 생성이 병목이 됨).
