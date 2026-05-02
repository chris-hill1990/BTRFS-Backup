#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/backup-common.sh"

usage() {
  cat <<USAGE
Usage: $(basename "$0") <command> [options]

Commands:
  status [--json]
      Show backup mount and per-volume snapshot status.

  snapshots [--json] [--local|--remote] [volume]
      List known local and/or backup-target snapshots.

  files [--json] --snapshot <snapshot> [--volume <volume>] [--source local|remote|any] [--path <relative-path>]
      List files and directories inside a snapshot directory.

  catalog [--json] --volume <volume> [--source local|remote|any] [--path <relative-path>] [--refresh-cache|--reset-cache|--stale-cache|--no-cache]
      List files and directories that exist in any snapshot for a volume.

  cache [--volume <volume>] [--source local|remote|any] [--reset-cache]
      Build or resume cached file catalogs with backup-catalogd. Process all volumes by default.

  versions [--json] <file>
      List snapshots that contain a version of the file.

  restore [--json] <file> --version <snapshot> --to <path> [--source local|remote|any]
      Restore a file from a snapshot to a new path. Existing destination paths
      are not overwritten.
USAGE
}

json_escape() {
  local value="$1"

  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "$value"
}

json_string() {
  printf '"'
  json_escape "$1"
  printf '"'
}

json_bool() {
  if (($1)); then
    printf 'true'
  else
    printf 'false'
  fi
}

progress_log() {
  printf '%s\n' "$*" >&2
}

backup_mount_is_mounted() {
  command -v mountpoint >/dev/null 2>&1 && mountpoint -q "$BACKUP_MOUNT"
}

snapshot_location_path() {
  local location="$1"
  local name="$2"
  local source="$3"

  case "$location" in
    local)
      snapshot_dir_for "$source"
      ;;
    remote)
      printf '%s/%s\n' "$BACKUP_MOUNT" "$name"
      ;;
    *)
      die "Unknown snapshot location: $location"
      ;;
  esac
}

list_snapshot_records() {
  local location_filter="$1"
  local volume_filter="$2"
  local name
  local source
  local location
  local directory
  local snapshot

  for name in "${VOLUME_NAMES[@]}"; do
    if [[ -n "$volume_filter" && "$name" != "$volume_filter" ]]; then
      continue
    fi

    source="${VOLUMES[$name]}"

    for location in local remote; do
      if [[ "$location_filter" != "all" && "$location" != "$location_filter" ]]; then
        continue
      fi

      directory="$(snapshot_location_path "$location" "$name" "$source")"

      while IFS= read -r snapshot; do
        printf '%s\t%s\t%s\t%s\n' "$name" "$location" "$snapshot" "$directory/$snapshot"
      done < <(list_snapshots "$directory" "$name")
    done
  done
}

