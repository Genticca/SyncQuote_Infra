# SyncQuote Infra

SyncQuote는 Upbit와 Binance WebSocket API에서 코인 시세를 수집하고, Redis와 MySQL을 이용해 수집/적재/조회 ETL 파이프라인을 구성하는 온프레미스 인프라 프로젝트입니다.

현재 목표는 단일 물리 서버 위에 VMware Workstation Pro VM을 구성하고, 서비스 계층은 Kubernetes로 운영하되 Redis와 MySQL은 DBMS 독립성을 위해 별도 VM으로 분리하는 것입니다.

## 1. 목표 아키텍처

```plaintext
[External Clients / Load Generators]
        |
        v
[sq-k8s-01]
  NGINX Ingress
  API/WAS Deployment
  Collector Deployment
  Archiver Deployment 또는 Collector 내부 아카이빙 모드
        |                         |
        | Redis read/write        | MySQL write/read
        v                         v
[sq-data-01]                  [sq-mysql-01]
  Redis Collector               MySQL
  Redis API
```

데이터 흐름은 다음을 기준으로 합니다.

```plaintext
Upbit/Binance WebSocket
  -> Collector
  -> Redis Collector
  -> Archiver
  -> MySQL
  -> API/WAS
  -> Redis API 또는 MySQL 조회
  -> Client
```

## 2. VMware VM 계획

호스트 OS는 Ubuntu Server 24.04.04, 가상화는 VMware Workstation Pro를 기준으로 합니다.

| VM | 역할 | vCPU | Memory | Disk | 비고 |
| --- | --- | ---: | ---: | ---: | --- |
| `sq-k8s-01` | Kubernetes, NGINX Ingress, API/WAS, Collector, Archiver | 4 | 6GB | 45GB | 서비스 실행 노드 |
| `sq-data-01` | Redis Collector, Redis API | 4 | 8GB | 55GB | Redis 전용 VM |
| `sq-mysql-01` | MySQL | 2 | 8GB | 75GB | 영구 저장소 VM |

총 디스크 할당량은 `175GB`입니다. 현재 계획의 상한인 `180GB 이하` 조건을 만족합니다.

### VMware 경로

| 항목 | 경로 |
| --- | --- |
| Ubuntu 설치 ISO | `/home/quote-team/iso/ubuntu-22.04.5-live-server-amd64.iso` |
| VM 저장 루트 | `/home/quote-team/VMWare/VMX` |
| VM 생성 스크립트 | `vmware/create-syncquote-vms.sh` |

### VM 생성 및 실행

```bash
cd SyncQuote_Infra/vmware
chmod +x create-syncquote-vms.sh
./create-syncquote-vms.sh create
./create-syncquote-vms.sh start
```

GUI 없이 실행하려면 다음처럼 실행합니다.

```bash
HEADLESS=true ./create-syncquote-vms.sh start
```

VMX 파일 경로를 확인합니다.

```bash
./create-syncquote-vms.sh list
```

VMware가 인식 중인 실행 VM 목록을 확인합니다.

```bash
./create-syncquote-vms.sh status
```

VM을 종료합니다.

```bash
./create-syncquote-vms.sh stop
```

### 스크립트 환경 변수

| 변수 | 기본값 | 설명 |
| --- | --- | --- |
| `ISO_PATH` | `/home/quote-team/iso/ubuntu-22.04.5-live-server-amd64.iso` | Ubuntu Server 설치 ISO |
| `VM_ROOT` | `/home/quote-team/VMWare/VMX` | VMX/VMDK 생성 위치 |
| `NETWORK` | `bridged` | `bridged` 또는 `nat` |
| `DISK_CONTROLLER_TYPE` | `lsilogic` | `lsilogic`, `lsisas`, `pvscsi` 중 선택 |
| `HEADLESS` | `false` | `true`이면 `vmrun ... nogui`로 실행 |
| `FORCE` | `false` | 기존 VM 디렉토리가 있어도 VMX 메타데이터 재작성 |

