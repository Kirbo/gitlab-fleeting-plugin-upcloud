#!/usr/bin/env bash
# custom-image-debian13.sh
# Combined setup + prepare script for Debian 13 GitLab Runner images
#
# Usage:
#   sudo ./custom-image-debian13.sh --setup            # only install & configure
#   sudo ./custom-image-debian13.sh --prepare          # only final cleanup (no shutdown)
#   sudo ./custom-image-debian13.sh --all              # do setup then prepare (default)
#   sudo ./custom-image-debian13.sh --all --poweroff   # do all, then power off on success
#
# The script is idempotent and prints progress. Run as root.

set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

if [[ "$(id -u)" -ne 0 ]]; then
  echo "ERROR: must be run as root (sudo)." >&2
  exit 1
fi

# ---------------------------
# Visual helpers (banner + colors)
# ---------------------------
if [[ -t 1 ]]; then
  BOLD='\033[1m'
  RESET='\033[0m'
  BLUE='\033[1;34m'
  GREEN='\033[1;32m'
  YELLOW='\033[1;33m'
  RED='\033[1;31m'
else
  BOLD=''
  RESET=''
  BLUE=''
  GREEN=''
  YELLOW=''
  RED=''
fi

# total steps — estimate for nicer banners (adjust if you add/remove steps)
TOTAL_STEPS=21
CURRENT_STEP=0
step_width=78

