#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/backup-common.sh"

sync_volume() {
  local name="$1"
  local source="$2"
  local snapshot_dir
  local backup_target
  local local_snapshots=()
  local remote_snapshot
  local snapshot
  local parent=""
  local parent_available=0
  local sent_count=0

  snapshot_dir="$(snapshot_dir_for "$source")"
  backup_target="$BACKUP_MOUNT/$name"

  log "----- Sync $name -----"
  log "Local snapshot directory: $snapshot_dir"
  log "Backup target: $backup_target"

  [[ -d "$snapshot_dir" ]] || die "Local snapshot directory does not exist: $snapshot_dir"
  mkdir -p "$backup_target"

  mapfile -t local_snapshots < <(list_snapshots "$snapshot_dir" "$name")

  if (( ${#local_snapshots[@]} == 0 )); then
    log "No local snapshots found for $name."
    return 0
  fi

  for snapshot in "${local_snapshots[@]}"; do
    remote_snapshot="$backup_target/$snapshot"

    if [[ -d "$remote_snapshot" ]]; then
      log "Backup target already has: $snapshot"
      parent="$snapshot"
      parent_available=1
      continue
    fi

    if (( parent_available )); then
      log "Sending missing snapshot incrementally: $snapshot (parent: $parent)"
      btrfs send -p "$snapshot_dir/$parent" "$snapshot_dir/$snapshot" \
        | btrfs receive "$backup_target"
    else
      log "Sending missing snapshot as full stream: $snapshot"
      btrfs send "$snapshot_dir/$snapshot" \
        | btrfs receive "$backup_target"
    fi

    parent="$snapshot"
    parent_available=1
    sent_count=$(( sent_count + 1 ))
  done

  if (( sent_count == 0 )); then
    log "Backup target already had every local snapshot for $name."
  else
    log "Sent $sent_count missing snapshot(s) for $name."
  fi

  trim_local_snapshots "$name" "$snapshot_dir"
  prune_remote_snapshots "$name" "$backup_target"
}

main() {
  local name

  setup_logging "Manual backup sync run"
  require_btrfs
  require_backup_mount

  for name in "${VOLUME_NAMES[@]}"; do
    sync_volume "$name" "${VOLUMES[$name]}"
    log
  done

  log "Manual backup sync run complete."
}

main "$@"
