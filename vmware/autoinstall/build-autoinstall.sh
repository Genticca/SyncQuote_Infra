#!/usr/bin/env bash
# SyncQuote 3-VM 무인 설치(autoinstall) 빌더
#
#   build    : 설치 ISO를 리마스터(autoinstall 커널 파라미터 주입) +
#              VM별 cloud-init seed ISO 생성 + vmx 재구성(ISO 연결/NAT) + 설치 시작
#   finalize : 설치 완료(자동 poweroff) 후 ISO를 분리하고 디스크로 부팅
#
# 콘솔/VNC 없이 셸만으로 Ubuntu Server를 3대 자동 설치한다.
# 헤드리스 + Wi-Fi 호스트 전제 → 기본 네트워크는 NAT(vmnet8).
set -euo pipefail

# ===== 설정 (환경변수로 override) =====
BASE_ISO="${BASE_ISO:-/home/quote-team/iso/ubuntu-22.04.5-live-server-amd64.iso}"
VM_ROOT="${VM_ROOT:-/home/quote-team/VMWare/VMX}"
WORK_DIR="${WORK_DIR:-/home/quote-team/autoinstall}"
OUT_ISO="${OUT_ISO:-${WORK_DIR}/ubuntu-autoinstall.iso}"

NETWORK="${NETWORK:-nat}"                 # nat | bridged
SUBNET="${SUBNET:-172.16.116}"            # vmnet8(NAT) 대역. bridged면 LAN 대역으로.
GATEWAY="${GATEWAY:-172.16.116.2}"        # VMware NAT 게이트웨이(.2)
DNS1="${DNS1:-172.16.116.2}"
DNS2="${DNS2:-8.8.8.8}"

USERNAME="${USERNAME:-quote}"
# 기본 비밀번호 'syncquote' 의 SHA-512 해시. 운영 시 'openssl passwd -6'로 교체 권장.
PWHASH="${PWHASH:-\$6\$iOUmRZ4hOSWe.RtM\$JyoqgWTSx5skS39GiFwa33TaN9C.m9d9zXKrYLw8lu.gPey8gPRjYU4gEWj5odSP7R6NGui8kQrP6AjzHsLsg.}"
SSH_PUBKEY="${SSH_PUBKEY:-}"              # 채우면 키 로그인 허용. 예) SSH_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)"

# VM 이름 → 호스트 IP 마지막 옥텟 (k8s=.10 / mysql=.100 / data=.101)
VMS=("sq-k8s-01" "sq-mysql-01" "sq-data-01")
declare -A OCTET=( [sq-k8s-01]=10 [sq-mysql-01]=100 [sq-data-01]=101 )

