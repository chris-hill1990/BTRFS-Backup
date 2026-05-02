#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

INSTALL_DIR="${INSTALL_DIR:-/opt/backup}"
CONFIG_FILE="${CONFIG_FILE:-/etc/btrfs-backup.conf}"
CACHE_DIR="${CACHE_DIR:-/var/cache/btrfs-backup}"
CACHE_OWNER="${CACHE_OWNER:-${SUDO_USER:-root}}"
CACHE_GROUP="${CACHE_GROUP:-}"
CRON_FILE="${CRON_FILE:-/etc/cron.d/btrfs-backup}"
CRON_SCHEDULE="${CRON_SCHEDULE:-0 23 * * *}"
CRON_USER="${CRON_USER:-root}"
SYSTEMD_FILE="${SYSTEMD_FILE:-/etc/systemd/system/btrfs-backup-catalog.service}"
INSTALL_CATALOG_SERVICE="${INSTALL_CATALOG_SERVICE:-1}"
START_CATALOG_SERVICE="${START_CATALOG_SERVICE:-1}"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

require_root() {
  if (( EUID != 0 )); then
    die "Run this installer with sudo or as root."
  fi
}

require_file() {
  local file="$1"

  [[ -f "$SCRIPT_DIR/$file" ]] || die "Missing required file: $SCRIPT_DIR/$file"
}

require_go() {
  command -v go >/dev/null 2>&1 || die "Go is required to build backup-catalogd and backupctl. Install golang-go and rerun install.sh."
}

install_backup_files() {
  printf 'Installing backup tool to %s\n' "$INSTALL_DIR"

  install -d -m 0755 "$INSTALL_DIR"

  install -m 0644 "$SCRIPT_DIR/backup-common.sh" "$INSTALL_DIR/backup-common.sh"
  install -m 0644 "$SCRIPT_DIR/btrfs-backup.conf.example" "$INSTALL_DIR/btrfs-backup.conf.example"
  install -m 0644 "$SCRIPT_DIR/README.md" "$INSTALL_DIR/README.md"

  install -m 0755 "$SCRIPT_DIR/backup-snapshot-daily.sh" "$INSTALL_DIR/backup-snapshot-daily.sh"
  install -m 0755 "$SCRIPT_DIR/backup-sync-manual.sh" "$INSTALL_DIR/backup-sync-manual.sh"
  install -m 0755 "$SCRIPT_DIR/backup-restore.sh" "$INSTALL_DIR/backup-restore.sh"
  install -m 0755 "$SCRIPT_DIR/backup-gui.py" "$INSTALL_DIR/backup-gui.py"
  install -m 0755 "$SCRIPT_DIR/root-backup.sh" "$INSTALL_DIR/root-backup.sh"
  rm -f "$INSTALL_DIR/install.sh"
}

install_go_tools() {
  require_go

  printf 'Building Go catalog daemon: %s/backup-catalogd\n' "$INSTALL_DIR"
  (cd "$SCRIPT_DIR" && go build -o "$INSTALL_DIR/backup-catalogd" ./cmd/backup-catalogd)
  chmod 0755 "$INSTALL_DIR/backup-catalogd"

  printf 'Building Go backup CLI: %s/backupctl\n' "$INSTALL_DIR"
  (cd "$SCRIPT_DIR" && go build -o "$INSTALL_DIR/backupctl" ./cmd/backupctl)
  chmod 0755 "$INSTALL_DIR/backupctl"
}

install_config() {
  local config_dir

  config_dir="$(dirname -- "$CONFIG_FILE")"
  install -d -m 0755 "$config_dir"

  if [[ -e "$CONFIG_FILE" ]]; then
    printf 'Config already exists, leaving it unchanged: %s\n' "$CONFIG_FILE"
    return 0
  fi

  install -m 0644 "$SCRIPT_DIR/btrfs-backup.conf.example" "$CONFIG_FILE"
  printf 'Installed config file: %s\n' "$CONFIG_FILE"
}

install_cache_dir() {
  local owner="$CACHE_OWNER"
  local group="$CACHE_GROUP"

  if ! id -u "$owner" >/dev/null 2>&1; then
    owner="root"
  fi
  if [[ -z "$group" ]]; then
    group="$(id -gn "$owner")"
  fi

  install -d -o "$owner" -g "$group" -m 0700 "$CACHE_DIR"
  find "$CACHE_DIR" -maxdepth 1 -type f \( -name 'catalog-*' -o -name '.catalog-*' \) -exec chown "$owner:$group" {} +
  find "$CACHE_DIR" -maxdepth 1 -type f \( -name 'catalog-*' -o -name '.catalog-*' \) -exec chmod 0600 {} +
  printf 'Installed cache directory: %s (owner %s:%s)\n' "$CACHE_DIR" "$owner" "$group"
}