외부 부하 생성 서버에서 VM에 직접 접근할 계획이므로 기본 네트워크는 `bridged`입니다. NAT가 필요하면 생성 전에 지정합니다.

```bash
NETWORK=nat ./create-syncquote-vms.sh create
```

주의사항:

- 기존 VM 디렉토리가 있으면 기본적으로 건너뜁니다.
- `FORCE=true`는 VMX 메타데이터를 다시 쓰지만 기존 VMDK는 삭제하지 않습니다.
- 최초 부팅 시 ISO가 CD-ROM으로 연결되어 있으므로 Ubuntu Server 설치를 진행합니다.
- 설치 완료 후에는 VMware 설정에서 ISO 연결을 해제하거나 VMX의 `sata0:1.startConnected` 값을 `FALSE`로 변경합니다.

## 3. 서비스 배치 원칙

| 계층 | 배치 | 이유 |
| --- | --- | --- |
| API/WAS | `sq-k8s-01`의 Kubernetes Deployment | 사용자 요청 처리와 수평 확장 대상 |
| NGINX Ingress | `sq-k8s-01` | 외부 HTTP 트래픽 진입점 |
| Collector | `sq-k8s-01`의 Kubernetes Deployment | WebSocket 수집 프로세스 관리, 재시작 자동화 |
| Archiver | `sq-k8s-01`의 Kubernetes Deployment 권장 | Redis 적재와 MySQL 영속화를 분리 |
| Redis | `sq-data-01` | 메모리 DBMS 독립성 확보, API용/Collector용 논리 분리 |
| MySQL | `sq-mysql-01` | 영구 저장소 독립성 확보 |

Redis와 MySQL은 Kubernetes 내부 Pod보다 별도 VM에서 운영하는 방향이 현재 계획에 더 맞습니다. 이렇게 하면 K8s 재배포, Pod 재시작, 서비스 롤링 업데이트가 DBMS 생명주기에 직접 영향을 주지 않습니다.

## 4. 현재 저장소 구성

매니페스트는 VM 계층별 디렉토리로 분류한다.

```plaintext
SyncQuote_Infra/
├── web/                          # Web Service VM (sq-k8s-01): 사용자 노출 계층
│   ├── api-deployment.yaml       #   API/WAS Deployment
│   ├── api-service.yaml          #   API Service
│   ├── front-deployment.yaml     #   Front(Vue+nginx) Deployment
│   ├── front-service.yaml        #   Front Service
│   ├── ingress.yaml              #   Ingress (/api/v1→API, /→Front)
│   ├── pdb.yaml                  #   API PodDisruptionBudget
│   └── hpa.yaml                  #   API HorizontalPodAutoscaler (CPU 기반)
├── data/                         # Redis + Collector/Archive VM (sq-data-01)
│   ├── redis-api.yaml            #   API용 Redis Deployment + Service
│   ├── redis-collector.yaml      #   수집용 Redis Deployment + Service
│   ├── collector-deployment.yaml #   collector + archiver (RUN_MODE 분리)
│   └── network-policy.yaml       #   Redis 접근 제어(NetworkPolicy)
├── mysql/                        # MySQL VM (sq-mysql-01)
│   └── mysql-service.yaml        #   외부 MySQL Service + Endpoints
├── shared/                       # 여러 계층 공통
│   ├── secret.example.yaml       #   secret.yaml 템플릿 (커밋 O)
│   └── secret.yaml               #   app-secret (커밋 X — .gitignore)
├── crypto-collector/             # Go 수집기/아카이버 소스
└── vmware/
    └── create-syncquote-vms.sh   # VMware VMX/VMDK 생성 및 실행 스크립트
```

현재 매니페스트 기준으로는 `redis-api.yaml`, `redis-collector.yaml`이 Redis를 Kubernetes 내부 Deployment로 실행하는 형태입니다. 최종 목표처럼 Redis를 `sq-data-01` 외부 VM으로 분리하려면 두 파일은 selector 없는 `Service + Endpoints` 형태로 바꾸거나 별도 외부 Redis용 매니페스트로 교체해야 합니다.

