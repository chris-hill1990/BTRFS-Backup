#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<USAGE
Usage: $(basename "$0") <command>

Commands:
  snapshot   Create today's read-only local snapshots. Use this from cron.
  sync       Manually sync every missing local snapshot to the backup target.
  status     Show backup mount and per-volume snapshot status.
  snapshots  List local and backup-target snapshots.
  files      List files and directories inside a snapshot.
  catalog    List files and directories found across snapshots for a volume.
  cache      Build or resume cached file catalogs.
  catalogd   Run the Go catalog cache daemon.
  versions   List snapshots that contain a file.
  restore    Restore a file from a snapshot to a new path.
  run        Create today's snapshots, then run the manual sync.
  help       Show this help.

Environment overrides:
  CONFIG_FILE=/etc/btrfs-backup.conf
  BACKUP_MOUNT=/mnt/backup
  LOGFILE=/var/log/btrfs-backup.log
  LOCAL_KEEP=3
  REMOTE_RETENTION_DAYS=365
  CACHE_DIR=/var/cache/btrfs-backup
  CATALOG_WORKERS=4
  CATALOG_PROGRESS_INTERVAL=5000
  CATALOGD_INTERVAL=30m
USAGE
}

case "${1:-help}" in
  snapshot | daily)
    exec "$SCRIPT_DIR/backup-snapshot-daily.sh"
    ;;
  sync | manual)
    exec "$SCRIPT_DIR/backup-sync-manual.sh"
    ;;
  status | snapshots)
    command="$1"
    shift
    if [[ -x "$SCRIPT_DIR/backupctl" ]]; then
      exec "$SCRIPT_DIR/backupctl" --script-dir "$SCRIPT_DIR" --config "${CONFIG_FILE:-/etc/btrfs-backup.conf}" "$command" "$@"
    fi
    exec "$SCRIPT_DIR/backup-restore.sh" "$command" "$@"
    ;;
  files | catalog | cache | versions | restore)
    command="$1"
    shift
    exec "$SCRIPT_DIR/backup-restore.sh" "$command" "$@"
    ;;
  catalogd)
    shift
    exec "$SCRIPT_DIR/backup-catalogd" --script-dir "$SCRIPT_DIR" "$@"
    ;;
  run)
    "$SCRIPT_DIR/backup-snapshot-daily.sh"
    exec "$SCRIPT_DIR/backup-sync-manual.sh"
    ;;
  help | -h | --help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