print_banner() {
  local text="$1"
  local tlen=${#text}
  local width=${step_width}
  if (( tlen + 4 >= width )); then
    width=$((tlen + 8))
  fi
  local pad=$(( (width - tlen - 2) / 2 ))
  local left
  local right
  left=$(printf -- '%*s' "$pad" '' | tr ' ' '=')
  right=$(printf -- '%*s' "$pad" '' | tr ' ' '=')
  if (( (width - tlen - 2) % 2 != 0 )); then
    right="${right}="
  fi
  printf -- "\n${BLUE}${BOLD}%s %s %s${RESET}\n\n" "$left" "$text" "$right"
}

next_step() {
  CURRENT_STEP=$((CURRENT_STEP+1))
  local text="STEP ${CURRENT_STEP} / ${TOTAL_STEPS} - $1"
  print_banner "$text"
}

info()   { printf -- "${GREEN}--> %s${RESET}\n" "$1"; }
warn()   { printf -- "${YELLOW}⚠ %s${RESET}\n" "$1"; }
error()  { printf -- "${RED}✖ %s${RESET}\n" "$1"; }
log()    { printf -- '%s %s\n' "$(date --iso-8601=seconds)" "$*"; }

safe_systemctl_stop() { systemctl stop "$1" 2>/dev/null || true; }
safe_systemctl_enable_now() { systemctl enable --now "$1" 2>/dev/null || true; }

# ---------------------------
# setup function
# ---------------------------
do_setup() {
  next_step "System update & upgrade"
  info "Running apt update"
  apt update -y
  info "Running apt upgrade (non-interactive)"
  apt upgrade -y

  next_step "Install base packages (locales, cloud-init, curl, gpg...)"
  info "Installing locales, cloud-init, curl, gnupg, lsb-release, apt-transport-https"
  apt install -y --no-install-recommends \
    locales \
    cloud-init \
    ca-certificates \
    curl \
    gnupg \
    lsb-release \
    apt-transport-https

  # Fix bad LC_CTYPE if present
  if grep -q '^LC_CTYPE' /etc/environment 2>/dev/null || true; then
    next_step "Fixing invalid LC_CTYPE in /etc/environment (if present)"
    info "Removing LC_CTYPE from /etc/environment"
    sed -i '/^LC_CTYPE/d' /etc/environment || true
  fi

  next_step "Configure locales"
  info "Setting LANG=C.UTF-8 and generating locales"
  update-locale LANG=C.UTF-8 LC_ALL=C.UTF-8 || true
  locale-gen C.UTF-8 || true
  locale-gen en_US.UTF-8 || true

  next_step "Installing Docker (official repository)"
  info "Adding Docker repo GPG key and apt source"
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) stable" \
    > /etc/apt/sources.list.d/docker.list

  info "Updating apt and installing docker-ce, containerd, buildx, compose plugin"
  apt update -y
  apt install -y --no-install-recommends \
    docker-ce docker-ce-cli containerd.io \
    docker-buildx-plugin docker-compose-plugin

  next_step "Docker daemon configuration"
  DOCKER_DAEMON_JSON='/etc/docker/daemon.json'
  if [[ ! -f "${DOCKER_DAEMON_JSON}" ]]; then
    info "Writing recommended Docker daemon config to ${DOCKER_DAEMON_JSON}"
    cat > "${DOCKER_DAEMON_JSON}" <<'EOF'
{
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  },
  "features": {
    "buildkit": true
  }
}
EOF
  else
    info "${DOCKER_DAEMON_JSON} exists — leaving unchanged"
  fi

  info "Enabling and starting docker"
  safe_systemctl_enable_now docker

  if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
    info "Adding ${SUDO_USER} to docker group"
    usermod -aG docker "${SUDO_USER}" || true
  fi

  next_step "Install GitLab Runner package (do NOT register)"
  info "Adding gitlab-runner repository and installing package"
  curl -fsSL https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.deb.sh | bash
  apt update -y
  apt install -y --no-install-recommends gitlab-runner || true
  safe_systemctl_enable_now gitlab-runner || true

  next_step "Ensure cloud-init & SSH host keys"
  info "cloud-init status (non-fatal)"
  cloud-init status --long || true
  info "Ensure SSH host keys exist (so current session stays accessible)"
  ssh-keygen -A || true
  chown -R root:root /etc/ssh || true
  chmod 755 /etc/ssh || true
  chmod 644 /etc/ssh/*pub 2>/dev/null || true
  chmod 600 /etc/ssh/ssh_host_* 2>/dev/null || true

  next_step "Clean baked runner config (if any) and verify"
  if [[ -f /etc/gitlab-runner/config.toml ]]; then
    info "Removing /etc/gitlab-runner/config.toml to avoid baking registration"
    rm -f /etc/gitlab-runner/config.toml || true
  fi

  info "Quick verification: docker & gitlab-runner versions"
  docker --version || true
  docker buildx version || true
  docker compose version || true
  gitlab-runner --version || true

  next_step "Setup finished"
  info "Setup steps completed — continue interactive config if needed"
}

# ---------------------------
# prepare function
# ---------------------------
do_prepare() {
  next_step "Stopping services to avoid writes"
  info "Stopping gitlab-runner and docker (if running)"
  safe_systemctl_stop gitlab-runner
  safe_systemctl_stop docker

  next_step "Remove GitLab Runner config & SSH host keys"
  info "Removing /etc/gitlab-runner/config.toml"
  rm -f /etc/gitlab-runner/config.toml 2>/dev/null || true
  info "Removing SSH host keys (they will be re-generated at first boot)"
  rm -f /etc/ssh/ssh_host_* 2>/dev/null || true

  next_step "Remove authorized_keys and clear machine-id"
  info "Removing root and home authorized_keys"
  rm -f /root/.ssh/authorized_keys 2>/dev/null || true
  rm -f /home/*/.ssh/authorized_keys 2>/dev/null || true
  info "Clearing machine-id and dbus machine-id"
  truncate -s 0 /etc/machine-id 2>/dev/null || true
  rm -f /var/lib/dbus/machine-id 2>/dev/null || true

  next_step "APT cache & lists cleanup"
  info "Cleaning apt caches"
  apt clean || true
  rm -rf /var/lib/apt/lists/* 2>/dev/null || true

  next_step "Temporary files cleanup"
  info "Clearing /tmp"
  rm -rf /tmp/* 2>/dev/null || true

  next_step "Logs & journal cleanup"
  info "Rotating and vacuuming journal, removing /var/log/*"
  journalctl --rotate || true
  journalctl --vacuum-time=1s || true
  rm -rf /var/log/* 2>/dev/null || true
  mkdir -p /var/log && chmod 755 /var/log || true

  next_step "Clear shell histories and caches"
  info "Clearing history for root and users in /home"
  history -c || true
  history -w || true
  rm -f /root/.bash_history 2>/dev/null || true
  for uhome in /home/*; do
    if [[ -d "$uhome" ]]; then
      rm -f "${uhome}/.bash_history" 2>/dev/null || true
      rm -f "${uhome}/.zsh_history" 2>/dev/null || true
    fi
  done
  info "Removing various caches"
  rm -f /root/.wget-hsts 2>/dev/null || true
  rm -rf /var/cache/* 2>/dev/null || true

  next_step "Sync and zero free space"
  info "Sync filesystem"
  sync
  info "Zeroing free space for better compression (may take a while)"
  dd if=/dev/zero of=/zerofill bs=1M 2>/dev/null || true
  sync
  rm -f /zerofill 2>/dev/null || true
  sync

  next_step "Final sync"
  sync

  next_step "Prepare finished"
  info "Image prep complete. System NOT powered off by this script."
  info "Now run: sudo poweroff  (and create the custom image from stopped server)"
}

# ---------------------------
# Argument parsing (with optional --poweroff)
# ---------------------------
# Default mode
MODE="--all"
DO_POWEROFF=1

# parse args (accepts --setup, --prepare, --all, --poweroff)
for a in "$@"; do
  case "$a" in
    --setup|--prepare|--all) MODE="$a" ;;
    --poweroff) DO_POWEROFF=1 ;;
    -h|--help) MODE="--help" ;;
    *) echo "Warning: unknown argument '$a' (ignoring)" >&2 ;;
  esac
done

case "${MODE}" in
  --setup)  do_setup ;;
  --prepare) do_prepare ;;
  --all) do_setup; do_prepare ;;
  --help)
    cat <<'USAGE'
Usage: custom-image-debian13.sh [--setup|--prepare|--all] [--poweroff]
  --setup      Install & configure packages (Docker, gitlab-runner, locales, cloud-init)
  --prepare    Final pre-image cleanup: remove keys, logs, caches, zero free space (no shutdown)
  --all        Run setup then prepare (default)
  --poweroff   Power off the machine at the end of a successful run
  -h,--help    Show this help
USAGE
    ;;
  *)
    echo "Unknown mode: ${MODE}" >&2
    exit 2
    ;;
esac

# ---------------------------
# Optional poweroff
# ---------------------------
if (( DO_POWEROFF )); then
  next_step "Powering off the machine (requested via --poweroff)"
  info "Syncing disks and powering off now"
  sync
  # give a moment for logs to flush
  sleep 2
  # Use systemctl poweroff for a clean shutdown
  systemctl poweroff -i
fi

exit 0
