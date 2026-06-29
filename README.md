# Bitcoin & 환율 실시간 모니터링 대시보드 — Kubernetes 인프라 가이드

> **이 문서는 인프라 비전공자도 이해할 수 있도록 작성된 K8s 배포 가이드입니다.**  
> Spring Boot WAS 기반의 실시간 비트코인 및 환율 모니터링 서비스를 Kubernetes 위에 배포하는 전체 과정을 다룹니다.

---

## 목차

1. [전체 서비스 아키텍처 흐름도](#1-전체-서비스-아키텍처-흐름도)
2. [컴포넌트별 상세 역할 및 파일 가이드](#2-컴포넌트별-상세-역할-및-파일-가이드)
3. [배포 및 구동 순서](#3-배포-및-구동-순서)
4. [배포 전 필수 체크리스트](#4-배포-전-필수-체크리스트)
5. [배포 후 상태 확인](#5-배포-후-상태-확인)
6. [트러블슈팅](#6-트러블슈팅)
7. [인프라 설계 핵심 요약](#7-인프라-설계-핵심-요약)
8. [용어 사전](#8-용어-사전)
9. [갱신 이력](#9-갱신-이력)

---

## 1. 전체 서비스 아키텍처 흐름도

사용자가 브라우저를 통해 접속해서 데이터가 수집되고 적재되기까지의 흐름은 다음과 같습니다.

```plaintext
[외부 사용자/클라이언트]
       ⬇️ (도메인 주소로 접속: 포트 80)
┌────────────────────────────────────────────────────────┐
│ 1. Ingress (지하철 게이트: 외부 트래픽을 도메인별로 분류)      │
└───────────────────────⬇────────────────────────────────┘
┌────────────────────────────────────────────────────────┐
│ 2. API Service (안내데스크: 트래픽을 가용한 Pod로 분산)       │
└───────────────────────⬇────────────────────────────────┘
┌────────────────────────────────────────────────────────┐
│ 3. API WAS Pod A, B (실제 일꾼: Replicas 2개로 분산 처리)  │
└───────────⬇─────────────────────────────⬇──────────────┘
    (실시간 시세 캐싱 조회)               (영속 데이터 조회)
┌───────────────────────────┐     ┌──────────────────────┐
│ 4. Redis API (메모리 캐시)  │     │ 5. MySQL (외부 DB)   │
└───────────────────────────┘     └──────────────────────┘
                                             ⬆ (수집 데이터 적재)
┌───────────────────────────┐     ┌──────────────────────┐
│ 6. Redis Collector        │ ⬅── │ 7. Collector (수집기) │
│    (수집 전용 메모리 캐시)   │     │    (싱글톤 구동, 1개) │
└───────────────────────────┘     └──────────────────────┘
```

### VM 구성 (VMware Workstation)

| VM 이름 | 역할 | 배포되는 Pod |
|---------|------|------------|
| **Control Plane VM** | K8s 클러스터 마스터 노드 / 전체 Pod 조율 | 없음 (관리 전용) |
| **Web VM (Node-Worker-02)** | 사용자 트래픽 진입 / API 서빙 | Nginx Ingress, API Pod A/B |
| **Collector VM (Node-Worker-01)** | 거래소 데이터 수집 | Collector Pod |
| **Redis VM** | 캐시 저장소 | Redis-API Pod, Redis-Collector Pod |
| **MySQL VM** | 영구 데이터 저장 | MySQL Server (K8s 외부) |

### 물리 서버 스펙

```plaintext
CPU     : Intel Ultra 5 Gen.13
Memory  : 32GB
Storage : 256GB
OS      : Ubuntu Server 24.04.03
VM      : VMware Workstation Full 26H1
K8s     : v1.32.2
```

---

## 2. 컴포넌트별 상세 역할 및 파일 가이드

클러스터에 배포되는 매니페스트 파일들은 보안성 향상과 자원 고립화를 위해 엄격하게 분리되어 있습니다.

### 파일 구조

```plaintext
k8s/
├── secret.yaml                 # 민감 정보 (DB/Redis 비밀번호)
├── network-policy.yaml         # Redis 접근 제어 방화벽
├── mysql-service.yaml          # 외부 MySQL VM 연결 Service + Endpoints
├── redis-api.yaml              # API 전용 Redis 배포 + Service
├── redis-collector.yaml        # Collector 전용 Redis 배포 + Service
├── collector-deployment.yaml   # 데이터 수집기 Pod 배포
├── api-deployment.yaml         # API 서버 Pod 배포
├── api-service.yaml            # API Pod 앞단 Service (Ingress 연결용)
├── ingress.yaml                # Nginx Ingress 라우팅 설정
├── hpa.yaml                    # API Pod 오토스케일링 설정
└── pdb.yaml                    # API Pod 최소 가용성 보장
```

---

### 2.1 보안 및 데이터 파이프라인 영역

#### `secret.yaml` — 보안 시크릿

- **역할**: 데이터베이스(MySQL) 및 캐시(Redis) 접속에 필요한 비밀번호를 안전하게 보관합니다.
- **특징**: 보안 가이드에 따라 파일 내 모든 비밀번호는 Base64로 인코딩되어 있으며, Pod 구동 시 환경변수로 안전하게 주입됩니다.
- **주의**: 가장 먼저 배포되어야 하며, **절대 Git에 커밋하지 마세요.**

```bash
# .gitignore에 반드시 추가
echo "k8s/secret.yaml" >> .gitignore
```

| Key | 용도 |
|-----|------|
| `mysql-password` | MySQL 접속 비밀번호 |
| `redis-password` | Redis 인증 비밀번호 |
| `external-api-key` | 거래소 API 인증 토큰 |

```bash
# base64 인코딩 방법
echo -n '실제비밀번호' | base64

# base64 디코딩 방법 (확인용)
echo 'base64값' | base64 --decode
```

---

#### `network-policy.yaml` — 네트워크 방화벽 정책

- **역할**: 클러스터 내부의 Pod 간 통신 경로를 제한하는 보안 울타리입니다.
- **특징**: 외부에서 Redis나 DB로 직접 접근하는 것을 차단하고, 오직 허가된 애플리케이션만 지정된 포트로 연결할 수 있도록 화이트리스트 방식으로 제어합니다.

```plaintext
redis-api Pod:
  api Pod       → 접근 허용 (포트 6379)
  collector Pod → 접근 차단
  그 외 모든 Pod → 접근 차단

redis-collector Pod:
  collector Pod → 접근 허용 (포트 6379)
  api Pod       → 접근 차단
  그 외 모든 Pod → 접근 차단
```

---

### 🌐 2.2 웹 서비스 및 라우팅 영역 (사용자 요청 처리)

#### `ingress.yaml` — 인그레스 컨트롤러

- **역할**: 외부에서 들어오는 공용 웹 트래픽(HTTP)을 맞이하는 시스템의 최앞단 '문지기'입니다.
- **특징**: 대용량 데이터 업로드 시 차단되는 것을 방지하기 위해 파일 제한 마진(`proxy-body-size: 10m`)이 설정되어 있으며, 트래픽을 `api-service`로 포워딩합니다.

```plaintext
포트 흐름:
사용자 (도메인 접속)
  ↓
Ingress (api.yourdomain.com → 80)
  ↓
api-service (port: 80)
  ↓
API Pod (containerPort: 80)
```

> **배포 전 반드시** `api.yourdomain.com`을 실제 도메인으로 변경하세요.  
> IP 직접 접근 시 `host:` 라인을 삭제하세요.

---

#### `api-service.yaml` — API 서비스

- **역할**: 고정된 내부 단일 접속점(ClusterIP)을 제공하여, 인그레스로부터 온 트래픽을 실제 일꾼인 API Pod들로 균등하게 나누어 줍니다(로드밸런싱).
- **타입**: `ClusterIP` — 클러스터 내부에서만 접근 가능 / 외부 직접 접근 불가

---

#### `api-deployment.yaml` — API 애플리케이션 배포

- **역할**: 사용자에게 대시보드 화면을 서빙하고 REST API를 처리하는 스프링 부트(Spring Boot) 서버를 구동합니다.
- **특징**: 단일 장애점(SPOF)을 방지하기 위해 서버 2개(`replicas: 2`)를 동시에 띄우며, 서버가 정상 구동되었는지 감시하는 헬스체크 룰(liveness/readiness Probe)이 내장되어 있습니다.

| 설정 | 값 | 이유 |
|------|-----|------|
| `replicas` | 2 | 단일 장애점 방지 (Pod A, B) |
| `SPRING_PROFILES_ACTIVE` | api | API 모드로 Spring Boot 기동 |
| `REDIS_URL` | redis-api-service | API 전용 Redis만 연결 |
| `memory requests/limits` | 256Mi / 256Mi | Guaranteed QoS 등급 확보 |
| `liveness path` | /actuator/health/liveness | Spring Boot 2.3+ 표준 |
| `readiness path` | /actuator/health/readiness | Spring Boot 2.3+ 표준 |

> HPA에 의해 CPU 70% 초과 시 최대 5개까지 자동 확장됩니다.

---

### 2.3 데이터 수집 및 아카이빙 영역

#### `collector-deployment.yaml` — 데이터 수집기 배포

- **역할**: 외부 비트코인 시세 API(Upbit/Binance) 등에서 실시간으로 데이터를 퍼오는 수집기 애플리케이션을 구동합니다.
- **특징**: 동일한 수집기가 여러 개 뜨면 데이터가 중복 수집되거나 DB 락(Lock)이 걸릴 수 있으므로, 철저하게 단 1개만 뜨도록 싱글톤(`replicas: 1`)을 보장합니다.

| 설정 | 값 | 이유 |
|------|-----|------|
| `replicas` | 1 | 싱글톤 전제 / 중복 수집 방지 |
| `SPRING_PROFILES_ACTIVE` | collector | Collector 모드로 Spring Boot 기동 |
| `REDIS_URL` | redis-collector-service | Collector 전용 Redis만 연결 |
| `memory requests/limits` | 512Mi / 512Mi | Guaranteed QoS 등급 확보 |

> **절대 replicas를 2 이상으로 변경하지 마세요.** 중복 수집 및 데이터 오염이 발생합니다.

---

### 2.4 캐시 및 스토리지 영역 (성능 최적화)

#### `redis-api.yaml` & `redis-collector.yaml` — 전용 Redis 캐시 시스템

- **역할**: 빈번한 DB 조회를 줄여 시스템 속도를 극대화하는 고속 메모리 캐시입니다.
- **고립화 원칙**: API 서버용 조회 캐시(`redis-api`)와 수집기용 적재 캐시(`redis-collector`)를 완벽하게 분리하여, 상호 간의 데이터 간섭이나 메모리 침범을 원천 차단했습니다.

| 설정 | 값 | 이유 |
|------|-----|------|
| `maxmemory` | 180mb | limits(256Mi) 대비 마진 확보 → OOMKilled 방지 |
| `maxmemory-policy` | allkeys-lru | 오래된 키 우선 제거 / 캐시 용도 최적화 |
| `appendonly` | no | 캐시 용도 / 재시작 시 초기화 허용 |
| `requirepass` | $(REDIS_PASSWORD) | Redis 무단 접근 차단 |
| `memory requests/limits` | 256Mi / 256Mi | Guaranteed QoS 등급 확보 |

**보안 리팩토링 포인트:**

```bash
# 이전 방식: ps aux에 비밀번호 평문 노출 위험
redis-cli -a $REDIS_PASSWORD ping

# 현재 방식: URL 기반 인증으로 비밀번호 노출 차단
redis-cli -u redis://:$(REDIS_PASSWORD)@localhost:6379 ping
```

---

#### `mysql-service.yaml` — 외부 데이터베이스 연동 서비스

- **역할**: 클러스터 내부에서 실행되는 Pod들이, 안정성을 위해 클러스터 외부에 따로 구축된 독립 데이터베이스(MySQL VM)를 마치 내부 서비스인 것처럼 자연스럽게 연결할 수 있도록 내부 IP 매핑(Endpoints) 인터페이스를 제공합니다.

```plaintext
Pod → mysql-service (ClusterIP) → Endpoints → MySQL VM IP (192.168.0.100:3306)
```

> **배포 전 반드시** `192.168.0.100`을 실제 MySQL VM의 고정 IP로 변경하세요!

---

### 2.5 가용성 및 성능 관리 영역

#### `hpa.yaml` — 수평형 자동 확장 정책

- **역할**: 갑작스러운 사용자 유입으로 API 서버의 CPU나 메모리 부하가 가중되면 자동으로 Pod 개수를 늘려주는 오토스케일링(Auto Scaling) 룰입니다.

```plaintext
스케일 아웃 (확장):
  CPU 평균 70% 초과 OR 메모리 평균 80% 초과
  → 즉시(0초) 감지 (비트코인 급등락 대응)
  → 15초마다 최대 100% 또는 2개씩 추가
  → 최대 5개까지 확장

스케일 인 (축소):
  부하 감소 후 5분(300초) 관찰 (재급등 대비)
  → 15초마다 축소
  → 최소 2개 항상 유지
```

> HPA 동작을 위해 클러스터에 **metrics-server**가 반드시 설치되어 있어야 합니다.

```bash
# metrics-server 설치
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# 설치 확인
kubectl get deployment metrics-server -n kube-system
```

---

#### `pdb.yaml` — 자원 보호 정책

- **역할**: 클러스터 점검이나 노드 이중화 작업 등 인프라 관리 행위로 인해 서버가 강제로 내려가더라도, 최소한의 서비스 구동 개수를 보장하여 무중단 서비스를 유지하게 해주는 안전장치입니다.

```plaintext
API Pod 2개 운영 중
→ 노드 점검으로 Pod 강제 종료 시도
→ PDB: "최소 1개는 반드시 유지!"
→ 1개씩 순차 종료 → 서비스 무중단 유지
```

---

## 3. 배포 및 구동 순서

자원 간의 의존성(비밀번호 주입, 엔드포인트 연결 등)이 엮여 있으므로 반드시 아래 순서대로 배포를 진행해야 정상적으로 클러스터가 구성됩니다.

```bash
# Step 1. 가장 먼저 암호화 키체인(Secret) 배포
# → 이후 배포되는 모든 Pod가 이 Secret을 참조하므로 반드시 선행
kubectl apply -f secret.yaml

# Step 2. 네트워크 방화벽 및 인프라 서비스(DB, 캐시) 레이어 배포
# → 앱보다 인프라가 먼저 준비되어야 앱이 연결 가능
kubectl apply -f network-policy.yaml
kubectl apply -f mysql-service.yaml
kubectl apply -f redis-api.yaml
kubectl apply -f redis-collector.yaml

# Step 3. 실제 비즈니스 로직 애플리케이션(API Server, Collector) 배포
# → Redis/MySQL이 준비된 후 앱 배포
kubectl apply -f api-deployment.yaml
kubectl apply -f api-service.yaml
kubectl apply -f collector-deployment.yaml

# Step 4. 외부 트래픽 인그레스 및 가용성(HPA, PDB) 정책 적용
# → 앱이 완전히 뜬 후 외부 트래픽 개방
kubectl apply -f ingress.yaml
kubectl apply -f hpa.yaml
kubectl apply -f pdb.yaml
```

### 한 번에 전체 배포

```bash
kubectl apply -f secret.yaml && \
kubectl apply -f network-policy.yaml && \
kubectl apply -f mysql-service.yaml && \
kubectl apply -f redis-api.yaml && \
kubectl apply -f redis-collector.yaml && \
kubectl apply -f api-deployment.yaml && \
kubectl apply -f api-service.yaml && \
kubectl apply -f collector-deployment.yaml && \
kubectl apply -f ingress.yaml && \
kubectl apply -f hpa.yaml && \
kubectl apply -f pdb.yaml
```

---

## 4. 배포 전 필수 체크리스트

배포 전 아래 항목들을 반드시 확인하고 실제 환경에 맞게 수정하세요.

### 4.1 수정 필요한 값 목록

| 파일 | 수정할 항목 | 현재 값 | 변경 필요 내용 |
|------|-----------|---------|--------------|
| `secret.yaml` | `mysql-password` | `bXlzcWwxMjM=` | 실제 MySQL 비밀번호 base64 인코딩 값 |
| `secret.yaml` | `redis-password` | `bXktcmVkaXMtcGFzc3dvcmQ=` | 실제 Redis 비밀번호 base64 인코딩 값 |
| `secret.yaml` | `external-api-key` | `dG9rZW4tMTIzNDU=` | 거래소 API 인증 토큰 base64 인코딩 값 |
| `mysql-service.yaml` | `ip` | `192.168.0.100` | 실제 MySQL VM 고정 IP 주소 |
| `ingress.yaml` | `host` | `api.yourdomain.com` | 실제 서비스 도메인 (IP 접근 시 라인 삭제) |
| `*-deployment.yaml` | `image` | `my-repo/bitcoin-dashboard:v1.0.0` | 실제 Docker 이미지 경로 및 버전 |

### 4.2 노드 라벨 설정

```bash
# worker 노드에 라벨 추가 (Pod 스케줄링 필수 조건)
kubectl label node node-worker-01 node-role.kubernetes.io/worker=true
kubectl label node node-worker-02 node-role.kubernetes.io/worker=true

# 라벨 적용 확인
kubectl get nodes --show-labels
```

### 4.3 클러스터 연결 확인

```bash
# kubectl 설치 확인
kubectl version --client

# 클러스터 연결 확인
kubectl cluster-info

# 노드 상태 확인 (모든 노드 Ready 상태여야 함)
kubectl get nodes
```

### 4.4 metrics-server 설치 확인 (HPA 필수)

```bash
# 설치 확인
kubectl get deployment metrics-server -n kube-system

# 미설치 시 설치
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
```

### 4.5 Git 보안 설정

```bash
# secret.yaml 절대 커밋 금지
echo "k8s/secret.yaml" >> .gitignore

# 실수로 커밋했다면
git rm --cached k8s/secret.yaml
git commit -m "remove secret.yaml from tracking"
```

---

## 5. 배포 후 상태 확인

### 5.1 전체 Pod 상태 확인

```bash
# 모든 Pod 상태 확인
kubectl get pods

# 예상 출력:
# NAME                                READY   STATUS    RESTARTS
# collector-deployment-xxx-xxx        1/1     Running   0
# api-server-xxx-aaa                  1/1     Running   0
# api-server-xxx-bbb                  1/1     Running   0
# redis-api-xxx-xxx                   1/1     Running   0
# redis-collector-xxx-xxx             1/1     Running   0
```

### 5.2 서비스 연결 확인

```bash
# 모든 Service 확인
kubectl get services

# Ingress 확인
kubectl get ingress

# MySQL Endpoints 확인 (외부 VM IP가 등록됐는지 확인)
kubectl get endpoints mysql-service
```

### 5.3 HPA 상태 확인

```bash
kubectl get hpa api-server-hpa

# 예상 출력:
# NAME             REFERENCE              TARGETS         MINPODS   MAXPODS   REPLICAS
# api-server-hpa   Deployment/api-server  30%/70%         2         5         2
```

### 5.4 로그 확인

```bash
# Collector 로그 (거래소 WebSocket 연결 확인)
kubectl logs -l app=collector -f

# API 서버 로그
kubectl logs -l app=api -f

# 특정 Pod 로그
kubectl logs <pod-name> -f

# 이전 재시작된 Pod 로그 (크래시 원인 분석)
kubectl logs <pod-name> --previous
```

### 5.5 NetworkPolicy 동작 확인

```bash
# NetworkPolicy 목록 확인
kubectl get networkpolicy

# 예상 출력:
# NAME                          POD-SELECTOR          AGE
# redis-api-strict-policy       app=redis-api         Xs
# redis-collector-strict-policy app=redis-collector   Xs
```

---

## 6. 트러블슈팅

### Pod가 Pending 상태인 경우

```bash
# 상세 원인 확인
kubectl describe pod <pod-name>
```

| 원인 | 해결 방법 |
|------|---------|
| 노드 라벨 없음 | `kubectl label node <node-name> node-role.kubernetes.io/worker=true` |
| 노드 리소스 부족 | `kubectl describe node <node-name>` 으로 리소스 확인 |
| Secret 없음 | `kubectl apply -f secret.yaml` 재실행 |
| 이미지 Pull 실패 | 이미지 경로 및 Docker 레지스트리 접근 권한 확인 |

---

### Pod가 CrashLoopBackOff인 경우

```bash
# 로그로 원인 파악
kubectl logs <pod-name> --previous
```

| 원인 | 해결 방법 |
|------|---------|
| Secret 키 이름 불일치 | `secret.yaml`의 키 이름과 deployment의 `secretKeyRef.key` 일치 여부 확인 |
| Redis 연결 실패 | `REDIS_URL` 환경변수와 Service 이름 일치 여부 확인 |
| MySQL 연결 실패 | `mysql-service.yaml`의 Endpoints IP가 실제 VM IP인지 확인 |
| 포트 불일치 | `containerPort`와 probe의 `port` 값 일치 여부 확인 |
| Redis 비밀번호 불일치 | `secret.yaml`의 `redis-password` base64 값 재확인 |

---

### HPA가 동작하지 않는 경우

```bash
# HPA 상세 확인
kubectl describe hpa api-server-hpa

# metrics-server 상태 확인
kubectl get deployment metrics-server -n kube-system
kubectl top pods  # 메트릭 수집 확인
```

| 원인 | 해결 방법 |
|------|---------|
| metrics-server 미설치 | [4.4 항목](#44-metrics-server-설치-확인-hpa-필수) 참고하여 설치 |
| metrics-server 미동작 | `kubectl rollout restart deployment metrics-server -n kube-system` |
| scaleTargetRef 이름 불일치 | `hpa.yaml`의 `name`과 `api-deployment.yaml`의 `metadata.name` 일치 확인 |

---

### Redis 연결 실패

```bash
# Redis Pod 상태 확인
kubectl get pods -l app=redis-api
kubectl get pods -l app=redis-collector

# Redis Service 확인
kubectl get service redis-api-service
kubectl get service redis-collector-service

# Redis 직접 접속 테스트
kubectl exec -it <redis-pod-name> -- redis-cli -u redis://:$(REDIS_PASSWORD)@localhost:6379 ping
```

| 원인 | 해결 방법 |
|------|---------|
| Secret의 redis-password 불일치 | base64 값 재확인 후 `kubectl apply -f secret.yaml` |
| Service 이름 오타 | deployment의 `REDIS_URL`과 Service `metadata.name` 일치 확인 |
| NetworkPolicy 차단 | 올바른 Pod label로 접근하는지 확인 |

---

### Ingress로 접근이 안 되는 경우

```bash
# Ingress 상태 확인
kubectl describe ingress api-server-ingress

# Ingress Controller 확인
kubectl get pods -n ingress-nginx
```

| 원인 | 해결 방법 |
|------|---------|
| Nginx Ingress Controller 미설치 | Nginx Ingress Controller 설치 필요 |
| 도메인 미설정 | `ingress.yaml`의 `host` 값 또는 DNS 설정 확인 |
| api-service 포트 불일치 | `ingress.yaml`의 `port.number`와 `api-service.yaml`의 `port` 일치 확인 |

---

### 전체 재배포 (초기화 후 재시작)

```bash
# 전체 삭제
kubectl delete -f k8s/

# 잠시 대기 후 전체 재배포
sleep 10

# 순서대로 재배포
kubectl apply -f secret.yaml && \
kubectl apply -f network-policy.yaml && \
kubectl apply -f mysql-service.yaml && \
kubectl apply -f redis-api.yaml && \
kubectl apply -f redis-collector.yaml && \
kubectl apply -f api-deployment.yaml && \
kubectl apply -f api-service.yaml && \
kubectl apply -f collector-deployment.yaml && \
kubectl apply -f ingress.yaml && \
kubectl apply -f hpa.yaml && \
kubectl apply -f pdb.yaml
```

---

## 7. 인프라 설계 핵심 요약 (팀원 공유용)

### Guaranteed QoS 클래스 완전 준수

구동되는 모든 컨테이너는 `requests`(최소 보장 자원)와 `limits`(최대 제한 자원)를 **1:1로 완전 고정(Fix)** 하고 `ephemeral-storage`까지 명시하여, 클러스터 부하 시 메모리 부족으로 인해 우리 서버가 강제 종료(OOMKill)되는 우선순위에서 **최하위 안전지대로 격상**시켰습니다.

```plaintext
일반 설정 (Burstable):        우리 설정 (Guaranteed):
requests: 256Mi               requests: 256Mi
limits:   512Mi               limits:   256Mi  ← 동일하게 고정
→ OOM 시 중간 순위 종료        → OOM 시 가장 마지막에 종료
```

### 보안 제일주의

불필요한 K8s 내부 시스템 접근을 막기 위해 모든 Pod의 K8s API 토큰 자동 마운트 옵션을 원천 차단(`automountServiceAccountToken: false`)했습니다.

### Redis 완전 분리

```plaintext
redis-collector ──▶ Collector Pod 전용 (수집 데이터 저장)
redis-api       ──▶ API Pod 전용 (캐시 조회)

하나로 합치면?
→ API Pod가 수집 중인 Redis에 동시 접근
→ 데이터 충돌 / 성능 저하 / 보안 취약점 발생
```

### NGINX 중복 제거

웹 프론트엔드 정적 파일 서빙과 리버스 프록시 역할은 앞단의 강력한 Ingress가 담당하므로, 클러스터 내부에 거추장스러운 별도의 `nginx-deployment`를 중복해서 띄우지 않아 불필요한 컴퓨팅 자원 낭비를 완벽하게 절약했습니다.

### 즉시 스케일 아웃 (원칙3)

```plaintext
stabilizationWindowSeconds: 0  # 부하 감지 즉시 스케일 아웃
→ 비트코인 급등락 시 사용자 폭발적 유입에 즉각 대응
→ 서비스 장애 없이 최대 5개 Pod까지 자동 확장
```

---

## 8. 용어 사전

인프라 비전공자를 위한 핵심 용어 설명입니다.

| 용어 | 쉬운 설명 |
|------|----------|
| **Pod** | K8s에서 컨테이너가 실행되는 가장 작은 단위. 하나의 앱 인스턴스 |
| **Deployment** | Pod를 몇 개 띄울지, 어떤 이미지로 띄울지 정의하는 설정 파일 |
| **Service** | Pod에 접근할 수 있는 고정 주소. Pod는 재시작마다 IP가 바뀌지만 Service는 고정 |
| **Ingress** | 외부에서 클러스터 안으로 들어오는 트래픽의 관문. 도메인 라우팅 담당 |
| **HPA** | CPU/메모리 사용량에 따라 Pod 수를 자동으로 늘리고 줄이는 기능 |
| **PDB** | 노드 점검 시에도 최소 몇 개의 Pod는 반드시 살아있어야 한다는 정책 |
| **Secret** | 비밀번호 같은 민감한 정보를 안전하게 저장하는 K8s 오브젝트 |
| **NetworkPolicy** | Pod 간 통신을 허용/차단하는 방화벽 규칙 |
| **ClusterIP** | 클러스터 내부에서만 접근 가능한 가상 IP. 외부에서는 접근 불가 |
| **Guaranteed 등급** | requests == limits 설정 시 부여되는 최고 안정성 등급. OOM 시 마지막에 종료 |
| **OOMKilled** | 메모리 초과로 Pod가 강제 종료되는 상황 |
| **SPOF** | Single Point of Failure. 하나가 죽으면 전체가 멈추는 단일 장애점 |
| **replicas** | 동일한 Pod를 몇 개 복제해서 띄울지 결정하는 숫자 |
| **base64** | 바이너리 데이터를 텍스트로 인코딩하는 방식. Secret 값 저장에 사용 |
| **WebSocket** | 서버와 클라이언트 간 실시간 양방향 통신 프로토콜 |
| **Redis** | 메모리 기반 초고속 캐시 저장소. DB보다 훨씬 빠른 조회 가능 |
| **allkeys-lru** | 메모리 꽉 찼을 때 가장 오래 안 쓴 데이터부터 지우는 정책 |
| **Endpoints** | Service가 실제로 트래픽을 전달할 IP:Port 목록. 외부 VM 연결에 사용 |
| **namespace** | K8s 리소스를 논리적으로 구분하는 그룹. 이 프로젝트는 default 사용 |
| **싱글톤** | 인스턴스를 단 1개만 띄우는 패턴. Collector에 적용 |

---

## 9. 갱신 이력

| 날짜 | 내용 |
|------|------|
| 2026-06-29 | redis-cli -u 보안 취약점 패치 반영 |
| 2026-06-29 | redis-collector 독립 캐시 분리 구조 반영 리팩토링 완료 |
| 2026-06-29 | 트러블슈팅 섹션 추가 |
| 2026-06-29 | 배포 전 필수 체크리스트 추가 |
| 2026-06-29 | 최초 작성 (Production-Ready) |

---

> **K8s 버전:** v1.32.2  
> **Redis 버전:** 7.2  
> **Spring Boot:** 2.3+