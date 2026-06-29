#!/usr/bin/env bash
set -euo pipefail

ISO_PATH="${ISO_PATH:-/home/quote-team/iso/ubuntu-22.04.5-live-server-amd64.iso}"
VM_ROOT="${VM_ROOT:-/home/quote-team/VMWare/VMX}"
NETWORK="${NETWORK:-bridged}"
DISK_CONTROLLER_TYPE="${DISK_CONTROLLER_TYPE:-lsilogic}"
HEADLESS="${HEADLESS:-false}"
FORCE="${FORCE:-false}"

usage() {
  cat <<'EOF'
Usage:
  ./create-syncquote-vms.sh [create|start|stop|list|status]

Environment overrides:
  ISO_PATH=/home/quote-team/iso/ubuntu-22.04.5-live-server-amd64.iso
  VM_ROOT=/home/quote-team/VMWare/VMX
  NETWORK=bridged|nat
  DISK_CONTROLLER_TYPE=lsilogic|lsisas|pvscsi
  HEADLESS=true|false
  FORCE=true|false

Examples:
  ./create-syncquote-vms.sh create
  ./create-syncquote-vms.sh start
  HEADLESS=true ./create-syncquote-vms.sh start
  ./create-syncquote-vms.sh list

VM plan:
  sq-k8s-01    4 vCPU / 6144 MB / 45 GB  - K8s, API, Ingress, Collector, Archiver
  sq-data-01   4 vCPU / 8192 MB / 55 GB  - Redis
  sq-mysql-01  2 vCPU / 8192 MB / 75 GB  - MySQL
  Total disk: 175 GB
EOF
}

require_command() {
  local command_name="$1"
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "ERROR: '${command_name}' command is required but was not found in PATH." >&2
    exit 1
  fi
}

vmrun_gui_mode() {
  if [[ "${HEADLESS}" == "true" ]]; then
    echo "nogui"
  else
    echo "gui"
  fi
}

network_line() {
  case "${NETWORK}" in
    bridged)
      echo 'ethernet0.connectionType = "bridged"'
      ;;
    nat)
      echo 'ethernet0.connectionType = "nat"'
      ;;
    *)
      echo "ERROR: NETWORK must be 'bridged' or 'nat'." >&2
      exit 1
      ;;
  esac
}

write_vmx() {
  local vm_name="$1"
  local vmx_path="$2"
  local mem_mb="$3"
  local cpus="$4"
  local cores_per_socket="$5"
  local disk_file="$6"

  cat >"${vmx_path}" <<EOF
.encoding = "UTF-8"
config.version = "8"
virtualHW.version = "21"
displayName = "${vm_name}"
guestOS = "ubuntu-64"
firmware = "efi"
uefi.secureBoot.enabled = "FALSE"

memsize = "${mem_mb}"
numvcpus = "${cpus}"
cpuid.coresPerSocket = "${cores_per_socket}"

$(network_line)
ethernet0.present = "TRUE"
ethernet0.virtualDev = "e1000e"
ethernet0.addressType = "generated"

scsi0.present = "TRUE"
scsi0.virtualDev = "${DISK_CONTROLLER_TYPE}"
scsi0:0.present = "TRUE"
scsi0:0.fileName = "${disk_file}"
scsi0:0.redo = ""

sata0.present = "TRUE"
sata0:1.present = "TRUE"
sata0:1.deviceType = "cdrom-image"
sata0:1.fileName = "${ISO_PATH}"
sata0:1.startConnected = "TRUE"
sata0:1.autodetect = "FALSE"

usb.present = "TRUE"
ehci.present = "TRUE"
ehci.pciSlotNumber = "35"
usb_xhci.present = "TRUE"
sound.present = "FALSE"

floppy0.present = "FALSE"
tools.syncTime = "TRUE"
isolation.tools.copy.disable = "FALSE"
isolation.tools.paste.disable = "FALSE"

svga.graphicsMemoryKB = "8388608"
vmci0.present = "TRUE"

checkpoint.vmState = ""
EOF
}

