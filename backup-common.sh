#!/usr/bin/env bash

# Shared configuration and helpers for the btrfs backup commands.
CONFIG_FILE="${CONFIG_FILE:-/etc/btrfs-backup.conf}"

BACKUP_MOUNT_ENV_SET=0
LOGFILE_ENV_SET=0
LOCAL_KEEP_ENV_SET=0
REMOTE_RETENTION_DAYS_ENV_SET=0
CACHE_DIR_ENV_SET=0
CATALOG_WORKERS_ENV_SET=0
CATALOG_PROGRESS_INTERVAL_ENV_SET=0
CATALOGD_INTERVAL_ENV_SET=0

if [[ ${BACKUP_MOUNT+x} ]]; then
  BACKUP_MOUNT_ENV="$BACKUP_MOUNT"
  BACKUP_MOUNT_ENV_SET=1
fi

if [[ ${LOGFILE+x} ]]; then
  LOGFILE_ENV="$LOGFILE"
  LOGFILE_ENV_SET=1
fi

if [[ ${LOCAL_KEEP+x} ]]; then
  LOCAL_KEEP_ENV="$LOCAL_KEEP"
  LOCAL_KEEP_ENV_SET=1
fi

if [[ ${REMOTE_RETENTION_DAYS+x} ]]; then
  REMOTE_RETENTION_DAYS_ENV="$REMOTE_RETENTION_DAYS"
  REMOTE_RETENTION_DAYS_ENV_SET=1
fi

if [[ ${CACHE_DIR+x} ]]; then
  CACHE_DIR_ENV="$CACHE_DIR"
  CACHE_DIR_ENV_SET=1
fi

if [[ ${CATALOG_WORKERS+x} ]]; then
  CATALOG_WORKERS_ENV="$CATALOG_WORKERS"
  CATALOG_WORKERS_ENV_SET=1
fi

if [[ ${CATALOG_PROGRESS_INTERVAL+x} ]]; then
  CATALOG_PROGRESS_INTERVAL_ENV="$CATALOG_PROGRESS_INTERVAL"
  CATALOG_PROGRESS_INTERVAL_ENV_SET=1
fi

if [[ ${CATALOGD_INTERVAL+x} ]]; then
  CATALOGD_INTERVAL_ENV="$CATALOGD_INTERVAL"
  CATALOGD_INTERVAL_ENV_SET=1
fi

BACKUP_MOUNT="/mnt/backup"
LOGFILE="/var/log/btrfs-backup.log"
LOCAL_KEEP=3
REMOTE_RETENTION_DAYS=365
CACHE_DIR="/var/cache/btrfs-backup"
CATALOG_WORKERS=4
CATALOG_PROGRESS_INTERVAL=5000
CATALOGD_INTERVAL="30m"

VOLUME_NAMES=(root drive3 drive4 drive2)

declare -Ag VOLUMES=(
  ["root"]="/"
  ["drive3"]="/mnt/drive3"
  ["drive4"]="/mnt/drive4"
  ["drive2"]="/mnt/drive2"
)

if [[ -r "$CONFIG_FILE" ]]; then
  # shellcheck source=/etc/btrfs-backup.conf
  source "$CONFIG_FILE"
fi

if (( BACKUP_MOUNT_ENV_SET )); then
  BACKUP_MOUNT="$BACKUP_MOUNT_ENV"
fi

if (( LOGFILE_ENV_SET )); then
  LOGFILE="$LOGFILE_ENV"
fi

if (( LOCAL_KEEP_ENV_SET )); then
  LOCAL_KEEP="$LOCAL_KEEP_ENV"
fi

if (( REMOTE_RETENTION_DAYS_ENV_SET )); then
  REMOTE_RETENTION_DAYS="$REMOTE_RETENTION_DAYS_ENV"
fi

if (( CACHE_DIR_ENV_SET )); then
  CACHE_DIR="$CACHE_DIR_ENV"
fi

if (( CATALOG_WORKERS_ENV_SET )); then
  CATALOG_WORKERS="$CATALOG_WORKERS_ENV"
fi

if (( CATALOG_PROGRESS_INTERVAL_ENV_SET )); then
  CATALOG_PROGRESS_INTERVAL="$CATALOG_PROGRESS_INTERVAL_ENV"
fi

if (( CATALOGD_INTERVAL_ENV_SET )); then
  CATALOGD_INTERVAL="$CATALOGD_INTERVAL_ENV"
fi