install_cron() {
  local cron_dir
  local cron_tmp

  cron_dir="$(dirname -- "$CRON_FILE")"
  cron_tmp="$(mktemp)"

  install -d -m 0755 "$cron_dir"

  cat > "$cron_tmp" <<CRON
# Installed by $INSTALL_DIR/install.sh
SHELL=/bin/bash
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
CONFIG_FILE=$CONFIG_FILE

$CRON_SCHEDULE $CRON_USER $INSTALL_DIR/backup-snapshot-daily.sh
CRON

  install -m 0644 "$cron_tmp" "$CRON_FILE"
  rm -f "$cron_tmp"

  printf 'Installed cron file: %s\n' "$CRON_FILE"
  printf 'Cron schedule: %s %s %s/backup-snapshot-daily.sh\n' "$CRON_SCHEDULE" "$CRON_USER" "$INSTALL_DIR"
}

install_desktop_file() {
  local desktop_dir
  local desktop_file
  local desktop_tmp

  desktop_dir="${DESKTOP_DIR:-/usr/local/share/applications}"
  desktop_file="$desktop_dir/btrfs-backup.desktop"
  desktop_tmp="$(mktemp)"

  install -d -m 0755 "$desktop_dir"

  cat > "$desktop_tmp" <<DESKTOP
[Desktop Entry]
Type=Application
Name=Btrfs Backup
Comment=Manage btrfs snapshots and restores
Exec=$INSTALL_DIR/backup-gui.py
Icon=drive-harddisk
Terminal=false
Categories=System;Filesystem;
StartupNotify=true
DESKTOP

  install -m 0644 "$desktop_tmp" "$desktop_file"
  rm -f "$desktop_tmp"

  printf 'Installed desktop launcher: %s\n' "$desktop_file"
}

install_catalog_service() {
  local systemd_dir
  local systemd_tmp

  if [[ "$INSTALL_CATALOG_SERVICE" != "1" ]]; then
    printf 'Skipping catalog daemon service install.\n'
    return 0
  fi

  systemd_dir="$(dirname -- "$SYSTEMD_FILE")"
  systemd_tmp="$(mktemp)"

  install -d -m 0755 "$systemd_dir"

  cat > "$systemd_tmp" <<SERVICE
[Unit]
Description=Btrfs Backup catalog cache daemon
Wants=network-online.target
After=local-fs.target network-online.target

[Service]
Type=simple
Environment=CONFIG_FILE=$CONFIG_FILE
ExecStart=$INSTALL_DIR/backup-catalogd --script-dir $INSTALL_DIR --config $CONFIG_FILE --source any
Restart=on-failure
RestartSec=30
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7

[Install]
WantedBy=multi-user.target
SERVICE

  install -m 0644 "$systemd_tmp" "$SYSTEMD_FILE"
  rm -f "$systemd_tmp"

  printf 'Installed catalog daemon service: %s\n' "$SYSTEMD_FILE"

  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl enable "$(basename -- "$SYSTEMD_FILE")"

    if [[ "$START_CATALOG_SERVICE" == "1" ]]; then
      systemctl restart "$(basename -- "$SYSTEMD_FILE")"
      printf 'Started catalog daemon service. Logs: journalctl -u %s -f\n' "$(basename -- "$SYSTEMD_FILE")"
    else
      printf 'Enabled catalog daemon service. Start it with: sudo systemctl start %s\n' "$(basename -- "$SYSTEMD_FILE")"
    fi
  else
    printf 'systemctl not found; service file installed but not enabled.\n'
  fi
}

main() {
  local file
  local required_files=(
    backup-common.sh
    btrfs-backup.conf.example
    backup-snapshot-daily.sh
    backup-sync-manual.sh
    backup-restore.sh
    backup-gui.py
    root-backup.sh
    README.md
    go.mod
    go.sum
    cmd/backup-catalogd/main.go
    cmd/backupctl/main.go
    internal/catalog/db.go
    internal/config/config.go
    internal/snapshots/snapshots.go
  )

  require_root
  require_go

  for file in "${required_files[@]}"; do
    require_file "$file"
  done

  install_backup_files
  install_go_tools
  install_config
  install_cache_dir
  install_cron
  install_desktop_file
  install_catalog_service

  printf '\nInstall complete.\n'
  printf 'Config: %s\n' "$CONFIG_FILE"
  printf 'Cache: %s\n' "$CACHE_DIR"
  printf 'Daily snapshots: %s/backup-snapshot-daily.sh\n' "$INSTALL_DIR"
  printf 'Manual sync: sudo %s/backup-sync-manual.sh\n' "$INSTALL_DIR"
  printf 'Status CLI: %s/backupctl status\n' "$INSTALL_DIR"
  if [[ "$INSTALL_CATALOG_SERVICE" == "1" ]]; then
    if command -v systemctl >/dev/null 2>&1; then
      printf 'Catalog daemon: sudo systemctl status %s\n' "$(basename -- "$SYSTEMD_FILE")"
    else
      printf 'Catalog daemon service file: %s (systemctl not found)\n' "$SYSTEMD_FILE"
    fi
  else
    printf 'Catalog daemon service: skipped\n'
  fi
  printf 'GUI: %s/backup-gui.py\n' "$INSTALL_DIR"
}

main "$@"