create_vm() {
  local vm_name="$1"
  local cpus="$2"
  local cores_per_socket="$3"
  local mem_mb="$4"
  local disk_gb="$5"

  local vm_dir="${VM_ROOT}/${vm_name}"
  local vmx_path="${vm_dir}/${vm_name}.vmx"
  local disk_file="${vm_name}.vmdk"
  local disk_path="${vm_dir}/${disk_file}"

  if [[ -e "${vm_dir}" && "${FORCE}" != "true" ]]; then
    echo "SKIP: ${vm_dir} already exists. Set FORCE=true to overwrite VMX metadata."
    return
  fi

  mkdir -p "${vm_dir}"

  if [[ ! -e "${disk_path}" ]]; then
    echo "CREATE: ${disk_path} (${disk_gb}GB sparse disk)"
    vmware-vdiskmanager -c -s "${disk_gb}GB" -a "${DISK_CONTROLLER_TYPE}" -t 0 "${disk_path}"
  else
    echo "KEEP: ${disk_path} already exists."
  fi

  echo "WRITE: ${vmx_path}"
  write_vmx "${vm_name}" "${vmx_path}" "${mem_mb}" "${cpus}" "${cores_per_socket}" "${disk_file}"
}

create_all() {
  require_command vmware-vdiskmanager

  if [[ ! -f "${ISO_PATH}" ]]; then
    echo "ERROR: ISO file does not exist: ${ISO_PATH}" >&2
    exit 1
  fi

  mkdir -p "${VM_ROOT}"

  create_vm "sq-k8s-01" "4" "2" "6144" "45"
  create_vm "sq-data-01" "4" "2" "8192" "55"
  create_vm "sq-mysql-01" "2" "1" "8192" "75"

  echo
  echo "VMX files are ready under ${VM_ROOT}."
  echo "Start them with: $0 start"
}

vmx_path_for() {
  local vm_name="$1"
  echo "${VM_ROOT}/${vm_name}/${vm_name}.vmx"
}

start_all() {
  require_command vmrun
  local mode
  mode="$(vmrun_gui_mode)"

  for vm_name in sq-k8s-01 sq-data-01 sq-mysql-01; do
    local vmx_path
    vmx_path="$(vmx_path_for "${vm_name}")"
    if [[ ! -f "${vmx_path}" ]]; then
      echo "ERROR: VMX file not found: ${vmx_path}" >&2
      exit 1
    fi
    echo "START: ${vm_name} (${mode})"
    vmrun -T ws start "${vmx_path}" "${mode}"
  done
}

stop_all() {
  require_command vmrun

  for vm_name in sq-k8s-01 sq-data-01 sq-mysql-01; do
    local vmx_path
    vmx_path="$(vmx_path_for "${vm_name}")"
    if [[ -f "${vmx_path}" ]]; then
      echo "STOP: ${vm_name}"
      vmrun -T ws stop "${vmx_path}" soft || vmrun -T ws stop "${vmx_path}" hard
    fi
  done
}

list_all() {
  for vm_name in sq-k8s-01 sq-data-01 sq-mysql-01; do
    local vmx_path
    vmx_path="$(vmx_path_for "${vm_name}")"
    if [[ -f "${vmx_path}" ]]; then
      echo "${vm_name}: ${vmx_path}"
    else
      echo "${vm_name}: missing (${vmx_path})"
    fi
  done
}

status_all() {
  require_command vmrun
  vmrun -T ws list
}

case "${1:-}" in
  create)
    create_all
    ;;
  start)
    start_all
    ;;
  stop)
    stop_all
    ;;
  list)
    list_all
    ;;
  status)
    status_all
    ;;
  -h|--help|help|"")
    usage
    ;;
  *)
    echo "ERROR: unknown command '${1}'." >&2
    usage >&2
    exit 1
    ;;
esac