log() { echo -e "\033[1;36m[autoinstall]\033[0m $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "'$1' 필요 → sudo apt install -y xorriso genisoimage"; }

# ===== ISO 리마스터: autoinstall 커널 파라미터 주입 =====
remaster_iso() {
  need xorriso; need genisoimage
  [[ -f "$BASE_ISO" ]] || die "BASE_ISO 없음: $BASE_ISO"
  mkdir -p "$WORK_DIR"

  log "grub.cfg 추출"
  rm -f "$WORK_DIR/grub.cfg"
  xorriso -osirrox on -indev "$BASE_ISO" -extract /boot/grub/grub.cfg "$WORK_DIR/grub.cfg"
  chmod +w "$WORK_DIR/grub.cfg"

  log "커널 라인에 'autoinstall ds=nocloud' 주입 + 타임아웃 단축"
  sed -i -E '/vmlinuz/ s/([[:space:]])---/\1autoinstall ds=nocloud ---/' "$WORK_DIR/grub.cfg"
  sed -i -E 's/^(set[[:space:]]+timeout=).*/\11/' "$WORK_DIR/grub.cfg" || true

  log "ISO 재패킹(El Torito/EFI 부트 복제): $OUT_ISO"
  rm -f "$OUT_ISO"
  xorriso -indev "$BASE_ISO" -outdev "$OUT_ISO" \
    -boot_image any replay \
    -compliance no_emul_toc \
    -map "$WORK_DIR/grub.cfg" /boot/grub/grub.cfg
  log "리마스터 완료"
}

# ===== VM별 cloud-init seed ISO(CIDATA) 생성 =====
make_seed() {
  local vm="$1" ip="$2" vmdir="$WORK_DIR/$vm"
  mkdir -p "$vmdir"

  local keyblock=""
  if [[ -n "$SSH_PUBKEY" ]]; then
    keyblock=$'    authorized-keys:\n      - '"$SSH_PUBKEY"
  fi

  cat >"$vmdir/user-data" <<EOF
#cloud-config
autoinstall:
  version: 1
  locale: en_US.UTF-8
  keyboard: {layout: us}
  network:
    version: 2
    ethernets:
      primary:
        match: {driver: e1000e}
        dhcp4: false
        addresses: [${ip}/24]
        routes:
          - {to: default, via: ${GATEWAY}}
        nameservers:
          addresses: [${DNS1}, ${DNS2}]
  ssh:
    install-server: true
    allow-pw: true
${keyblock}
  packages:
    - open-vm-tools
  storage:
    layout: {name: direct}
  identity:
    hostname: ${vm}
    username: ${USERNAME}
    password: "${PWHASH}"
  late-commands:
    - curtin in-target --target=/target -- systemctl enable ssh
  shutdown: poweroff
EOF

  printf 'instance-id: %s\nlocal-hostname: %s\n' "$vm" "$vm" >"$vmdir/meta-data"

  rm -f "$vmdir/seed.iso"
  genisoimage -quiet -output "$vmdir/seed.iso" -volid CIDATA -joliet -rock \
    "$vmdir/user-data" "$vmdir/meta-data"
  log "  seed 생성: $vmdir/seed.iso  (ip=${ip}, host=${vm})"
}

# ===== vmx 재구성: 설치 ISO + seed ISO 연결, NAT 전환 =====
configure_vmx() {
  local vm="$1" seediso="$WORK_DIR/$vm/seed.iso"
  local V="$VM_ROOT/$vm/$vm.vmx"
  [[ -f "$V" ]] || die "vmx 없음: $V (먼저 create-syncquote-vms.sh create)"

  vmrun -T ws stop "$V" hard >/dev/null 2>&1 || true

  # sata0:1 = 리마스터한 설치 ISO
  sed -i "s|^sata0:1.fileName = .*|sata0:1.fileName = \"$OUT_ISO\"|" "$V"
  sed -i 's|^sata0:1.startConnected = .*|sata0:1.startConnected = "TRUE"|' "$V"
  sed -i 's|^sata0:1.autodetect = .*|sata0:1.autodetect = "FALSE"|' "$V"

  # sata0:2 = cloud-init seed ISO (중복 방지 후 추가)
  sed -i '/^sata0:2\./d' "$V"
  printf '\nsata0:2.present = "TRUE"\nsata0:2.deviceType = "cdrom-image"\nsata0:2.fileName = "%s"\nsata0:2.startConnected = "TRUE"\nsata0:2.autodetect = "FALSE"\n' "$seediso" >> "$V"

  # 네트워크
  if [[ "$NETWORK" == "nat" ]]; then
    sed -i 's/^ethernet0.connectionType = .*/ethernet0.connectionType = "nat"/' "$V"
  fi
  log "  vmx 재구성: $vm  (network=$NETWORK)"
}

cmd_build() {
  remaster_iso
  for vm in "${VMS[@]}"; do
    log "[$vm]"
    make_seed "$vm" "${SUBNET}.${OCTET[$vm]}"
    configure_vmx "$vm"
    vmrun -T ws start "$VM_ROOT/$vm/$vm.vmx" nogui
    log "  설치 시작됨(nogui): $vm"
  done
  echo
  log "3대 무인 설치 진행 중. 진행/완료 확인:"
  echo "    watch -n5 'vmrun -T ws list'   # 설치 끝나면 자동 poweroff → 목록에서 사라짐"
  echo "  모두 사라지면(=설치 완료):"
  echo "    $0 finalize                    # ISO 분리 후 디스크로 부팅"
}

cmd_finalize() {
  for vm in "${VMS[@]}"; do
    local V="$VM_ROOT/$vm/$vm.vmx"
    vmrun -T ws stop "$V" hard >/dev/null 2>&1 || true
    # 설치 미디어 분리 → 디스크 부팅
    sed -i 's|^sata0:1.startConnected = .*|sata0:1.startConnected = "FALSE"|' "$V"
    sed -i '/^sata0:2\./ s|startConnected = "TRUE"|startConnected = "FALSE"|' "$V"
    vmrun -T ws start "$V" nogui
    log "디스크 부팅: $vm  → ssh ${USERNAME}@${SUBNET}.${OCTET[$vm]}"
  done
  echo
  log "완료. 접속: ssh ${USERNAME}@${SUBNET}.10  (k8s) / .100 (mysql) / .101 (data)  비밀번호: 기본 'syncquote'"
}

case "${1:-build}" in
  build)    cmd_build ;;
  finalize) cmd_finalize ;;
  *) echo "Usage: $0 [build|finalize]"; exit 1 ;;
esac