log() {
  printf '%s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

setup_logging() {
  local title="$1"

  if [[ -n "${LOGFILE:-}" ]]; then
    mkdir -p "$(dirname -- "$LOGFILE")"
    if [[ -t 1 ]]; then
      exec > >(tee -a "$LOGFILE") 2>&1
    else
      exec >> "$LOGFILE" 2>&1
    fi
  fi

  log "===== $title $(date -Is) ====="
}

require_btrfs() {
  command -v btrfs >/dev/null 2>&1 || die "btrfs command not found"
}

require_backup_mount() {
  command -v mountpoint >/dev/null 2>&1 || die "mountpoint command not found"
  mountpoint -q "$BACKUP_MOUNT" || die "Backup mount is not available: $BACKUP_MOUNT"
}

snapshot_dir_for() {
  local source="$1"

  if [[ "$source" == "/" ]]; then
    printf '/.snapshots\n'
  else
    printf '%s/.snapshots\n' "$source"
  fi
}

snapshot_name_for() {
  local name="$1"
  local snapshot_date="$2"

  printf '%s-%s\n' "$name" "$snapshot_date"
}

normalize_path() {
  local path="$1"

  realpath -m -- "$path"
}

volume_name_for_path() {
  local path="$1"
  local name
  local source
  local best_name=""
  local best_length=-1

  path="$(normalize_path "$path")"

  for name in "${VOLUME_NAMES[@]}"; do
    source="$(normalize_path "${VOLUMES[$name]}")"

    if [[ "$source" == "/" ]]; then
      if (( best_length < 1 )); then
        best_name="$name"
        best_length=1
      fi
      continue
    fi

    if [[ "$path" == "$source" || "$path" == "$source/"* ]]; then
      if (( ${#source} > best_length )); then
        best_name="$name"
        best_length=${#source}
      fi
    fi
  done

  [[ -n "$best_name" ]] || return 1
  printf '%s\n' "$best_name"
}

relative_path_for_volume() {
  local path="$1"
  local source="$2"

  path="$(normalize_path "$path")"
  source="$(normalize_path "$source")"

  if [[ "$source" == "/" ]]; then
    printf '%s\n' "${path#/}"
  elif [[ "$path" == "$source" ]]; then
    printf '\n'
  else
    printf '%s\n' "${path#"$source/"}"
  fi
}

snapshot_content_path() {
  local snapshot_root="$1"
  local relative_path="$2"

  if [[ -z "$relative_path" ]]; then
    printf '%s\n' "$snapshot_root"
  else
    printf '%s/%s\n' "$snapshot_root" "$relative_path"
  fi
}

list_snapshots() {
  local directory="$1"
  local name="$2"
  local base
  local suffix

  [[ -d "$directory" ]] || return 0

  {
    while IFS= read -r base; do
      suffix="${base#"$name-"}"
      if [[ "$suffix" != "$base" && "$suffix" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
        printf '%s\n' "$base"
      fi
    done < <(find "$directory" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null)
  } | sort
}

backupctl_path() {
  if [[ -x "${SCRIPT_DIR:-}/backupctl" ]]; then
    printf '%s/backupctl\n' "$SCRIPT_DIR"
  elif command -v backupctl >/dev/null 2>&1; then
    command -v backupctl
  else
    return 1
  fi
}

create_daily_snapshot() {
  local name="$1"
  local source="$2"
  local snapshot_date="$3"
  local snapshot_dir
  local snapshot_name
  local snapshot_path

  snapshot_dir="$(snapshot_dir_for "$source")"
  snapshot_name="$(snapshot_name_for "$name" "$snapshot_date")"
  snapshot_path="$snapshot_dir/$snapshot_name"

  log "----- Snapshot $name -----"
  log "Source: $source"
  log "Local snapshot directory: $snapshot_dir"

  [[ -d "$source" ]] || die "Source directory does not exist: $source"
  mkdir -p "$snapshot_dir"

  if [[ -e "$snapshot_path" && ! -d "$snapshot_path" ]]; then
    die "Snapshot path exists but is not a directory: $snapshot_path"
  fi

  if [[ -d "$snapshot_path" ]]; then
    log "Snapshot already exists: $snapshot_name"
    return 0
  fi

  log "Creating read-only snapshot: $snapshot_name"
  btrfs subvolume snapshot -r "$source" "$snapshot_path"
}

trim_local_snapshots() {
  local name="$1"
  local snapshot_dir="$2"
  local snapshots=()
  local delete_records=()
  local record
  local delete_count
  local snapshot
  local snapshot_path
  local backupctl
  local retention_tmp
  local i

  if backupctl="$(backupctl_path)"; then
    retention_tmp="$(mktemp)"
    if "$backupctl" --script-dir "$SCRIPT_DIR" --config "$CONFIG_FILE" retention --local --tsv "$name" > "$retention_tmp"; then
      mapfile -t delete_records < "$retention_tmp"
      rm -f "$retention_tmp"

      if (( ${#delete_records[@]} == 0 )); then
        mapfile -t snapshots < <(list_snapshots "$snapshot_dir" "$name")
        log "Local retention already satisfied for $name (${#snapshots[@]}/$LOCAL_KEEP snapshots)."
        return 0
      fi

      log "Trimming local snapshots for $name; deleting ${#delete_records[@]} and keeping last $LOCAL_KEEP."
      for record in "${delete_records[@]}"; do
        IFS=$'\t' read -r _ _ snapshot snapshot_path <<< "$record"
        log "Deleting local snapshot: $snapshot"
        btrfs subvolume delete "$snapshot_path"
        prune_catalog_snapshot "$name" "local" "$snapshot"
      done
      return 0
    fi

    rm -f "$retention_tmp"
    log "WARNING: backupctl retention failed for $name; falling back to shell retention."
  fi

  mapfile -t snapshots < <(list_snapshots "$snapshot_dir" "$name")

  if (( ${#snapshots[@]} <= LOCAL_KEEP )); then
    log "Local retention already satisfied for $name (${#snapshots[@]}/$LOCAL_KEEP snapshots)."
    return 0
  fi

  delete_count=$(( ${#snapshots[@]} - LOCAL_KEEP ))
  log "Trimming local snapshots for $name; deleting $delete_count and keeping last $LOCAL_KEEP."

  for (( i = 0; i < delete_count; i++ )); do
    snapshot="${snapshots[$i]}"
    log "Deleting local snapshot: $snapshot"
    btrfs subvolume delete "$snapshot_dir/$snapshot"
    prune_catalog_snapshot "$name" "local" "$snapshot"
  done
}

prune_catalog_snapshot() {
  local name="$1"
  local location="$2"
  local snapshot="$3"
  local catalogd
  local source_filter
  local db_file
  local pruned=0

  if [[ -x "${SCRIPT_DIR:-}/backup-catalogd" ]]; then
    catalogd="$SCRIPT_DIR/backup-catalogd"
  elif command -v backup-catalogd >/dev/null 2>&1; then
    catalogd="$(command -v backup-catalogd)"
  else
    log "Catalog prune skipped; backup-catalogd not found."
    return 0
  fi

  for source_filter in any "$location"; do
    db_file="$CACHE_DIR/catalog-$name-$source_filter.db"
    [[ -e "$db_file" || -e "$db_file-wal" ]] || continue

    log "Pruning catalog cache for deleted $location snapshot: $snapshot ($source_filter)"
    if "$catalogd" \
      --prune \
      --script-dir "$SCRIPT_DIR" \
      --config "$CONFIG_FILE" \
      --source "$source_filter" \
      --volume "$name" \
      --location "$location" \
      --snapshot "$snapshot"; then
      pruned=1
    else
      log "WARNING: Could not prune catalog cache for $snapshot ($source_filter)."
    fi
  done

  if (( ! pruned )); then
    log "No catalog cache needed pruning for $snapshot."
  fi
}

prune_remote_snapshots() {
  local name="$1"
  local backup_target="$2"
  local delete_records=()
  local record
  local old_snapshot
  local snapshot
  local snapshot_path
  local backupctl
  local retention_tmp

  log "Pruning remote snapshots for $name older than $REMOTE_RETENTION_DAYS days."

  if backupctl="$(backupctl_path)"; then
    retention_tmp="$(mktemp)"
    if "$backupctl" --script-dir "$SCRIPT_DIR" --config "$CONFIG_FILE" retention --remote --tsv "$name" > "$retention_tmp"; then
      mapfile -t delete_records < "$retention_tmp"
      rm -f "$retention_tmp"

      for record in "${delete_records[@]}"; do
        IFS=$'\t' read -r _ _ snapshot snapshot_path <<< "$record"
        log "Deleting remote snapshot: $snapshot"
        btrfs subvolume delete "$snapshot_path"
        prune_catalog_snapshot "$name" "remote" "$snapshot"
      done
      return 0
    fi

    rm -f "$retention_tmp"
    log "WARNING: backupctl retention failed for $name; falling back to shell retention."
  fi

  while IFS= read -r -d '' old_snapshot; do
    snapshot="$(basename -- "$old_snapshot")"
    log "Deleting remote snapshot: $snapshot"
    btrfs subvolume delete "$old_snapshot"
    prune_catalog_snapshot "$name" "remote" "$snapshot"
  done < <(
    find "$backup_target" \
      -mindepth 1 \
      -maxdepth 1 \
      -type d \
      -name "$name-*" \
      -mtime +"$REMOTE_RETENTION_DAYS" \
      -print0
  )
}