또한 Archiver가 Collector와 완전히 분리된 Deployment로 운영되려면 별도의 `archiver-deployment.yaml` 또는 Collector 이미지의 실행 모드 분리가 필요합니다.

## 5. Kubernetes 배포 순서

계층 디렉토리 단위로, 의존 순서(공통 → MySQL → 데이터 → 웹)대로 적용한다.
(`kubectl apply -f <dir>/` 는 디렉토리 안 YAML을 한 번에 적용한다.)

```bash
kubectl apply -f shared/   # secret 먼저 (Pod가 참조)
kubectl apply -f mysql/    # mysql-service.yaml의 Endpoint IP를 sq-mysql-01로 교체 후
kubectl apply -f data/     # 두 Redis + collector/archiver + NetworkPolicy
kubectl apply -f web/      # API + Front + Ingress + PDB
```

`web/hpa.yaml`은 실제 HorizontalPodAutoscaler다 — `kubectl apply -f web/`에 포함되며 metrics-server가 설치돼 있어야 동작한다.
Ingress가 동작하려면 NGINX Ingress Controller가 클러스터에 먼저 설치돼 있어야 한다.
`secret.yaml`은 `.gitignore`로 제외되니, `shared/secret.example.yaml`을 복사해 실제 값으로 채운다.

worker 노드 라벨이 필요합니다.

```bash
kubectl label node sq-k8s-01 node-role.kubernetes.io/worker=true --overwrite
kubectl get nodes --show-labels
```

HPA를 사용하려면 metrics-server가 필요합니다.

```bash
kubectl get deployment metrics-server -n kube-system
kubectl top pods
```

## 6. 배포 전 체크리스트

- `secret.yaml`은 실제 운영 비밀번호로 교체하고 Git 커밋 대상에서 제외합니다.
- `mysql-service.yaml`의 Endpoint IP를 `sq-mysql-01`의 고정 IP로 변경합니다.
- Redis를 외부 VM으로 분리할 경우 Redis Service/Endpoint도 `sq-data-01`의 고정 IP로 변경합니다.
- `api-deployment.yaml`, `collector-deployment.yaml`의 `image`를 실제 CI에서 빌드한 이미지로 변경합니다.
- `ingress.yaml`의 host를 실제 도메인 또는 IP 접근 방식에 맞게 수정합니다.
- `hpa.yaml`이 실제 `HorizontalPodAutoscaler` 리소스인지 확인합니다.
- `sq-data-01`, `sq-mysql-01`은 고정 IP를 사용합니다.
- 외부 부하 생성 서버가 `sq-k8s-01`의 Ingress 주소에 접근 가능한지 확인합니다.

## 7. 운영 확인 명령

Pod와 Deployment 상태를 확인합니다.

```bash
kubectl get deployments
kubectl get pods -o wide
```

Service와 Endpoint를 확인합니다.

```bash
kubectl get services
kubectl get endpoints
```

Ingress를 확인합니다.

```bash
kubectl get ingress
kubectl describe ingress
```

API와 Collector 로그를 확인합니다.

```bash
kubectl logs -l app=api -f
kubectl logs -l app=collector -f
```

HPA가 실제로 동작하는지 확인합니다.

```bash
kubectl get hpa
kubectl describe hpa
kubectl top pods
```

## 8. 트러블슈팅 기준

| 증상 | 우선 확인 |
| --- | --- |
| Pod `Pending` | 노드 라벨, 노드 자원, 이미지 pull 권한 |
| Pod `CrashLoopBackOff` | `kubectl logs <pod> --previous`, Secret key, 환경 변수 |
| API가 Redis에 연결 실패 | Redis Service 이름, Redis 비밀번호, Endpoint IP 또는 Redis Pod 상태 |
| API가 MySQL에 연결 실패 | `mysql-service` Endpoint IP, MySQL 방화벽, MySQL bind-address |
| Ingress 접근 실패 | Ingress Controller 설치, host/DNS, Service port |
| HPA 미동작 | metrics-server, resource requests, `hpa.yaml`의 targetRef |

