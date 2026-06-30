# SyncQuote 모니터링 스택 (Prometheus + Grafana + Exporters)

내부 전용 메트릭 모니터링. **DB가 SSOT**라는 방침대로 MySQL/Redis는 클러스터 밖 VM에서
네이티브로 돌고, 모니터링도 그 토폴로지를 그대로 따른다.

## 아키텍처

```
[sq-k8s-01 172.16.116.10]                 [sq-mysql-01 .100]        [sq-data-01 .101]
  ┌─ monitoring ns ─────────────┐          node_exporter :9100      node_exporter :9100
  │ Grafana (ClusterIP) ◄─SSH터널           mysqld_exporter :9104    Redis :6379(requirepass)
  │   └─ Prometheus (ClusterIP 9090)
  │        ├─ scrape node_exporter  .10/.100/.101:9100
  │        ├─ scrape mysqld_exporter .100:9104
  │        └─ scrape redis-exporter (Pod, ClusterIP :9121) ─► .101:6379
  └─────────────────────────────┘
  node_exporter :9100 (이 노드)
```

- **내부 전용**: Grafana·Prometheus·redis-exporter 모두 **ClusterIP**(노드에 열린 포트 0).
  Grafana 접근은 **SSH 터널 / kubectl port-forward로만** → 외부·LAN 노출 자체가 없음.
  exporter 포트(9100/9104)는 DB VM 방화벽에서 Prometheus 노드(.10)만 허용.

## 설치 순서

### 1) 호스트 exporter (host/ 참고) — 직접 실행
- **3개 VM 모두**: `node_exporter.service` 로 node_exporter(:9100) 기동
- **sq-mysql-01 에만**:
  1. `mysql -u root -p < host/mysql-exporter-user.sql` (전용 계정, 비번 채우기)
  2. `host/exporter.my.cnf.example` → `/etc/mysqld_exporter/.my.cnf` (비번/권한 600)
  3. `mysqld_exporter.service` 로 mysqld_exporter(:9104) 기동
- 확인: 각 VM에서 `curl -s localhost:9100/metrics | head`, mysql VM은 `:9104`도.

### 2) 시크릿 (한 번)
```bash
cd monitoring
cp secret.example.yaml secret.yaml
#   grafana-admin-password = 강력한 비번
#   redis-password         = default/app-secret 의 redis-password 와 동일 값
kubectl apply -f secret.yaml
```

### 3) 클러스터 스택
```bash
kubectl apply -k monitoring/
kubectl -n monitoring get pods,svc
# 타깃 상태 확인: Prometheus UI(포트포워드) 또는 Grafana Explore 에서 up == 1
kubectl -n monitoring port-forward svc/prometheus 9090:9090   # → http://localhost:9090/targets
```

### 4) Grafana 접속 & 대시보드
- 접속(둘 중 하나):
  - 로컬에서 바로: `kubectl -n monitoring port-forward svc/grafana 3000:3000` → `http://localhost:3000`
  - 원격(SSH 터널): `ssh -L 3000:localhost:30030 <user>@172.16.116.10` 는 NodePort 시절 방식 →
    ClusterIP에선 `ssh -t <user>@172.16.116.10 'kubectl -n monitoring port-forward svc/grafana 30030:3000'`
    후 다른 터미널 `ssh -L 3000:localhost:30030 <user>@172.16.116.10`, 또는 노드에서 port-forward 띄우고 그 포트를 `-L`로 터널
  - admin / secret 비번
- 데이터소스(Prometheus)는 자동 등록됨. **대시보드는 import**:
  - Node Exporter Full = **1860**
  - MySQL Overview = **14057** (또는 7362)
  - Redis = **763** (또는 11835)
  - Dashboards → Import → ID 입력 → 데이터소스 Prometheus 선택
  - (Grafana Pod에 인터넷 egress가 없으면 grafana.com에서 JSON 받아 업로드)

## 방화벽 (이중 방어, 노드/VM에서 설정)
```
# Grafana는 ClusterIP라 노드에 열린 포트가 없음 → NodePort 방화벽 규칙 불필요.
# 접근은 SSH(22) 터널 / kubectl port-forward로만. SSH는 키 인증·최신 패치 유지.

# DB VM(.100/.101): exporter 포트는 Prometheus 노드(.10)만
ufw allow from 172.16.116.10 to any port 9100 proto tcp   # 두 VM
ufw allow from 172.16.116.10 to any port 9104 proto tcp   # mysql VM
# Redis 6379 는 기존 정책대로 .10(노드)만 — 신규 개방 금지
```

## 핵심 지표 (대시보드에서 볼 것)
- **MySQL**: Threads_connected vs max_connections, QPS(Queries rate), slow queries,
  InnoDB buffer pool hit율, row-lock 대기, aborted_connects
- **Redis**: used_memory vs maxmemory, evicted_keys(noeviction이라 0이어야 정상), keyspace
  hit/miss율, ops/sec, connected/blocked_clients, (옵션) Stream XLEN·소비자 lag
- **노드**: CPU/메모리/디스크 I·O·용량/네트워크

## 후순위(이번 범위 밖)
Alertmanager(임계 알림), WAS actuator(/actuator/prometheus → Hikari/JVM/SSE), 로그(Loki).
