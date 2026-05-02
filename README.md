# Btrfs Backup Tool

Btrfs Backup keeps local read-only Btrfs snapshots, syncs them to a mounted backup drive, and maintains a local SQLite catalog for browsing and restore workflows.

- `backup-snapshot-daily.sh` creates today's read-only local snapshots and is the cron entrypoint.
- `backup-sync-manual.sh` checks the backup drive and sends every local snapshot that is missing on the target, one snapshot at a time. After a successful sync for a volume, it trims local snapshots to the last 3.
- `backup-gui.py` is a small desktop app for status, sync, drive file browsing, and restore.
- `backupctl` is a Go CLI for fast snapshot inventory/status commands.
- `backup-catalogd` is a Go daemon that keeps file catalogs cached in the background.
- `root-backup.sh` is the main shell entrypoint for snapshot, sync, catalog, version, and restore commands.
- `install.sh` installs the tool to `/opt/backup`, writes the daily snapshot cron file, and installs the catalog daemon service.
- `btrfs-backup.conf.example` is the default config installed to `/etc/btrfs-backup.conf` on first install.

## Requirements

- Linux with Btrfs support.
- Source volumes configured in `/etc/btrfs-backup.conf` must live on Btrfs filesystems. Snapshots are created under each source's `.snapshots` directory, or `/.snapshots` for `/`.
- The backup target mounted at `BACKUP_MOUNT` must be a writable Btrfs filesystem, because sync uses `btrfs send` and `btrfs receive`.
- Root privileges are required for snapshot creation, sync, retention deletes, restore writes, install, and the catalog daemon service.
- Runtime command-line tools: Bash, `btrfs` from `btrfs-progs`, `mountpoint`, `find`, `sort`, `realpath`, and standard GNU/coreutils tools.
- Go is required at install time to build `backupctl` and `backup-catalogd`.
- Cron is used for daily snapshot scheduling. systemd is used for the background catalog daemon when available.
- The desktop GUI requires Python 3, GTK 4, libadwaita, and `pkexec`.

On Ubuntu, install the main dependencies with:

```sh
sudo apt install btrfs-progs golang-go
```

Install the optional GUI dependencies with:

```sh
sudo apt install python3-gi gir1.2-gtk-4.0 gir1.2-adw-1 pkexec
```

## Install

Run the installer as root:

```sh
sudo ./install.sh
```

It keeps the install flat under `/opt/backup`, builds the Go CLI and catalog daemon, installs `/etc/btrfs-backup.conf` if it does not already exist, writes a desktop launcher, installs the catalog daemon systemd service, and writes this cron file:

```sh
/etc/cron.d/btrfs-backup
```

The default cron schedule is:

```cron
0 23 * * * root /opt/backup/backup-snapshot-daily.sh
```

You can override install settings with environment variables:

```sh
sudo INSTALL_DIR=/opt/backup CONFIG_FILE=/etc/btrfs-backup.conf CRON_SCHEDULE="0 23 * * *" ./install.sh
```

Set `START_CATALOG_SERVICE=0` during install if you want the service enabled but not started immediately. Set `INSTALL_CATALOG_SERVICE=0` if you do not want the systemd service installed.

Catalog data contains file paths, so the installer creates `/var/cache/btrfs-backup` as a private local cache directory owned by the installing desktop user and root-writable for the daemon. The catalog daemon, CLI, and GUI read it locally; the tool does not expose a web service or upload catalog data. Override the owner with `CACHE_OWNER=<user>` if you install from a root shell instead of `sudo`.

## Configuration

Before the first install, edit `btrfs-backup.conf.example` in this repo if you already know your backup mount, retention settings, or volume list. The installer copies that file to `/etc/btrfs-backup.conf` only when the config does not already exist, so reinstalling the tool leaves your machine-specific config unchanged.

Machine-specific settings live in:

```sh
/etc/btrfs-backup.conf
```

The installer creates this file from `btrfs-backup.conf.example` only if it does not already exist.

Defaults:

```sh
BACKUP_MOUNT=/mnt/backup
LOGFILE=/var/log/btrfs-backup.log
LOCAL_KEEP=3
REMOTE_RETENTION_DAYS=365
CACHE_DIR=/var/cache/btrfs-backup
CATALOG_WORKERS=4
CATALOG_PROGRESS_INTERVAL=5000
CATALOGD_INTERVAL=30m
```

Configured volumes:

```sh
root=/
drive3=/mnt/drive3
drive4=/mnt/drive4
drive2=/mnt/drive2
```

You can override scalar settings with environment variables when running the commands. For example:

```sh
sudo BACKUP_MOUNT=/mnt/other-backup /opt/backup/backup-sync-manual.sh
```

## Cron

Run only the local snapshot creator from cron:

```cron
CONFIG_FILE=/etc/btrfs-backup.conf
0 23 * * * root /opt/backup/backup-snapshot-daily.sh
```

Equivalent dispatcher command:

```cron
0 23 * * * root /opt/backup/root-backup.sh snapshot
```

## Manual Sync

Mount the backup drive at `/mnt/backup`, then run:

```sh
sudo /opt/backup/backup-sync-manual.sh
```

Equivalent dispatcher command:

```sh
sudo /opt/backup/root-backup.sh sync
```

