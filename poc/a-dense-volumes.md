# A(dense WAL cache) 메인넷 측정용 볼륨 — 인수인계 노트

작성 2026-10-20. 이 박스(OrbStack, 12 vCPU/12.6 GB)에서 만든 메인넷 상태 볼륨들.
**삭제하지 말 것.** 다른 에이전트가 복사/재사용할 수 있게 최소 1개는 남긴다.

## 볼륨 목록

| 볼륨 | 높이 | 상태 | 용도 |
|---|---:|---|---|
| **`utreexod-mainnet-clone-306k`** | **306,507** | 유휴(복사 대기용) | **다른 에이전트용 클론** (원본과 바이트 동일, 독립 볼륨) |
| `utreexod-a-dense-mainnet` | 306,507 | 유휴 | A(dense off) run이 멈춘 상태 |
| `utreexod-a-dense-mainnet-b` | 296,507 (replay 후 증가) | B(dense on) run이 사용 | A/B 비교용 두 번째 복사본 |

높이 근거: `forest_blockcounts.dat` = 1,226,028 B = 306,507 × 4 B (블록당 add count 1개).
`forest_meta.dat` md5는 원본과 동일(클론 무결성 확인).

## 클론 내용/크기

```
/data/mainnet/utreexostate_flat/   forest_data.dat(1 TiB sparse), forest_blockcounts/covered/…
                                   forest_deleted.dat, forest_meta.dat, eh-ctrl.dat, eh-dir.dat (posmap=eh)
/data/mainnet/blocks_ffldb         1.2 GB  (prune=550 MiB → 최근 ~1.8k 블록만)
절대 크기 1.1 TiB (sparse), 실제 사용 24 GB
```

## 쓰는 법

이미지: `cw-utreexod-a-dense:poc` (`Dockerfile.lab`로 이 트리에서 빌드: dense WAL 캐시 + FLUSHSTAT/WALSTAT 계측).
compose: `poc/docker-compose.a-dense-mainnet.yml` — 볼륨은 `VOLUME_NAME`으로 선택.

```bash
# 클론으로 기동 (dense off)
VOLUME_NAME=utreexod-mainnet-clone-306k IBD_MAX_HEIGHT=306507 \
  docker compose -p a-dense -f poc/docker-compose.a-dense-mainnet.yml up -d

# dense on
VOLUME_NAME=utreexod-mainnet-clone-306k WAL_DENSE=1 IBD_MAX_HEIGHT=306507 \
  docker compose -p a-dense -f poc/docker-compose.a-dense-mainnet.yml up -d

# 정지 (graceful만: 10m grace 동안 flat forest flush)
docker compose -p a-dense -f poc/docker-compose.a-dense-mainnet.yml stop
```

측정 플래그/환경(현재 파일 그대로):
`--proofonly --flatutreexoproofindex --prune=550 --utreexoproofindexmaxmemory=1024
--utxocachemaxsize=4096 --ibdmaxheight=<H>`, `UTREEXOD_POSMAP=eh`, `mem_limit 11g`,
`--connect=192.168.0.55:8333`(utreexo proof 브리지; 컨테이너에서는 도달 확인됨),
`UTREEXO_WALSTAT=1`, `UTREEXO_WAL_DENSE`, `UTREEXOD_UTXO_FLUSH_INTERVAL`(계측용, 기본 5m).

샘플러: `bash poc/a-dense-sample.sh <초> <csv> [컨테이너]` → 높이/`VmRSS`/cgroup anon·file/
majflt/minflt/`/proc/1/io`. (`/proc/1/smaps`로 WAL 캐시만 분리하는 건 불가: 1 TiB anon 매핑이
Go heap 예약과 VMA 병합된다 → **총 cgroup anon** 기준으로 비교할 것.)

## 이 높이(306k)의 한계 — 중요

- WAL cache flush 창은 **UTXO 캐시 flush가 결정**한다(`--utxocachemaxsize=4096`에서 utxo 캐시가
  ~1.6 GiB/약 100만 엔트리에서 flush) → WAL 창 ≈ **5M 엔트리, 페이로드 ~250 MB**.
- 그래서 이 상태로는 handoff의 앵커 조건(**33.5M 엔트리 / anon 3.36 GB**)이 재현되지 않는다.
  2013-2014 블록은 블록당 고유 오프셋이 ~200-500개라 5분(주기 flush) 동안 33.5M에 못 미친다.
- 앵커 스케일을 보려면: modern 높이(≥600k) 블록으로 IBD를 더 진행하거나, prune을 풀어
  (`--prune=0`) flat index catch-up replay가 33.5M 엔트리를 채우게 해야 한다.
- 기대 이득은 창 크기에 비례: `(100 − 48.9) B × 창 엔트리 수` → 이 높이에선 ~250 MB,
  앵커 창이면 ~1.7 GB.
