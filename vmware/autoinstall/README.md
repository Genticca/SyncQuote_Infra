# SyncQuote — 무인 설치(autoinstall)

콘솔/VNC 없이 **셸만으로** Ubuntu Server 3대를 자동 설치한다. 헤드리스 + Wi-Fi 호스트 전제라 기본 네트워크는 **NAT(vmnet8)**.

| VM | IP(기본, NAT) | 비고 |
|---|---|---|
| sq-k8s-01 | `172.16.116.10` | K8s/Ingress/Collector/Archiver |
| sq-mysql-01 | `172.16.116.100` | MySQL |
| sq-data-01 | `172.16.116.101` | Redis(수집) |

> 계정 `quote` / 비밀번호 `syncquote`(기본). **운영 전 반드시 교체** — `PWHASH="$(openssl passwd -6)"`.

## 사전 준비

```bash
sudo apt install -y xorriso genisoimage           # ISO 리마스터/seed 생성 도구
# VM은 이미 생성돼 있어야 함. 없으면:
#   FORCE=true ./create-syncquote-vms.sh create
```

## 1) 빌드 & 설치 시작

```bash
cd ~/SyncQuote_Infra/vmware/autoinstall
# (선택) 내 공개키로 키 로그인까지:
#   export SSH_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)"
# (선택) 비밀번호 교체:
#   export PWHASH="$(openssl passwd -6)"
./build-autoinstall.sh build
```

하는 일:
1. 설치 ISO를 리마스터해 **`autoinstall ds=nocloud`** 커널 파라미터 주입(→ 부팅 시 확인 프롬프트 없이 자동 설치).
2. VM별 cloud-init **seed ISO(CIDATA)** 생성 — 호스트명·고정 IP·계정·`open-vm-tools` 포함.
3. 각 vmx에 [설치 ISO + seed ISO] 연결 + **NAT 전환**.
4. 3대 `nogui`로 부팅 → 무인 설치 시작.

## 2) 진행 확인

```bash
watch -n5 'vmrun -T ws list'
```
설치가 끝나면 각 VM이 **자동 poweroff**(`shutdown: poweroff`) 되어 목록에서 사라진다. 3대 모두 사라지면 완료.

## 3) 마무리(ISO 분리 후 디스크 부팅)

```bash
./build-autoinstall.sh finalize
```
설치 미디어를 분리하고 디스크로 재부팅한다. 이후 접속:

```bash
ssh quote@172.16.116.10     # k8s
ssh quote@172.16.116.100    # mysql
ssh quote@172.16.116.101    # data
```

## 환경변수 override

| 변수 | 기본 | 설명 |
|---|---|---|
| `BASE_ISO` | `~/iso/ubuntu-22.04.5-live-server-amd64.iso` | 원본 설치 ISO |
| `VM_ROOT` | `~/VMWare/VMX` | vmx 위치 |
| `WORK_DIR` | `~/autoinstall` | 리마스터/seed 산출물 |
| `NETWORK` | `nat` | `nat` \| `bridged` |
| `SUBNET` / `GATEWAY` | `172.16.116` / `.2` | NAT면 그대로. bridged면 LAN 대역으로 |
| `DNS1` / `DNS2` | `172.16.116.2` / `8.8.8.8` | 네임서버 |
| `USERNAME` | `quote` | 관리 계정 |
| `PWHASH` | (해시) | `openssl passwd -6` 결과 |
| `SSH_PUBKEY` | (없음) | 채우면 키 로그인 허용 |

## 다음 단계

설치 완료 후 OS 위에 역할별 소프트웨어(containerd/K8s, MySQL, Redis)를 올리는 절차는
[`../../docs/vmware-k8s-deployment.md`](../../docs/vmware-k8s-deployment.md) 참고.

> **NAT 사용 시 주의**: VM IP가 `172.16.116.x`로 바뀌므로, K8s `mysql-service`/`redis-collector-service`의
> Endpoints와 netplan 고정 IP도 이 대역으로 맞춰야 한다. 외부(인터넷)에서 Ingress(80/443)에 닿으려면
> 호스트에서 vmnet8 NAT 포트포워딩이 필요하다.