## 9. 보안 원칙

- `secret.yaml`의 실제 값은 문서에 기록하지 않습니다.
- Redis와 MySQL은 외부 노출을 피하고 필요한 VM/Pod에서만 접근하도록 방화벽을 설정합니다.
- Pod에는 불필요한 Kubernetes API 토큰을 마운트하지 않습니다.
- 운영 이미지는 `latest` 대신 고정 태그를 사용합니다.
- DBMS VM은 Kubernetes 재배포와 별도로 백업/복구 정책을 둡니다.

## 10. 향후 정리 필요 작업

- Redis 매니페스트를 현재 계획에 맞게 외부 VM용 `Service + Endpoints` 구조로 정리
- Collector와 Archiver 실행 모드 분리
- 실제 HPA 매니페스트 검증 및 metrics-server 설치 절차 확정
- Redis/MySQL 모니터링 지표와 알림 구성
- 외부 부하 생성기에서 Ingress/API/DB 병목을 분리 측정하는 테스트 시나리오 작성

## 11. CI/CD 배포 방식

이미지 빌드와 GHCR push는 GitHub-hosted runner에서 수행하고, Kubernetes 배포만 온프레미스 self-hosted runner에서 수행합니다. 따라서 GitHub Actions에 `KUBE_CONFIG_B64`를 저장하지 않습니다.

배포 job은 다음 워크플로우에서 `runs-on: [self-hosted, linux, x64]`를 사용합니다.

| 서비스 | 워크플로우 | 배포 대상 |
| --- | --- | --- |
| API/WAS | `.github/workflows/deploy-prod.yml` | `deployment/api-server` |
| Front | `SyncQuote_Front/.github/workflows/deploy-front.yml` | `deployment/front-server` |
| Collector/Archiver | `SyncQuote_Infra/.github/workflows/deploy-collector.yml` | `deployment/collector-deployment`, `deployment/archiver-deployment` |

self-hosted runner가 설치된 서버 또는 VM에서는 아래 명령이 성공해야 합니다.

```bash
command -v kubectl
kubectl config current-context
kubectl -n default get deployment/api-server
kubectl -n default get deployment/front-server
kubectl -n default get deployment/collector-deployment
kubectl -n default get deployment/archiver-deployment
```

GHCR 이미지가 private이면 Kubernetes 클러스터에는 image pull secret이 필요합니다. 현재 매니페스트는 `imagePullSecrets: ghcr-secret`을 참조합니다.

```bash
kubectl -n default create secret docker-registry ghcr-secret \
  --docker-server=ghcr.io \
  --docker-username=<github-user-or-org> \
  --docker-password=<ghcr-read-token>
```

이 Secret은 GitHub Actions의 kubeconfig가 아니라, Pod가 GHCR 이미지를 pull하기 위한 Kubernetes Secret입니다.

## 12. 용어

| 용어 | 설명 |
| --- | --- |
| Deployment | Pod 실행 개수, 이미지, 업데이트 전략을 관리하는 Kubernetes 리소스 |
| Service | Pod 또는 외부 Endpoint에 안정적인 접근 주소를 제공하는 리소스 |
| Endpoints | Service가 실제로 트래픽을 전달할 IP와 Port 목록 |
| Ingress | 외부 HTTP/HTTPS 트래픽을 서비스로 라우팅하는 진입점 |
| HPA | CPU/메모리 등 지표에 따라 Pod 개수를 자동 조절하는 기능 |
| PDB | 점검이나 장애 상황에서 최소 가용 Pod 수를 보장하는 정책 |
| Redis | 메모리 기반 캐시/버퍼 저장소 |
| MySQL | 영구 저장을 담당하는 관계형 DBMS |
| Collector | 거래소 WebSocket에서 시세를 수집하는 프로세스 |
| Archiver | Redis의 수집 데이터를 MySQL에 영속화하는 프로세스 |
