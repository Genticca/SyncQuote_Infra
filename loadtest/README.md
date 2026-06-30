# SyncQuote 부하 테스트기

> SyncQuote **Query API**(읽기 경로: `api-server` Deployment)에 부하를 걸어
> **HPA의 scale-out/scale-in 동작**과 **서버 용량 한계**를 검증하는 테스트기.
> 기존 인프라 가이드의 kustomize·GHCR·`node-role.kubernetes.io/worker` 컨벤션을 따른다.

이 테스트기의 특징:
- **실데이터 기반 부하**: `setup()`에서 `/api/v1/markets`로 실제 종목을 받아 부하 (하드코딩 X)
- **정확한 엔드포인트**: `/api/v1/tickers`, `/api/v1/markets`, `/api/v1/candles`
- **생성기 최적화**: `responseType:'none'`으로 응답 바디를 버려 더 높은 부하 가능
- **백엔드 경로 분리**: candles를 `1s`(Redis)·`1m`(MySQL)로 섞어 백엔드 부하도 관측
- **SSE 별도 측정**: 스트리밍 부하를 분리 (`sse-load-test.js`)
- **단일 노드 결과 해석 가이드** + **metrics-server 사전 체크** 포함

> ⚠️ 이 테스트기는 **우리가 운영하는 클러스터의 자체 캐파시티 검증용**이다.
> 권한 없는 대상이나 운영 중 실사용자 트래픽이 있는 환경에는 사용하지 말 것.

---

## 0. 두 가지 방향

| 방향 | 부하 발생 위치 | 셋업 | 결과 신뢰도 | 용도 |
|---|---|---|---|---|
| **B. Web 외부 부하** (`web/`) | 별도 테스트기 VM | VM 1대 + k6 | ◎ 측정 대상과 자원 격리 | **메인** — 실사용자 경로, 데모/검증 수치 |
| **A. K8s 자체 부하** (`k8s/`) | 클러스터 안 (Job) | `kubectl apply` | △ 단일 노드면 자원 오염 | **보조** — "HPA가 도는가" 빠른 확인 |

단일 노드 구성에선 **B안을 메인**, A안은 빠른 반복 보조로 쓰는 걸 권장한다.

```
loadtest/
├── README.md                       ← (이 파일)
├── k8s/                            ← 방향 A: K8s 자체 부하
│   ├── 00-namespace.yaml
│   ├── 10-k6-script-configmap.yaml  (인클러스터용 k6 스크립트)
│   ├── 20-k6-job.yaml
│   └── kustomization.yaml
└── web/                           ← 방향 B: 외부 부하 (Windows 11 Docker)
    └── k6/
        ├── hpa-load-test.js         일반 API 부하 (메인, 기본 grafana/k6)
        ├── sse-load-test.js         SSE 동시 커넥션 부하 (xk6-sse 확장 필요)
        └── Dockerfile               SSE용 커스텀 k6 이미지(xk6-sse 포함) 빌드
```

> **운영(부하 발생) 환경**: 외부 **Windows 11 + Docker** 머신에서 DDNS(`http://genti.iptime.org`, 80)로
> 클러스터에 접근해 부하를 건다. 집 공유기(iptime)·회선이 경로에 포함되므로, 결과는
> "실사용자가 외부에서 겪는 end-to-end 부하"에 가깝다(§4 해석 가이드 참고).

---

## 1. 공통 사전 준비 (A·B 둘 다 필요)

### 1-1. metrics-server (HPA 동작 전제) ⚠️ 필수
HPA(`api-server-hpa`)가 CPU를 읽으려면 metrics-server가 떠 있어야 한다.
없으면 부하를 걸어도 **Pod가 안 늘어난다**.

```bash
# 숫자(CPU/MEM)가 나오면 OK
kubectl top nodes
kubectl top pods -l app=api

# 안 떠 있으면 설치
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# ⚠️ kubeadm 자체서명 인증서 환경(이 프로젝트)에서는 TLS 검증 때문에 안 뜰 수 있다.
#    그럴 때만 --kubelet-insecure-tls 추가:
kubectl -n kube-system patch deployment metrics-server --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
kubectl top nodes   # 재확인
```

### 1-2. 외부 노출 주소(BASE_URL) 확인
이 프로젝트의 외부 진입점은 **DDNS `http://genti.iptime.org` (80 포트)**다.
공유기(iptime)가 외부 80 → 노드 Ingress로 포워딩하므로, 부하기에서는 그냥 이 주소를 쓴다.
```bash
# BASE_URL = http://genti.iptime.org   (80은 기본 포트라 :80 생략 가능)

# (참고) 노드 직접 주소가 필요할 때 — 클러스터 내부에서:
kubectl -n ingress-nginx get svc ingress-nginx-controller
# 예) 80:3xxxx/TCP → http://<node-ip>:3xxxx
```

