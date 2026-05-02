#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/backup-common.sh"

main() {
  local snapshot_date
  local name

  snapshot_date="$(date +%F)"

  setup_logging "Daily local snapshot run"
  require_btrfs

  for name in "${VOLUME_NAMES[@]}"; do
    create_daily_snapshot "$name" "${VOLUMES[$name]}" "$snapshot_date"
    log
  done

  log "Daily local snapshot run complete."
}

main "$@"
