# 부하 테스트 진행 가이드 (이어서 하기)

> 자고 일어나서 이거 보고 그대로 따라가면 됩니다.
> 상세 설명은 [`README.md`](./README.md), 배경 결정은 메모리 `loadtest-setup` 참고.

---

## 0. 지금까지 된 것 (현재 상태)

- [x] 부하 테스트기를 `SyncQuote_Infra/loadtest/`로 이동 (두 팀원 안 중 **loadtest_Min 채택**, Beam안 폐기)
- [x] 실제 API/HPA와 전수 대조 — 엔드포인트·파라미터·HPA(`api-server-hpa`, maxReplicas 4) 일치 확인
- [x] `README.md`를 **운영 환경(외부 Windows 11 Docker → WSL2 → DDNS)** 기준으로 갱신
- [x] `web/k6/sse-load-test.js` → **xk6-sse 방식으로 재작성** (기존 http.get 측정은 깨져 있었음)
- [x] `web/k6/Dockerfile` 추가 (xk6-sse 포함 `sq-k6-sse` 이미지)
- [ ] **(미완) xk6-sse 이미지 첫 빌드 + SSE 1회 스모크런** ← 내가 실제 실행은 못 함, 아래 5단계에서 확인
- [ ] **(미완) `SyncQuote_Infra` 레포 커밋** ← 별도 git 레포라 아직 커밋 안 됨

---

## 1. 전제 조건 (한 번만 확인)

클러스터 쪽(노드에 kubectl 되는 환경에서):

```bash
# (1) metrics-server 떠 있나? 숫자 나오면 OK. 없으면 부하 걸어도 Pod 안 늘어남.
kubectl top nodes
kubectl top pods -l app=api
#   안 뜨면 README §1-1 보고 설치(+ --kubelet-insecure-tls)

# (2) HPA 살아있나?
kubectl get hpa api-server-hpa
#   TARGETS에 <unknown>이면 metrics-server 문제

# (3) 데이터 적재됐나? (비어있으면 setup()이 에러로 멈춤)
curl -s http://genti.iptime.org/api/v1/markets | head -c 300
```

부하 거는 외부 머신(Windows 11)에서:
```powershell
docker version    # Docker 동작 확인
```

---

## 2. 외부 머신 준비 (Windows 11)

> ⚠️ **PowerShell 말고 WSL2 셸에서** 돌린다. `--network host`가 정상 동작하고 부하를 최대로 뽑을 수 있다.

```powershell
wsl                 # WSL2(Ubuntu) 진입
```

WSL2 안에서:
```bash
ulimit -n 1048576                       # FD 상한 ↑ (특히 SSE)
cd <SyncQuote_Infra 체크아웃 경로>       # 아래 명령들이 loadtest/... 상대경로를 씀

# 연결 스모크 (200 + JSON 나오면 통과)
curl -s http://genti.iptime.org/api/v1/markets | head -c 300
```

---

## 3. 일반 API / HPA 부하 (메인) — 먼저 이걸로 확인

```bash
# 가볍게 시작 (기본 200 VU, 약 11분)
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  grafana/k6 run -e BASE_URL=http://genti.iptime.org \
  - < loadtest/web/k6/hpa-load-test.js
```

**부하 거는 동안 별도 터미널에서 관찰** (클러스터 쪽):
```bash
watch -n2 'kubectl get hpa api-server-hpa; echo; kubectl top pods -l app=api; echo; kubectl top nodes'
```
- 정상 흐름: CPU% > 70% → ~1분 뒤 REPLICAS **2→3→4** → 부하 종료 5분 뒤 다시 **2**
- Grafana(Pod 수·CPU·지연)도 같이 본다. **공유기 세션 수·집 회선 사용률도 같이 본다** (아래 주의).

**더 세게:**
```bash
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  grafana/k6 run -e BASE_URL=http://genti.iptime.org \
  -e MAX_VUS=400 -e RAMP_UP=2m -e HOLD=6m -e RAMP_DOWN=3m \
  - < loadtest/web/k6/hpa-load-test.js
```

---

## 4. ⚠️ DDNS 경로 주의 (해석할 때 꼭)