infer_volume_for_snapshot() {
  local snapshot="$1"
  local name
  local best_name=""
  local best_length=-1

  for name in "${VOLUME_NAMES[@]}"; do
    if [[ "$snapshot" == "$name-"* && ${#name} -gt best_length ]]; then
      best_name="$name"
      best_length=${#name}
    fi
  done

  [[ -n "$best_name" ]] || return 1
  printf '%s\n' "$best_name"
}

snapshot_record_for() {
  local snapshot="$1"
  local volume="$2"
  local source_filter="$3"
  local location
  local source
  local directory
  local snapshot_root

  if [[ -z "$volume" ]]; then
    volume="$(infer_volume_for_snapshot "$snapshot")" || die "Could not infer volume from snapshot name: $snapshot"
  fi

  [[ -n "${VOLUMES[$volume]+x}" ]] || die "Unknown volume: $volume"

  source="${VOLUMES[$volume]}"

  for location in local remote; do
    if [[ "$source_filter" != "any" && "$location" != "$source_filter" ]]; then
      continue
    fi

    directory="$(snapshot_location_path "$location" "$volume" "$source")"
    snapshot_root="$directory/$snapshot"

    if [[ -d "$snapshot_root" ]]; then
      printf '%s\t%s\t%s\t%s\n' "$volume" "$location" "$source" "$snapshot_root"
      return 0
    fi
  done

  return 1
}

clean_snapshot_relative_path() {
  local relative_path="$1"

  if [[ -z "$relative_path" || "$relative_path" == "." ]]; then
    printf '\n'
    return 0
  fi

  [[ "$relative_path" != /* ]] || die "Snapshot path must be relative: $relative_path"

  while [[ "$relative_path" == ./* ]]; do
    relative_path="${relative_path#./}"
  done

  relative_path="${relative_path%/}"

  case "/$relative_path/" in
    */../*) die "Snapshot path cannot contain '..': $relative_path" ;;
  esac

  printf '%s\n' "$relative_path"
}

live_path_for_snapshot_relative() {
  local source="$1"
  local relative_path="$2"

  source="$(normalize_path "$source")"

  if [[ -z "$relative_path" ]]; then
    printf '%s\n' "$source"
  elif [[ "$source" == "/" ]]; then
    printf '/%s\n' "$relative_path"
  else
    printf '%s/%s\n' "$source" "$relative_path"
  fi
}

snapshot_entry_type() {
  local path="$1"

  if [[ -L "$path" ]]; then
    printf 'symlink\n'
  elif [[ -d "$path" ]]; then
    printf 'directory\n'
  elif [[ -f "$path" ]]; then
    printf 'file\n'
  else
    printf 'other\n'
  fi
}

cmd_files() {
  local json=0
  local snapshot=""
  local volume=""
  local source_filter="any"
  local relative_path=""
  local record
  local record_volume
  local record_location
  local source
  local snapshot_root
  local browse_path
  local resolved_snapshot_root
  local resolved_browse_path
  local entry
  local name
  local entry_type
  local entry_relative
  local live_path
  local size
  local modified
  local first=1

  while (($#)); do
    case "$1" in
      --json)
        json=1
        ;;
      --snapshot)
        shift
        (($#)) || die "--snapshot requires a snapshot name"
        snapshot="$1"
        ;;
      --volume)
        shift
        (($#)) || die "--volume requires a volume name"
        volume="$1"
        ;;
      --source)
        shift
        (($#)) || die "--source requires local, remote, or any"
        source_filter="$1"
        ;;
      --path)
        shift
        (($#)) || die "--path requires a relative path"
        relative_path="$1"
        ;;
      -h | --help)
        usage
        return 0
        ;;
      -*)
        die "Unknown files option: $1"
        ;;
      *)
        die "Unexpected files argument: $1"
        ;;
    esac
    shift
  done

  case "$source_filter" in
    local | remote | any) ;;
    *) die "--source must be local, remote, or any" ;;
  esac

  [[ -n "$snapshot" ]] || die "files requires --snapshot"

  relative_path="$(clean_snapshot_relative_path "$relative_path")"
  record="$(snapshot_record_for "$snapshot" "$volume" "$source_filter")" \
    || die "Snapshot not found: $snapshot"
  IFS=$'\t' read -r record_volume record_location source snapshot_root <<< "$record"
  browse_path="$(snapshot_content_path "$snapshot_root" "$relative_path")"
  resolved_snapshot_root="$(realpath -m -- "$snapshot_root")"
  resolved_browse_path="$(realpath -m -- "$browse_path")"

  if [[ "$resolved_browse_path" != "$resolved_snapshot_root" && "$resolved_browse_path" != "$resolved_snapshot_root/"* ]]; then
    die "Snapshot path escapes the selected snapshot: $relative_path"
  fi

  [[ -d "$browse_path" && ! -L "$browse_path" ]] || die "Snapshot path is not a directory: $relative_path"

  if (( json )); then
    printf '{"volume":'
    json_string "$record_volume"
    printf ',"location":'
    json_string "$record_location"
    printf ',"snapshot":'
    json_string "$snapshot"
    printf ',"path":'
    json_string "$relative_path"
    printf ',"snapshot_path":'
    json_string "$browse_path"
    printf ',"entries":['

    while IFS= read -r -d '' entry; do
      name="$(basename -- "$entry")"
      entry_type="$(snapshot_entry_type "$entry")"
      if [[ -z "$relative_path" ]]; then
        entry_relative="$name"
      else
        entry_relative="$relative_path/$name"
      fi
      live_path="$(live_path_for_snapshot_relative "$source" "$entry_relative")"
      size="$(stat -c '%s' -- "$entry" 2>/dev/null || printf '0')"
      modified="$(stat -c '%Y' -- "$entry" 2>/dev/null || printf '0')"

      if (( first )); then
        first=0
      else
        printf ','
      fi

      printf '{"name":'
      json_string "$name"
      printf ',"type":'
      json_string "$entry_type"
      printf ',"relative_path":'
      json_string "$entry_relative"
      printf ',"path":'
      json_string "$entry"
      printf ',"restore_path":'
      json_string "$live_path"
      printf ',"size":%s' "$size"
      printf ',"modified":%s' "$modified"
      printf '}'
    done < <(find "$browse_path" -mindepth 1 -maxdepth 1 -print0 | sort -z)

    printf ']}\n'
    return 0
  fi

  printf 'Snapshot: %s (%s %s)\n' "$snapshot" "$record_volume" "$record_location"
  printf 'Path: /%s\n' "$relative_path"
  printf '%-10s %-12s %s\n' "TYPE" "SIZE" "NAME"

  while IFS= read -r -d '' entry; do
    name="$(basename -- "$entry")"
    entry_type="$(snapshot_entry_type "$entry")"
    size="$(stat -c '%s' -- "$entry" 2>/dev/null || printf '0')"
    printf '%-10s %-12s %s\n' "$entry_type" "$size" "$name"
  done < <(find "$browse_path" -mindepth 1 -maxdepth 1 -print0 | sort -z)
}

catalog_records() {
  catalog_records_live "$@"
}

catalog_cache_base() {
  local volume="$1"
  local source_filter="$2"

  printf '%s/catalog-%s-%s' "$CACHE_DIR" "$volume" "$source_filter"
}

catalog_snapshot_signature() {
  local volume="$1"
  local source_filter="$2"
  local source
  local location
  local directory
  local snapshot

  source="${VOLUMES[$volume]}"

  {
    for location in local remote; do
      if [[ "$source_filter" != "any" && "$location" != "$source_filter" ]]; then
        continue
      fi

      directory="$(snapshot_location_path "$location" "$volume" "$source")"

      while IFS= read -r snapshot; do
        printf '%s\t%s\t%s/%s\n' "$location" "$snapshot" "$directory" "$snapshot"
      done < <(list_snapshots "$directory" "$volume")
    done
  } | sha256sum | awk '{print $1}'
}

catalog_snapshot_roots() {
  local volume="$1"
  local source_filter="$2"
  local source
  local location
  local directory
  local snapshot

  source="${VOLUMES[$volume]}"

  for location in local remote; do
    if [[ "$source_filter" != "any" && "$location" != "$source_filter" ]]; then
      continue
    fi

    directory="$(snapshot_location_path "$location" "$volume" "$source")"

    while IFS= read -r snapshot; do
      printf '%s\t%s\t%s/%s\n' "$location" "$snapshot" "$directory" "$snapshot"
    done < <(list_snapshots "$directory" "$volume")
  done
}

catalog_cache_is_fresh() {
  local volume="$1"
  local source_filter="$2"
  local cache_base
  local current_signature
  local cached_signature

  cache_base="$(catalog_cache_base "$volume" "$source_filter")"

  [[ -r "$cache_base.sig" && -r "$cache_base.db" ]] || return 1

  current_signature="$(catalog_snapshot_signature "$volume" "$source_filter")"
  cached_signature="$(cat "$cache_base.sig")"

  [[ "$current_signature" == "$cached_signature" ]]
}

build_catalog_cache() {
  local volume="$1"
  local source_filter="$2"
  local reset_cache="${3:-0}"
  local catalogd
  local args=()

  if [[ -x "$SCRIPT_DIR/backup-catalogd" ]]; then
    catalogd="$SCRIPT_DIR/backup-catalogd"
  elif command -v backup-catalogd >/dev/null 2>&1; then
    catalogd="$(command -v backup-catalogd)"
  else
    die "backup-catalogd is required to build catalog caches. Run install.sh or build it with: go build -o backup-catalogd ./cmd/backup-catalogd"
  fi

  args=(
    --once
    --script-dir "$SCRIPT_DIR"
    --config "$CONFIG_FILE"
    --source "$source_filter"
    --volume "$volume"
  )

  if (( reset_cache )); then
    args+=(--reset-cache)
  fi

  "$catalogd" "${args[@]}"
}

catalog_records_from_db() {
  local volume="$1"
  local source_filter="$2"
  local relative_path="$3"
  local catalogd

  if [[ -x "$SCRIPT_DIR/backup-catalogd" ]]; then
    catalogd="$SCRIPT_DIR/backup-catalogd"
  elif command -v backup-catalogd >/dev/null 2>&1; then
    catalogd="$(command -v backup-catalogd)"
  else
    return 1
  fi

  "$catalogd" \
    --query \
    --script-dir "$SCRIPT_DIR" \
    --config "$CONFIG_FILE" \
    --source "$source_filter" \
    --volume "$volume" \
    --path "$relative_path"
}

catalog_cache_db_is_readable() {
  local volume="$1"
  local source_filter="$2"
  local cache_base

  cache_base="$(catalog_cache_base "$volume" "$source_filter")"
  [[ -r "$cache_base.db" ]] && return 0

  if [[ "$source_filter" != "any" ]]; then
    cache_base="$(catalog_cache_base "$volume" "any")"
    [[ -r "$cache_base.db" ]] && return 0
  fi

  return 1
}

catalog_records_from_cache() {
  local volume="$1"
  local source_filter="$2"
  local relative_path="$3"

  catalog_cache_db_is_readable "$volume" "$source_filter" || return 1
  catalog_records_from_db "$volume" "$source_filter" "$relative_path"
}

catalog_records_cached() {
  local volume="$1"
  local source_filter="$2"
  local relative_path="$3"
  local cache_mode="$4"
  local reset_cache="${5:-0}"
  local cache_base
  local db_file

  cache_base="$(catalog_cache_base "$volume" "$source_filter")"
  db_file="$cache_base.db"

  case "$cache_mode" in
    none)
      catalog_records_live "$volume" "$source_filter" "$relative_path"
      return 0
      ;;
    refresh)
      build_catalog_cache "$volume" "$source_filter" "$reset_cache" || die "Could not rebuild catalog cache: $db_file"
      catalog_records_from_cache "$volume" "$source_filter" "$relative_path"
      return 0
      ;;
    stale)
      if catalog_cache_db_is_readable "$volume" "$source_filter"; then
        catalog_records_from_cache "$volume" "$source_filter" "$relative_path"
      else
        catalog_records_live "$volume" "$source_filter" "$relative_path"
      fi
      return 0
      ;;
    auto)
      if catalog_cache_is_fresh "$volume" "$source_filter"; then
        catalog_records_from_cache "$volume" "$source_filter" "$relative_path"
      elif build_catalog_cache "$volume" "$source_filter" 0; then
        catalog_records_from_cache "$volume" "$source_filter" "$relative_path"
      else
        catalog_records_live "$volume" "$source_filter" "$relative_path"
      fi
      return 0
      ;;
    *)
      die "Unknown cache mode: $cache_mode"
      ;;
  esac
}

catalog_records_live() {
  local volume="$1"
  local source_filter="$2"
  local relative_path="$3"
  local source
  local location
  local directory
  local snapshot
  local snapshot_root
  local browse_path
  local resolved_snapshot_root
  local resolved_browse_path
  local entry
  local name
  local entry_type
  local entry_relative
  local version_key
  local current_type
  local sort_type
  local live_path
  local present_live
  local deleted
  local key

  declare -A entry_names=()
  declare -A entry_types=()
  declare -A version_counts=()
  declare -A latest_snapshots=()
  declare -A first_snapshots=()
  declare -A seen_versions=()

  source="${VOLUMES[$volume]}"

  for location in local remote; do
    if [[ "$source_filter" != "any" && "$location" != "$source_filter" ]]; then
      continue
    fi

    directory="$(snapshot_location_path "$location" "$volume" "$source")"

    while IFS= read -r snapshot; do
      snapshot_root="$directory/$snapshot"
      browse_path="$(snapshot_content_path "$snapshot_root" "$relative_path")"
      resolved_snapshot_root="$(realpath -m -- "$snapshot_root")"
      resolved_browse_path="$(realpath -m -- "$browse_path")"

      if [[ "$resolved_browse_path" != "$resolved_snapshot_root" && "$resolved_browse_path" != "$resolved_snapshot_root/"* ]]; then
        continue
      fi

      [[ -d "$browse_path" && ! -L "$browse_path" ]] || continue

      while IFS= read -r -d '' entry; do
        name="$(basename -- "$entry")"
        entry_type="$(snapshot_entry_type "$entry")"

        if [[ -z "$relative_path" ]]; then
          entry_relative="$name"
        else
          entry_relative="$relative_path/$name"
        fi

        entry_names["$entry_relative"]="$name"

        current_type="${entry_types[$entry_relative]:-}"
        if [[ -z "$current_type" ]]; then
          entry_types["$entry_relative"]="$entry_type"
        elif [[ "$current_type" != "$entry_type" ]]; then
          entry_types["$entry_relative"]="mixed"
        fi

        version_key="$entry_relative"$'\t'"$snapshot"
        if [[ -z "${seen_versions[$version_key]+x}" ]]; then
          seen_versions["$version_key"]=1
          version_counts["$entry_relative"]=$(( ${version_counts[$entry_relative]:-0} + 1 ))
        fi

        if [[ -z "${first_snapshots[$entry_relative]:-}" || "$snapshot" < "${first_snapshots[$entry_relative]}" ]]; then
          first_snapshots["$entry_relative"]="$snapshot"
        fi

        if [[ -z "${latest_snapshots[$entry_relative]:-}" || "$snapshot" > "${latest_snapshots[$entry_relative]}" ]]; then
          latest_snapshots["$entry_relative"]="$snapshot"
        fi
      done < <(find "$browse_path" -mindepth 1 -maxdepth 1 -print0 | sort -z)
    done < <(list_snapshots "$directory" "$volume")
  done

  for key in "${!entry_names[@]}"; do
    live_path="$(live_path_for_snapshot_relative "$source" "$key")"
    present_live=0
    deleted=1

    if [[ -e "$live_path" || -L "$live_path" ]]; then
      present_live=1
      deleted=0
    fi

    if [[ "${entry_types[$key]}" == "directory" ]]; then
      sort_type=0
    else
      sort_type=1
    fi

    printf '%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%d\t%d\n' \
      "$sort_type" \
      "${entry_names[$key]}" \
      "${entry_types[$key]}" \
      "$key" \
      "$live_path" \
      "${version_counts[$key]:-0}" \
      "${version_counts[$key]:-0}" \
      "${first_snapshots[$key]}" \
      "${latest_snapshots[$key]}" \
      "$present_live" \
      "$deleted"
  done | sort -t $'\t' -k1,1n -k2,2f | cut -f2-
}

print_catalog_records_text() {
  local records=()
  local record
  local name
  local entry_type
  local relative_path
  local live_path
  local version_label
  local versions_count
  local first_snapshot
  local latest_snapshot
  local present_live
  local deleted
  local state

  mapfile -t records

  printf '%-10s %-8s %-10s %s\n' "TYPE" "VERSIONS" "STATE" "PATH"

  for record in "${records[@]}"; do
    IFS=$'\t' read -r name entry_type relative_path live_path version_label versions_count first_snapshot latest_snapshot present_live deleted <<< "$record"
    state="current"
    if (( deleted )); then
      state="deleted"
    fi
    printf '%-10s %-8s %-10s %s\n' "$entry_type" "$versions_count" "$state" "$relative_path"
  done
}

print_catalog_records_json() {
  local volume="$1"
  local source_filter="$2"
  local relative_path="$3"
  local records=()
  local record
  local name
  local entry_type
  local entry_relative
  local live_path
  local version_label
  local versions_count
  local first_snapshot
  local latest_snapshot
  local present_live
  local deleted
  local first=1

  mapfile -t records

  printf '{"volume":'
  json_string "$volume"
  printf ',"source":'
  json_string "$source_filter"
  printf ',"path":'
  json_string "$relative_path"
  printf ',"entries":['

  for record in "${records[@]}"; do
    IFS=$'\t' read -r name entry_type entry_relative live_path version_label versions_count first_snapshot latest_snapshot present_live deleted <<< "$record"

    if (( first )); then
      first=0
    else
      printf ','
    fi

    printf '{"name":'
    json_string "$name"
    printf ',"type":'
    json_string "$entry_type"
    printf ',"relative_path":'
    json_string "$entry_relative"
    printf ',"restore_path":'
    json_string "$live_path"
    printf ',"versions_count":%s' "$versions_count"
    printf ',"first_snapshot":'
    json_string "$first_snapshot"
    printf ',"latest_snapshot":'
    json_string "$latest_snapshot"
    printf ',"present_live":'
    json_bool "$present_live"
    printf ',"deleted":'
    json_bool "$deleted"
    printf '}'
  done

  printf ']}\n'
}

cmd_catalog() {
  local json=0
  local volume=""
  local source_filter="any"
  local relative_path=""
  local cache_mode="auto"
  local reset_cache=0

  while (($#)); do
    case "$1" in
      --json)
        json=1
        ;;
      --volume)
        shift
        (($#)) || die "--volume requires a volume name"
        volume="$1"
        ;;
      --source)
        shift
        (($#)) || die "--source requires local, remote, or any"
        source_filter="$1"
        ;;
      --path)
        shift
        (($#)) || die "--path requires a relative path"
        relative_path="$1"
        ;;
      --refresh-cache)
        cache_mode="refresh"
        ;;
      --reset-cache)
        cache_mode="refresh"
        reset_cache=1
        ;;
      --no-cache)
        cache_mode="none"
        ;;
      --stale-cache)
        cache_mode="stale"
        ;;
      -h | --help)
        usage
        return 0
        ;;
      -*)
        die "Unknown catalog option: $1"
        ;;
      *)
        die "Unexpected catalog argument: $1"
        ;;
    esac
    shift
  done

  [[ -n "$volume" ]] || die "catalog requires --volume"
  [[ -n "${VOLUMES[$volume]+x}" ]] || die "Unknown volume: $volume"

  case "$source_filter" in
    local | remote | any) ;;
    *) die "--source must be local, remote, or any" ;;
  esac

  relative_path="$(clean_snapshot_relative_path "$relative_path")"

  if (( json )); then
    catalog_records_cached "$volume" "$source_filter" "$relative_path" "$cache_mode" "$reset_cache" | print_catalog_records_json "$volume" "$source_filter" "$relative_path"
  else
    catalog_records_cached "$volume" "$source_filter" "$relative_path" "$cache_mode" "$reset_cache" | print_catalog_records_text
  fi
}

cmd_cache() {
  local volume_filter=""
  local source_filter="any"
  local reset_cache=0
  local name

  while (($#)); do
    case "$1" in
      --volume)
        shift
        (($#)) || die "--volume requires a volume name"
        volume_filter="$1"
        ;;
      --source)
        shift
        (($#)) || die "--source requires local, remote, or any"
        source_filter="$1"
        ;;
      --reset-cache)
        reset_cache=1
        ;;
      -h | --help)
        usage
        return 0
        ;;
      -*)
        die "Unknown cache option: $1"
        ;;
      *)
        die "Unexpected cache argument: $1"
        ;;
    esac
    shift
  done

  case "$source_filter" in
    local | remote | any) ;;
    *) die "--source must be local, remote, or any" ;;
  esac

  if [[ -n "$volume_filter" && -z "${VOLUMES[$volume_filter]+x}" ]]; then
    die "Unknown volume: $volume_filter"
  fi

  for name in "${VOLUME_NAMES[@]}"; do
    if [[ -n "$volume_filter" && "$name" != "$volume_filter" ]]; then
      continue
    fi

    printf 'Building catalog cache for %s (%s)\n' "$name" "$source_filter"
    build_catalog_cache "$name" "$source_filter" "$reset_cache" || die "Could not build catalog cache for $name"
  done
}

cmd_status() {
  local json=0
  local mounted=0
  local name
  local source
  local local_dir
  local remote_dir
  local local_snapshots=()
  local remote_snapshots=()
  local latest_local
  local latest_remote
  local first=1

  while (($#)); do
    case "$1" in
      --json)
        json=1
        ;;
      -h | --help)
        usage
        return 0
        ;;
      *)
        die "Unknown status option: $1"
        ;;
    esac
    shift
  done

  if backup_mount_is_mounted; then
    mounted=1
  fi

  if (( json )); then
    printf '{"backup_mount":'
    json_string "$BACKUP_MOUNT"
    printf ',"backup_mounted":'
    json_bool "$mounted"
    printf ',"volumes":['

    for name in "${VOLUME_NAMES[@]}"; do
      source="${VOLUMES[$name]}"
      local_dir="$(snapshot_dir_for "$source")"
      remote_dir="$BACKUP_MOUNT/$name"
      mapfile -t local_snapshots < <(list_snapshots "$local_dir" "$name")
      mapfile -t remote_snapshots < <(list_snapshots "$remote_dir" "$name")
      latest_local=""
      latest_remote=""

      if (( ${#local_snapshots[@]} > 0 )); then
        latest_local="${local_snapshots[$(( ${#local_snapshots[@]} - 1 ))]}"
      fi

      if (( ${#remote_snapshots[@]} > 0 )); then
        latest_remote="${remote_snapshots[$(( ${#remote_snapshots[@]} - 1 ))]}"
      fi

      if (( first )); then
        first=0
      else
        printf ','
      fi

      printf '{"name":'
      json_string "$name"
      printf ',"source":'
      json_string "$source"
      printf ',"local_dir":'
      json_string "$local_dir"
      printf ',"remote_dir":'
      json_string "$remote_dir"
      printf ',"local_count":%d' "${#local_snapshots[@]}"
      printf ',"latest_local":'
      json_string "$latest_local"
      printf ',"remote_count":%d' "${#remote_snapshots[@]}"
      printf ',"latest_remote":'
      json_string "$latest_remote"
      printf '}'
    done

    printf ']}\n'
    return 0
  fi

  printf 'Backup mount: %s (%s)\n' "$BACKUP_MOUNT" "$([[ "$mounted" == 1 ]] && printf mounted || printf not-mounted)"
  printf '%-10s %-8s %-18s %-8s %-18s %s\n' "VOLUME" "LOCAL" "LATEST_LOCAL" "REMOTE" "LATEST_REMOTE" "SOURCE"

  for name in "${VOLUME_NAMES[@]}"; do
    source="${VOLUMES[$name]}"
    local_dir="$(snapshot_dir_for "$source")"
    remote_dir="$BACKUP_MOUNT/$name"
    mapfile -t local_snapshots < <(list_snapshots "$local_dir" "$name")
    mapfile -t remote_snapshots < <(list_snapshots "$remote_dir" "$name")
    latest_local="-"
    latest_remote="-"

    if (( ${#local_snapshots[@]} > 0 )); then
      latest_local="${local_snapshots[$(( ${#local_snapshots[@]} - 1 ))]}"
    fi

    if (( ${#remote_snapshots[@]} > 0 )); then
      latest_remote="${remote_snapshots[$(( ${#remote_snapshots[@]} - 1 ))]}"
    fi

    printf '%-10s %-8d %-18s %-8d %-18s %s\n' \
      "$name" "${#local_snapshots[@]}" "$latest_local" "${#remote_snapshots[@]}" "$latest_remote" "$source"
  done
}

print_snapshot_records_text() {
  local records=()
  local record
  local volume
  local location
  local snapshot
  local path

  mapfile -t records

  for record in "${records[@]}"; do
    IFS=$'\t' read -r volume location snapshot path <<< "$record"
    printf '%-10s %-6s %-18s %s\n' "$volume" "$location" "$snapshot" "$path"
  done
}

print_snapshot_records_json() {
  local records=()
  local record
  local volume
  local location
  local snapshot
  local path
  local first=1

  mapfile -t records

  printf '[\n'
  for record in "${records[@]}"; do
    IFS=$'\t' read -r volume location snapshot path <<< "$record"

    if (( first )); then
      first=0
    else
      printf ',\n'
    fi

    printf '  {"volume":'
    json_string "$volume"
    printf ',"location":'
    json_string "$location"
    printf ',"snapshot":'
    json_string "$snapshot"
    printf ',"path":'
    json_string "$path"
    printf '}'
  done
  printf '\n]\n'
}

cmd_snapshots() {
  local json=0
  local location_filter="all"
  local volume_filter=""

  while (($#)); do
    case "$1" in
      --json)
        json=1
        ;;
      --local)
        location_filter="local"
        ;;
      --remote)
        location_filter="remote"
        ;;
      --all)
        location_filter="all"
        ;;
      -h | --help)
        usage
        return 0
        ;;
      -*)
        die "Unknown snapshots option: $1"
        ;;
      *)
        if [[ -n "$volume_filter" ]]; then
          die "Only one volume filter can be provided."
        fi
        volume_filter="$1"
        ;;
    esac
    shift
  done

  if [[ -n "$volume_filter" && -z "${VOLUMES[$volume_filter]+x}" ]]; then
    die "Unknown volume: $volume_filter"
  fi

  if (( json )); then
    list_snapshot_records "$location_filter" "$volume_filter" | print_snapshot_records_json
  else
    list_snapshot_records "$location_filter" "$volume_filter" | print_snapshot_records_text
  fi
}

version_records_from_db() {
  local volume="$1"
  local source_filter="$2"
  local relative_path="$3"
  local catalogd

  if [[ -x "$SCRIPT_DIR/backup-catalogd" ]]; then
    catalogd="$SCRIPT_DIR/backup-catalogd"
  elif command -v backup-catalogd >/dev/null 2>&1; then
    catalogd="$(command -v backup-catalogd)"
  else
    return 1
  fi

  "$catalogd" \
    --versions \
    --script-dir "$SCRIPT_DIR" \
    --config "$CONFIG_FILE" \
    --source "$source_filter" \
    --volume "$volume" \
    --path "$relative_path"
}

version_records_for_file() {
  local file_path="$1"
  local source_filter="$2"
  local normalized_path
  local name
  local source
  local relative_path
  local location
  local directory
  local snapshot
  local candidate

  [[ "$file_path" == /* ]] || die "File path must be absolute: $file_path"

  normalized_path="$(normalize_path "$file_path")"
  name="$(volume_name_for_path "$normalized_path")" || die "No configured volume contains: $file_path"
  source="${VOLUMES[$name]}"
  relative_path="$(relative_path_for_volume "$normalized_path" "$source")"

  if version_records_from_db "$name" "$source_filter" "$relative_path"; then
    return 0
  fi

  for location in local remote; do
    if [[ "$source_filter" != "any" && "$location" != "$source_filter" ]]; then
      continue
    fi

    directory="$(snapshot_location_path "$location" "$name" "$source")"

    while IFS= read -r snapshot; do
      candidate="$(snapshot_content_path "$directory/$snapshot" "$relative_path")"
      if [[ -e "$candidate" || -L "$candidate" ]]; then
        printf '%s\t%s\t%s\t%s\t%s\n' "$name" "$location" "$snapshot" "$candidate" "$normalized_path"
      fi
    done < <(list_snapshots "$directory" "$name")
  done
}

print_version_records_text() {
  local records=()
  local record
  local volume
  local location
  local snapshot
  local path
  local requested

  mapfile -t records

  for record in "${records[@]}"; do
    IFS=$'\t' read -r volume location snapshot path requested <<< "$record"
    printf '%-10s %-6s %-18s %s\n' "$volume" "$location" "$snapshot" "$path"
  done
}

print_version_records_json() {
  local records=()
  local record
  local volume
  local location
  local snapshot
  local path
  local requested
  local first=1

  mapfile -t records

  printf '[\n'
  for record in "${records[@]}"; do
    IFS=$'\t' read -r volume location snapshot path requested <<< "$record"

    if (( first )); then
      first=0
    else
      printf ',\n'
    fi

    printf '  {"volume":'
    json_string "$volume"
    printf ',"location":'
    json_string "$location"
    printf ',"snapshot":'
    json_string "$snapshot"
    printf ',"path":'
    json_string "$path"
    printf ',"requested_path":'
    json_string "$requested"
    printf '}'
  done
  printf '\n]\n'
}

cmd_versions() {
  local json=0
  local source_filter="any"
  local file_path=""

  while (($#)); do
    case "$1" in
      --json)
        json=1
        ;;
      --source)
        shift
        (($#)) || die "--source requires local, remote, or any"
        source_filter="$1"
        ;;
      -h | --help)
        usage
        return 0
        ;;
      -*)
        die "Unknown versions option: $1"
        ;;
      *)
        if [[ -n "$file_path" ]]; then
          die "Only one file path can be provided."
        fi
        file_path="$1"
        ;;
    esac
    shift
  done

  case "$source_filter" in
    local | remote | any) ;;
    *) die "--source must be local, remote, or any" ;;
  esac

  [[ -n "$file_path" ]] || die "versions requires a file path"

  if (( json )); then
    version_records_for_file "$file_path" "$source_filter" | print_version_records_json
  else
    version_records_for_file "$file_path" "$source_filter" | print_version_records_text
  fi
}

find_restore_record() {
  local file_path="$1"
  local version="$2"
  local source_filter="$3"
  local record
  local volume
  local location
  local snapshot
  local path
  local requested

  while IFS= read -r record; do
    IFS=$'\t' read -r volume location snapshot path requested <<< "$record"
    if [[ "$snapshot" == "$version" ]]; then
      printf '%s\n' "$record"
      return 0
    fi
  done < <(version_records_for_file "$file_path" "$source_filter")

  return 1
}

cmd_restore() {
  local json=0
  local source_filter="any"
  local file_path=""
  local version=""
  local destination=""
  local record
  local volume
  local location
  local snapshot
  local source_path
  local requested
  local destination_parent

  while (($#)); do
    case "$1" in
      --json)
        json=1
        ;;
      --version)
        shift
        (($#)) || die "--version requires a snapshot name"
        version="$1"
        ;;
      --to)
        shift
        (($#)) || die "--to requires a destination path"
        destination="$1"
        ;;
      --source)
        shift
        (($#)) || die "--source requires local, remote, or any"
        source_filter="$1"
        ;;
      -h | --help)
        usage
        return 0
        ;;
      -*)
        die "Unknown restore option: $1"
        ;;
      *)
        if [[ -n "$file_path" ]]; then
          die "Only one file path can be provided."
        fi
        file_path="$1"
        ;;
    esac
    shift
  done

  case "$source_filter" in
    local | remote | any) ;;
    *) die "--source must be local, remote, or any" ;;
  esac

  [[ -n "$file_path" ]] || die "restore requires a file path"
  [[ -n "$version" ]] || die "restore requires --version"
  [[ -n "$destination" ]] || die "restore requires --to"
  [[ "$destination" == /* ]] || die "Destination path must be absolute: $destination"

  destination="$(normalize_path "$destination")"

  if [[ -e "$destination" || -L "$destination" ]]; then
    die "Destination already exists; refusing to overwrite: $destination"
  fi

  record="$(find_restore_record "$file_path" "$version" "$source_filter")" \
    || die "Snapshot version not found for $file_path: $version"

  IFS=$'\t' read -r volume location snapshot source_path requested <<< "$record"

  destination_parent="$(dirname -- "$destination")"
  mkdir -p "$destination_parent"
  cp -aT -- "$source_path" "$destination"

  if (( json )); then
    printf '{"volume":'
    json_string "$volume"
    printf ',"location":'
    json_string "$location"
    printf ',"snapshot":'
    json_string "$snapshot"
    printf ',"source_path":'
    json_string "$source_path"
    printf ',"requested_path":'
    json_string "$requested"
    printf ',"destination":'
    json_string "$destination"
    printf '}\n'
  else
    printf 'Restored %s from %s (%s) to %s\n' "$requested" "$snapshot" "$location" "$destination"
  fi
}

main() {
  local command="${1:-help}"

  case "$command" in
    status)
      shift
      cmd_status "$@"
      ;;
    snapshots)
      shift
      cmd_snapshots "$@"
      ;;
    files)
      shift
      cmd_files "$@"
      ;;
    catalog)
      shift
      cmd_catalog "$@"
      ;;
    cache)
      shift
      cmd_cache "$@"
      ;;
    versions)
      shift
      cmd_versions "$@"
      ;;
    restore)
      shift
      cmd_restore "$@"
      ;;
    help | -h | --help)
      usage
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
