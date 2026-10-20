# A (WAL mmap 캐시 dense packing) 검증

측정일 2026-10-20. `utreexo-lh` (branch `poc/eh-readcoalesce`), Docker `golang:1.25-bookworm`.
검증 대상: "WAL 캐시가 파일 오프셋으로 주소지정돼 interior write가 페이지당 몇 개만 채운다"
→ dense(엔트리 인덱스) 패킹으로 GB급 회수.

## 0. 결론

**방향은 맞고 크기도 유효하다. 다만 handoff의 2.5–6 GB는 모델 과대추정이고,
실측 앵커 기준 회수량은 ~2.3 GB (낙관) ~ 9.5 GB (비관), 중심값 ~2–3 GB.**

| 근거 | entries | bytes/entry | waste | 회수 가능량 |
|---|---:|---:|---:|---:|
| 실제 mainnet smaps (v5.3, 33.5M) | 33.5 M | **100 B** | 3.2× | **2.3 GB** |
| 모델(균일 위치, 34.4M) | 34.4 M | 310 B | 9.7× | 9.5 GB |
| 모델(균일 위치, 3.44M) | 3.44 M | 522 B | 16.3× | — |
| 실측 컨테이너 (실제 forest, 170 k) | 0.17 M | 36 B | **1.1×** | 0 |
| 실측 컨테이너 (실제 forest, 80 k) | 0.08 M | 34 B | **1.0×** | 0 |

→ dense 쪽 비용은 32 B/entry 고정. 즉 **회수량 = (측정 B/entry − 32) × entries − 인덱스 비용**.
인덱스 8–16 B/entry × 33.5M = 0.27–0.55 GB → 순이득 **~1.8–2.7 GB (실측 앵커)**.

## 1. 검증한 것 / 못 한 것

**검증됨**
- 컨테이너 규모에서는 낭비가 **없다** (1.0–1.2×). 실제 forest(record 모드 + RehashAndProve,
  2000 adds/1500 dels × 40블록)에서 `entries/page = 126.9`, `34 B/entry` — dense 이론값 128/page와
  일치. 즉 "dense로 가면 32 B/entry"는 **측정으로 확인**됐다.
- mainnet 규모(위치 span 3.46억)에서는 낭비가 **분명히 존재**한다: 위치를 균일 샘플링하는
  모델에서 interior write는 페이지당 ~1개, 집계 density 10–32%.
- 낭비는 **엔트리 수/위치 span에 비례**한다: 0.17M → 1.1×, 3.4M → 16×, 34M → 9.7× (모델),
  실측 33.5M → 3.2×.

**못 한 것**
- mainnet 실측(3.36 GB anon)을 여기서 재현하는 것. 이 호스트는 12.6 GB RAM에 맵/forest를
  다 올릴 수 있어 33.5M 엔트리 규모의 실제 분포를 만들 수 없다(H2/H3와 같은 한계).

## 2. handoff 서술 중 틀린 부분 (수정)

> "포레스트는 행마다 별도 구역이고 행 간격이 512 GiB다(`rowBaseOffset`)"

실제 `rowBaseOffset`(forest.go:43)은 `capBits = min(forestRows, 34)`:

```
row r base = (2^35 - 2^(34-r+1)) × 32
row 0 = 549 GB, row 1 = 824 GB, ... row 34 = 64 B
```

즉 **행 간격은 512 GiB가 아니라 기하급수적으로 줄어들며, 전체 파일이 1.1 TB 안에 다 들어간다**
(측정에서도 row 0..24 span = 1024 GiB로 확인). 낭비의 원인은 "행 사이의 빈 공간"이 아니라
**행 내부에서 위치가 성기게 흩어지는 것**이다: row 1은 3.46억 leaf일 때 11 GB(페이지 270만)인데
한 flush 창의 interior write는 수십만 개뿐 → 페이지당 몇 개만 채워진다.

이 수정은 A의 결론을 바꾸지 않는다(여전히 GB급) — 다만 **이유**와 **확장 스케일**이 다르다.

## 3. 남은 판단

| 항목 | 값 |
|---|---|
| 순이득 (실측 앵커) | **~1.8–2.7 GB** (모델 앵커면 최대 9 GB) |
| 회수분 용도 | 그대로 page cache → `forest_data.dat`(21 GB, 상주 1.3 GB)/posmap의 major fault 감소 |
| 구현 비용 | `internal/mmapcache` 내부만 교체(Store API 유지), offset→slot 인덱스 0.27–0.55 GB |
| 리스크 | 현재 lock-free(atomic bitmap CAS) → dense append는 샤딩 락 필요. serializeEntries/journal replay/Clear/ForEach/Overflowed 전부 재검증 |
| 기각된 변형 | v5.3a(interior를 journal 밖으로) — `TestForestNoFlushBeforeWAL`, `TestWALCrashBeforeCommit` 위반 |

## 4. 계측 코드 (이 리포에 추가됨)

- `utreexo-lh/wal_density_test.go` — 실제 forest(record 모드)로 WAL 캐시 밀도 측정.
  smaps에서 1 TB anon 영역 Rss를 읽고, 같은 엔트리 수를 dense 재생해 비교.
- `utreexo-lh/wal_density_mainnet_test.go` — mainnet 형상(3.46억 위치, leaf 순차 + 삭제는
  전 역사에서 균일, 조상 24단계) 오프셋을 **실제 `mmapcache.Store`**에 넣어 Rss 측정.
  (`rowBaseOffset`/`detect_row` 수식은 `prof/density.py`와 동일)

```
docker run --rm --memory=8g -v "$PWD/utreexo-lh":/src -w /src \
  -v utreexo-gomod:/go/pkg/mod golang:1.25-bookworm \
  go test -run 'TestWALCacheDensity' -v -count=1 .
python3 prof/density.py 2000000 1620000     # mainnet 창 규모 모델(≈38 s)
```

## 5. 한 줄

**A는 검증됐다** — dense로 바꾸면 32 B/entry가 되는 것까지 측정했고, 실제 mainnet 앵커(100 B/entry)로
**~2.3 GB 회수**가 기대된다. 단 (a) 컨테이너 규모에서는 효과가 0이라 회귀 테스트로 잡을 수 없고,
(b) "행 간격 512 GiB"라는 설명은 틀렸으며 원인은 행 내부 위치 희소성이다.