### 1-3. 부하 거는 동안 관찰 (별도 터미널)
```bash
watch -n2 'echo "=== HPA ==="; kubectl get hpa api-server-hpa; \
           echo; echo "=== API PODS ==="; kubectl top pods -l app=api; \
           echo; echo "=== NODE ==="; kubectl top nodes'
```
정상 흐름: CPU% > 70% → ~1분 뒤 REPLICAS 2→3→4 → 부하 종료 5분 뒤 다시 2.

---

## 2. 방향 B — Web 외부 부하 (메인) · Windows 11 Docker

부하 생성기는 **외부 Windows 11 + Docker** 머신에서 돌린다. 측정 대상(클러스터)과
물리적으로 분리돼 자원이 섞이지 않는다.

### 2-1. Windows에서 실행 위치 — WSL2 안에서 돌릴 것 ⚠️
한 대로 부하를 최대한 끌어올리려면 **PowerShell이 아니라 WSL2 셸**에서 docker를 돌린다.
- Docker Desktop은 이미 **WSL2(리눅스 커널)** 위에서 동작 → `--network host`가 정상 동작(NAT 우회).
- PowerShell에서 바로 `docker run` 하면 host 네트워크가 반쪽이라 동시연결·RPS를 짜낼 때 손해.

```powershell
# Windows에서 WSL2 진입
wsl
```

### 2-2. 실행 (WSL2 셸)
```bash
ulimit -n 1048576   # 이 셸 FD 상한 ↑ (특히 SSE 동시연결)

# 일반 API 부하 (기본 200 VU, 11분) — 기본 grafana/k6 이미지로 충분
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  grafana/k6 run -e BASE_URL=http://genti.iptime.org \
  - < loadtest/web/k6/hpa-load-test.js

# 더 강한 부하로 6분 유지
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  grafana/k6 run -e BASE_URL=http://genti.iptime.org \
  -e MAX_VUS=400 -e RAMP_UP=2m -e HOLD=6m -e RAMP_DOWN=3m \
  - < loadtest/web/k6/hpa-load-test.js

# SSE 부하 — xk6-sse 확장이 필요하므로 커스텀 이미지를 먼저 빌드(§2-3)
docker build -t sq-k6-sse loadtest/web/k6
docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
  sq-k6-sse run -e BASE_URL=http://genti.iptime.org \
  - < loadtest/web/k6/sse-load-test.js
```

> 한 머신에선 **컨테이너 1개**면 충분하다. k6는 이미 모든 코어를 쓰므로 여러 개 띄우면
> 서로 CPU를 뺏어 오히려 손해다. 한 대로 부족하면 그때 *여러 머신*에 분산한다.

### 2-3. SSE용 커스텀 k6 이미지 (xk6-sse)
기본 `grafana/k6`에는 SSE 확장(`k6/x/sse`)이 없다. `web/k6/Dockerfile`로 한 번 빌드해 둔다.
```bash
docker build -t sq-k6-sse loadtest/web/k6   # xk6로 xk6-sse 포함 k6 빌드
```
왜 필요한가: 기본 `http.get`은 응답을 끝까지 받아야 끝나는 모델이라, 서버가
`SseEmitter(Long.MAX_VALUE)`로 무한 유지하는 SSE에선 **매번 타임아웃→실패로만 찍혀** 측정이 무의미하다.
xk6-sse는 SSE를 네이티브로 열어 이벤트를 실제 수신하며 연결을 유지하므로 서버 timeout 정책과 무관하게 동작한다.

### 옵션 (`-e KEY=VALUE`)
| 변수 | 기본값 | 설명 |
|---|---|---|
| `BASE_URL` | `http://localhost:80` | 대상 Query API 베이스 URL (운영: `http://genti.iptime.org`) |
| `MAX_VUS` | `200` | (일반) 최대 동시 가상 사용자 수 |
| `RAMP_UP` | `3m` | 0→MAX_VUS 증가 (scale-out 유발) |
| `HOLD` | `5m` | MAX_VUS 유지 (CPU 70% 초과 구간) |
| `RAMP_DOWN` | `3m` | MAX_VUS→0 감소 (scale-in 관찰) |
| `MAX_CONN` | `300` | (SSE) 최대 동시 연결 수 |
| `STREAM_PATH` | `/api/v1/stream/tickers` | (SSE) 스트림 경로 (검증됨, 보통 그대로) |