경로가 `부하기 → 인터넷 → iptime 공유기(NAT) → 노드 → api Pod`라, **공유기·집 회선이 측정에 포함**된다.

- RPS가 어느 선에서 안 오르고 지연이 튐 → **집 회선 대역폭**이 먼저 한계일 수 있음 (클러스터 아님)
- SSE 동시연결 늘리다 집 인터넷이 끊김 → **공유기 conntrack/세션 테이블 고갈** (클러스터 아님)
- 순수 HPA/클러스터 캐파만 깨끗이 보려면 → 내부(LAN/VPN)에서 `http://<node-ip>:<NodePort>` 직접 호출로 한 번 더 측정해 비교

---

## 5. SSE 부하 (xk6-sse) — **여기가 첫 검증 포인트**

```bash
# (1) 커스텀 이미지 빌드 (최초 1회, 몇 분 걸림)
docker build -t sq-k6-sse loadtest/web/k6

# (2) 짧게 스모크: MAX_CONN 낮게 잡고 동작 확인
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  sq-k6-sse run -e BASE_URL=http://genti.iptime.org -e MAX_CONN=20 \
  - < loadtest/web/k6/sse-load-test.js
```

**스모크에서 확인할 것:**
- 요약에 `sse_opened`, `sse_events`, `sse_time_to_first_event` 가 0이 아니게 찍히면 정상
- `sse_events`가 계속 0 → 스트림이 이벤트를 안 보냄(데이터 적재/Collector 확인) 또는 경로 문제
- 빌드가 실패하면 → 아래 트러블슈팅

**본 부하 (스모크 통과 후):**
```bash
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  sq-k6-sse run -e BASE_URL=http://genti.iptime.org -e MAX_CONN=300 \
  - < loadtest/web/k6/sse-load-test.js
```
> ⚠️ SSE 대량 동시연결은 공유기를 먼저 죽일 수 있다. **작게 시작**하고 공유기 세션 수를 보면서 올린다.
> 동시연결 한계만 깨끗이 보려면 내부에서 노드 직접 호출 권장.

---

## 6. 트러블슈팅

| 증상 | 원인 / 조치 |
|---|---|
| `--network host` 효과 없음 / 성능 낮음 | PowerShell에서 돌림 → **WSL2 셸**에서 실행 |
| 부하 거는데 Pod 안 늘어남 | metrics-server 없음/오류 → §1 (1) 재확인 |
| `setup()`에서 마켓 비어있다고 에러 | Collector 미적재 → 데이터 들어온 뒤 재시도 |
| DDNS 접속 안 됨 (`curl` 실패) | 공유기 포트포워딩(80) / DDNS 갱신 / 외부망 여부 확인 |
| `docker build` (xk6) 실패 | Go 모듈/네트워크 문제 → 재시도, 또는 `phymbert/xk6-sse@latest` 태그 확인 |
| 집 인터넷이 끊김 | 공유기 세션 고갈 → 부하 중단, MAX_CONN 낮추고 내부 직접 호출로 전환 |
| `k6/x/sse` import 에러 | 기본 `grafana/k6`로 SSE 돌린 것 → **반드시 `sq-k6-sse` 이미지** 사용 |

---

## 7. 끝나고 할 일

- [ ] SSE 스모크 결과 정상 확인 (5단계)
- [ ] (보조) 인클러스터 A안 돌려볼지: `kubectl apply -k loadtest/k8s/` (README §3)
- [ ] **`SyncQuote_Infra` 레포 커밋** — loadtest/ 추가분 (별도 레포라 따로 커밋해야 함)
- [ ] 결과 캡처/수치 정리 (Grafana 스크린샷 + k6 요약)

---

### 참고 좌표
- 클러스터: `sq-k8s-01` / `172.16.116.10` (단일 노드, 4 vCPU / 6GB)
- HPA: `api-server-hpa`, minReplicas 2 / **maxReplicas 4** (단일 노드 설계 상한)
- 외부 진입: **`http://genti.iptime.org`** (80)
- 엔드포인트(검증됨): `/api/v1/{markets,tickers,candles}`, SSE `/api/v1/stream/tickers`
