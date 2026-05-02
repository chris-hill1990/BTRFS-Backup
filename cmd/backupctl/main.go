package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	cfgpkg "btrfs-backup/internal/config"
	"btrfs-backup/internal/snapshots"
)

func main() {
	defaultScriptDir := cfgpkg.EnvString("BACKUP_SCRIPT_DIR", cfgpkg.DefaultScriptDir())
	defaultConfigFile := cfgpkg.EnvString("CONFIG_FILE", "/etc/btrfs-backup.conf")

	common := flag.NewFlagSet("backupctl", flag.ExitOnError)
	scriptDir := common.String("script-dir", defaultScriptDir, "directory containing backup-common.sh")
	configFile := common.String("config", defaultConfigFile, "backup config file")
	common.Usage = usage
	_ = common.Parse(os.Args[1:])

	args := common.Args()
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	if command == "help" || command == "-h" || command == "--help" {
		usage()
		return
	}

	cfg, err := cfgpkg.Load(*scriptDir, *configFile)
	if err != nil {
		fatal("load config: %v", err)
	}

	switch command {
	case "status":
		if err := cmdStatus(cfg, args); err != nil {
			fatal("%v", err)
		}
	case "snapshots":
		if err := cmdSnapshots(cfg, args); err != nil {
			fatal("%v", err)
		}
	case "retention":
		if err := cmdRetention(cfg, args); err != nil {
			fatal("%v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: backupctl [--script-dir <dir>] [--config <file>] <command> [options]

Commands:
  status [--json]
      Show backup mount and per-volume snapshot status.

  snapshots [--json|--tsv] [--local|--remote|--all] [volume]
      List known local and/or backup-target snapshots.

  retention [--json|--tsv] [--local|--remote|--all] [volume]
      List snapshots that retention would remove.
`)
}

func cmdStatus(cfg cfgpkg.AppConfig, args []string) error {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected status argument: %s", flags.Arg(0))
	}

	status, err := snapshots.BuildStatus(cfg)
	if err != nil {
		return err
	}

	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			BackupMount   string                   `json:"backup_mount"`
			BackupMounted bool                     `json:"backup_mounted"`
			Volumes       []snapshots.VolumeStatus `json:"volumes"`
		}{
			BackupMount:   status.BackupMount,
			BackupMounted: status.BackupMounted,
			Volumes:       status.Volumes,
		})
	}

	mounted := "not-mounted"
	if status.BackupMounted {
		mounted = "mounted"
	}
	fmt.Printf("Backup mount: %s (%s)\n", status.BackupMount, mounted)
	fmt.Printf("%-10s %-8s %-18s %-8s %-18s %s\n", "VOLUME", "LOCAL", "LATEST_LOCAL", "REMOTE", "LATEST_REMOTE", "SOURCE")
	for _, volume := range status.Volumes {
		fmt.Printf("%-10s %-8d %-18s %-8d %-18s %s\n",
			volume.Name, volume.LocalCount, dashIfEmpty(volume.LatestLocal),
			volume.RemoteCount, dashIfEmpty(volume.LatestRemote), volume.Source)
	}
	return nil
}

func cmdSnapshots(cfg cfgpkg.AppConfig, args []string) error {
	flags := flag.NewFlagSet("snapshots", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "print JSON")
	tsvOutput := flags.Bool("tsv", false, "print tab-separated records")
	localOnly := flags.Bool("local", false, "list local snapshots")
	remoteOnly := flags.Bool("remote", false, "list remote snapshots")
	all := flags.Bool("all", false, "list all snapshots")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("only one volume filter can be provided")
	}
	if *jsonOutput && *tsvOutput {
		return fmt.Errorf("choose only one of --json or --tsv")
	}

	locationFilter, err := locationFilterFromFlags(*localOnly, *remoteOnly, *all)
	if err != nil {
		return err
	}

	volumeFilter, err := volumeFilterFromArgs(cfg, flags.Args())
	if err != nil {
		return err
	}

	records, err := snapshots.Records(cfg, locationFilter, volumeFilter)
	if err != nil {
		return err
	}

	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(records)
	}
	if *tsvOutput {
		writeTSVRecords(records)
		return nil
	}

	for _, record := range records {
		fmt.Printf("%-10s %-6s %-18s %s\n", record.Volume, record.Location, record.Snapshot, record.Path)
	}
	return nil
}

func cmdRetention(cfg cfgpkg.AppConfig, args []string) error {
	flags := flag.NewFlagSet("retention", flag.ExitOnError)
	jsonOutput := flags.Bool("json", false, "print JSON")
	tsvOutput := flags.Bool("tsv", false, "print tab-separated records")
	localOnly := flags.Bool("local", false, "list local retention removals")
	remoteOnly := flags.Bool("remote", false, "list remote retention removals")
	all := flags.Bool("all", false, "list all retention removals")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *jsonOutput && *tsvOutput {
		return fmt.Errorf("choose only one of --json or --tsv")
	}

	locationFilter, err := locationFilterFromFlags(*localOnly, *remoteOnly, *all)
	if err != nil {
		return err
	}

	volumeFilter, err := volumeFilterFromArgs(cfg, flags.Args())
	if err != nil {
		return err
	}

	records, err := snapshots.RetentionRecords(cfg, locationFilter, volumeFilter, time.Now())
	if err != nil {
		return err
	}

	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(records)
	}
	if *tsvOutput {
		writeTSVRecords(records)
		return nil
	}

	for _, record := range records {
		fmt.Printf("%-10s %-6s %-18s %s\n", record.Volume, record.Location, record.Snapshot, record.Path)
	}
	return nil
}

func locationFilterFromFlags(localOnly bool, remoteOnly bool, all bool) (string, error) {
	selectedFilters := 0
	for _, selected := range []bool{localOnly, remoteOnly, all} {
		if selected {
			selectedFilters++
		}
	}
	if selectedFilters > 1 {
		return "", fmt.Errorf("choose only one of --local, --remote, or --all")
	}

	locationFilter := "all"
	switch {
	case localOnly:
		locationFilter = "local"
	case remoteOnly:
		locationFilter = "remote"
	case all:
		locationFilter = "all"
	}
	if err := snapshots.ValidateLocationFilter(locationFilter); err != nil {
		return "", err
	}
	return locationFilter, nil
}

func volumeFilterFromArgs(cfg cfgpkg.AppConfig, args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("only one volume filter can be provided")
	}
	if len(args) == 0 {
		return "", nil
	}

	volumeFilter := args[0]
	if _, err := cfgpkg.FilterVolumes(cfg.Volumes, []string{volumeFilter}); err != nil {
		return "", err
	}
	return volumeFilter, nil
}

func writeTSVRecords(records []snapshots.Record) {
	for _, record := range records {
		fmt.Printf("%s\t%s\t%s\t%s\n", record.Volume, record.Location, record.Snapshot, record.Path)
	}
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}