---

## 3. 방향 A — K8s 자체 부하 (보조)

```bash
# metrics-server 확인 후
kubectl apply -k loadtest/k8s/
kubectl logs -f job/k6-loadtest -n loadtest   # 결과 실시간

# 재실행 (Job은 10분 후 자동 삭제)
kubectl delete job k6-loadtest -n loadtest
kubectl apply -k loadtest/k8s/

# 정리
kubectl delete namespace loadtest
```
기본은 `api-service` 직접 호출(Ingress 우회). Ingress 경유로 바꾸려면
`10-k6-script-configmap.yaml`의 `BASE_URL` 주석 참고.

---

## 4. 결과 해석 가이드 (⚠️ 단일 노드 필독)

이 프로젝트는 **`sq-k8s-01` 1대(4 vCPU / 6GB)** + **`maxReplicas: 4`** 구성이다.

| 관찰 | 올바른 해석 |
|---|---|
| REPLICAS가 4에서 안 늘어남 | `maxReplicas:4` 상한 + 노드 용량 한계. **HPA 실패 아니라 설계 상한.** |
| 응답시간(p95) 급증 | Pod 4개 띄울 자원이 부족 → 스케줄 불가. **노드 용량 병목**이지 앱 결함 아님. |
| HOLD 구간 실패율 상승 | 과부하 정상 신호일 수 있음. Pod 증설 후 다시 안정되는지가 핵심. |
| (A안) 부하 키웠는데 API CPU 안 오름 | 부하기 Pod가 노드 CPU를 먹는 중 → B안(외부)으로 측정 권장. |
| **RPS가 일정 선에서 안 오름 / 지연이 갑자기 튐** | DDNS 경로라 **집 회선 업/다운 대역폭**이 먼저 한계일 수 있음. 클러스터가 아니라 회선 병목. |
| **SSE 동시연결 늘리니 집 인터넷이 끊김** | 가정용 공유기 **conntrack/NAT 세션 테이블** 고갈. 클러스터 한계가 아니라 **엣지(공유기) 한계**. SSE 대량 동시연결은 작게 시작하고 공유기 세션 수를 함께 관찰. |

> **⚠️ DDNS 경로 주의**: `genti.iptime.org`로 외부에서 부하를 걸면 경로가
> `부하기 → 인터넷 → iptime 공유기(NAT) → 노드 → api Pod`라, **공유기·집 회선이 측정에 포함**된다.
> 장점은 "실사용자가 외부에서 겪는 end-to-end 부하"에 가깝다는 것이고, 단점은 클러스터보다
> 공유기/회선이 먼저 병목나면 HPA 거동이 가려질 수 있다는 것이다. 부하 중 Grafana(Pod·CPU·지연)뿐
> 아니라 **공유기 세션 수·회선 사용률**도 같이 본다. 순수 HPA/클러스터 캐파만 깨끗이 보려면
> 내부(LAN/VPN)에서 `http://<node-ip>:<NodePort>` 직접 호출로 한 번 더 측정해 비교한다.
>
> k6 요약(`http_req_failed`)은 PASS/FAIL 판정이 아니라, scale-out 이후 실패율이
> 다시 안정되는지를 보는 **참고 지표**로만 쓴다. 실제 판단은 Grafana(Pod 수·CPU·지연)로.
> 진짜 HA가 필요하면 `kubeadm join`으로 worker 노드를 추가한 뒤 다시 측정.

---

## 5. 실행 전 확인할 것

1. **엔드포인트 경로** — `/api/v1/tickers`, `/api/v1/markets`, `/api/v1/candles`,
   SSE `/api/v1/stream/tickers`까지 실제 Spring 컨트롤러 경로와 일치함(검증 완료).
2. **Collector 적재** — `setup()`이 `/api/v1/markets`로 실데이터를 받으므로,
   데이터가 비어있으면 시작 시점에 에러로 알려준다.
3. **metrics-server** — `kubectl top nodes`가 숫자를 보여줘야 HPA가 동작.
4. **SSE 커스텀 이미지** — `sse-load-test.js`는 xk6-sse 확장이 필요. `docker build -t sq-k6-sse loadtest/web/k6` 선행(§2-3).
5. **(Windows) WSL2에서 실행** — `--network host` 정상 동작과 최대 부하를 위해 PowerShell이 아닌 WSL2 셸에서 docker 실행(§2-1).