The sync command compares each volume's local `.snapshots` directory with its backup target directory under `/mnt/backup`. Missing snapshots are sent in sorted date order. If a previous snapshot is already present on the target, the next missing snapshot is sent incrementally from that parent; otherwise the first missing snapshot is sent as a full stream.

After the sync finishes for a volume, local snapshots are trimmed to `LOCAL_KEEP`, which defaults to 3. Remote snapshots older than `REMOTE_RETENTION_DAYS` are also pruned. The sync command asks `backupctl retention` to plan those deletes, then performs the actual `btrfs subvolume delete` calls.

## GUI

After install, open **Btrfs Backup** from the desktop app launcher, or run:

```sh
/opt/backup/backup-gui.py
```

The GUI is a thin front-end over `root-backup.sh`. It keeps the backup summary visible, including local and remote snapshot counts, then lets you choose a drive and browse the files found across its snapshots. Files that exist in snapshots but not on the live filesystem are marked deleted. Clicking a file loads its available versions so you can restore one to a new path. The file browser and version lookup read cached SQLite catalogs without privilege prompts when available; use the refresh button beside the drive selector to build or resume the cache after adding or syncing snapshots. Sync, snapshot, restore, and cache refresh actions use `pkexec` when the app is opened as a normal user, so Ubuntu should ask for administrator authentication only for those write/admin actions.

## Restore CLI

Show backup mount and per-volume snapshot status:

```sh
/opt/backup/root-backup.sh status
/opt/backup/root-backup.sh status --json
/opt/backup/backupctl status
```

List local and backup-target snapshots:

```sh
/opt/backup/root-backup.sh snapshots
/opt/backup/backupctl snapshots
```

Limit the listing:

```sh
/opt/backup/root-backup.sh snapshots --local root
/opt/backup/root-backup.sh snapshots --remote drive3
/opt/backup/backupctl snapshots --local root
```

Preview what snapshot retention would remove:

```sh
/opt/backup/backupctl retention --local root
/opt/backup/backupctl retention --remote drive3
```

Browse the file catalog for a drive across all of its snapshots:

```sh
/opt/backup/root-backup.sh catalog --volume root
/opt/backup/root-backup.sh catalog --volume root --path etc --json
```

Rebuild cached catalogs:

```sh
sudo /opt/backup/root-backup.sh cache
sudo /opt/backup/root-backup.sh cache --volume root --source any
sudo /opt/backup/root-backup.sh cache --volume root --reset-cache
```

`catalog` reads cached catalog data from SQLite. Use `--refresh-cache` to ask the Go daemon binary to build or resume the cache first, `--reset-cache` to clear and rebuild it, `--stale-cache` to prefer an existing SQLite cache even if snapshots changed, or `--no-cache` to walk snapshots directly.

Catalog caches should stay on the local machine. The daemon recursively walks snapshot trees with `CATALOG_WORKERS` concurrent workers, writes queryable rows into `catalog-<volume>-<source>.db`, and records completed snapshots in the SQLite `snapshots` table. During a large snapshot, it commits a batch and updates a `.scan` checkpoint every `CATALOG_PROGRESS_INTERVAL` entries, so a reboot can resume inside that snapshot. Partial snapshot rows stay hidden from catalog reads until the snapshot is marked done. Cache files are written private to the cache owner by default. When sync removes local or remote snapshots, it prunes matching catalog rows and paths that no remaining snapshot references.

Cache rebuilds print progress as they index snapshots. Set `CATALOG_PROGRESS_INTERVAL` to change how often entry-count updates and cache commits happen:

```sh
sudo CATALOG_PROGRESS_INTERVAL=10000 /opt/backup/root-backup.sh cache --volume root
```

The systemd service keeps the cache warm in the background:

```sh
sudo systemctl status btrfs-backup-catalog.service
journalctl -u btrfs-backup-catalog.service -f
```

By default it runs once on startup, then every `CATALOGD_INTERVAL`.

List files inside a snapshot:

```sh
/opt/backup/root-backup.sh files --snapshot root-2026-04-21 --volume root
/opt/backup/root-backup.sh files --snapshot root-2026-04-21 --volume root --path etc --json
```

List versions of a file:

```sh
/opt/backup/root-backup.sh versions /etc/fstab
```

Restore one version to a new path:

```sh
sudo /opt/backup/root-backup.sh restore /etc/fstab --version root-2026-04-21 --to /tmp/fstab.restore
```

Restore refuses to overwrite an existing destination. It prefers a local snapshot when the same version exists locally and on the backup target. Use `--source remote` or `--source local` to force one side:

```sh
sudo /opt/backup/root-backup.sh restore /etc/fstab --version root-2026-04-21 --source remote --to /tmp/fstab.restore
```

Status, snapshot, retention, file-listing, version, and restore commands support `--json`, which is what the GUI uses:

```sh
/opt/backup/root-backup.sh status --json
/opt/backup/root-backup.sh snapshots --json
/opt/backup/backupctl retention --json
/opt/backup/root-backup.sh catalog --volume root --path etc --json
/opt/backup/root-backup.sh files --snapshot root-2026-04-21 --volume root --path etc --json
/opt/backup/root-backup.sh versions /etc/fstab --json
sudo /opt/backup/root-backup.sh restore /etc/fstab --version root-2026-04-21 --to /tmp/fstab.restore --json
```
