#!/usr/bin/env python3
"""Small GTK front-end for the btrfs backup CLI."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import threading
from pathlib import Path
from typing import Any

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Adw", "1")

from gi.repository import Adw, Gio, GLib, Gtk, Pango  # noqa: E402


APP_ID = "org.local.BtrfsBackup"
DEFAULT_CONFIG_FILE = "/etc/btrfs-backup.conf"


class BackupWindow(Adw.ApplicationWindow):
    def __init__(self, app: Adw.Application) -> None:
        super().__init__(application=app)

        self.script_dir = Path(__file__).resolve().parent
        self.cli = self.script_dir / "root-backup.sh"
        self.config_file = os.environ.get("CONFIG_FILE", DEFAULT_CONFIG_FILE)

        self.busy_count = 0
        self.volume_records: list[dict[str, Any]] = []
        self.volume_names: list[str] = []
        self.catalog_path = ""
        self.selected_file = ""
        self.restore_destination = ""
        self.selected_version: dict[str, Any] | None = None
        self.volume_rows: list[dict[str, Gtk.Label]] = []

        self.set_title("Btrfs Backup")
        self.set_default_size(1080, 760)

        self.toast_overlay = Adw.ToastOverlay()
        self.set_content(self.toast_overlay)

        toolbar_view = Adw.ToolbarView()
        self.toast_overlay.set_child(toolbar_view)

        header = Adw.HeaderBar()
        toolbar_view.add_top_bar(header)

        refresh_button = Gtk.Button.new_from_icon_name("view-refresh-symbolic")
        refresh_button.set_tooltip_text("Refresh backup status")
        refresh_button.connect("clicked", lambda _button: self.refresh_all())
        header.pack_start(refresh_button)

        self.snapshot_button = Gtk.Button.new_from_icon_name("document-new-symbolic")
        self.snapshot_button.set_tooltip_text("Create today's local snapshots")
        self.snapshot_button.connect("clicked", lambda _button: self.run_snapshot())
        header.pack_end(self.snapshot_button)

        self.sync_button = Gtk.Button.new_from_icon_name("emblem-synchronizing-symbolic")
        self.sync_button.set_tooltip_text("Sync missing snapshots to the backup drive")
        self.sync_button.connect("clicked", lambda _button: self.run_sync())
        header.pack_end(self.sync_button)

        scroller = Gtk.ScrolledWindow()
        scroller.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
        toolbar_view.set_content(scroller)

        content = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=16)
        content.set_margin_top(18)
        content.set_margin_bottom(18)
        content.set_margin_start(18)
        content.set_margin_end(18)
        scroller.set_child(content)

        self.build_status_group(content)
        self.build_catalog_group(content)
        self.build_restore_group(content)
        self.build_log_group(content)

        self.refresh_all()

    def build_status_group(self, content: Gtk.Box) -> None:
        status_group = Adw.PreferencesGroup(title="Status")
        content.append(status_group)

        status_box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12)
        status_box.set_margin_top(6)
        status_box.set_margin_bottom(6)
        status_group.add(status_box)

        self.mount_label = Gtk.Label(xalign=0)
        self.mount_label.add_css_class("title-4")
        status_box.append(self.mount_label)

        self.volume_grid = Gtk.Grid(column_spacing=18, row_spacing=8)
        self.volume_grid.set_hexpand(True)
        status_box.append(self.volume_grid)

        headings = ["Drive", "Local", "Latest Local", "Remote", "Latest Remote", "Source"]
        for column, heading in enumerate(headings):
            label = Gtk.Label(label=heading, xalign=0)
            label.add_css_class("heading")
            self.volume_grid.attach(label, column, 0, 1, 1)

    def build_catalog_group(self, content: Gtk.Box) -> None:
        catalog_group = Adw.PreferencesGroup(title="Drive Files")
        content.append(catalog_group)

        catalog_box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
        catalog_box.set_margin_top(6)
        catalog_group.add(catalog_box)

        controls = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        catalog_box.append(controls)

        self.volume_model = Gtk.StringList.new([])
        self.volume_selector = Gtk.DropDown.new(self.volume_model, None)
        self.volume_selector.set_tooltip_text("Choose backup drive")
        self.volume_selector.connect("notify::selected", self.on_volume_changed)
        controls.append(self.volume_selector)

        self.catalog_source_filter = Gtk.DropDown.new_from_strings(["All", "Local", "Remote"])
        self.catalog_source_filter.set_tooltip_text("Filter catalog by snapshot location")
        self.catalog_source_filter.connect("notify::selected", self.on_catalog_source_changed)
        controls.append(self.catalog_source_filter)

        self.cache_button = Gtk.Button.new_from_icon_name("view-refresh-symbolic")
        self.cache_button.set_tooltip_text("Build or resume the cached file catalog for this drive")
        self.cache_button.connect("clicked", lambda _button: self.refresh_catalog_cache())
        controls.append(self.cache_button)

        self.catalog_up_button = Gtk.Button.new_from_icon_name("go-up-symbolic")
        self.catalog_up_button.set_tooltip_text("Go to parent folder")
        self.catalog_up_button.set_sensitive(False)
        self.catalog_up_button.connect("clicked", lambda _button: self.go_up_catalog_folder())
        controls.append(self.catalog_up_button)

        self.catalog_path_label = Gtk.Label(label="Choose a drive to browse files", xalign=0)
        self.catalog_path_label.set_hexpand(True)
        self.catalog_path_label.set_ellipsize(Pango.EllipsizeMode.END)
        controls.append(self.catalog_path_label)

        self.file_list = Gtk.ListBox()
        self.file_list.add_css_class("boxed-list")
        self.file_list.set_selection_mode(Gtk.SelectionMode.SINGLE)
        self.file_list.connect("row-selected", self.on_catalog_file_selected)
        self.file_list.connect("row-activated", self.on_catalog_file_activated)
        catalog_box.append(self.file_list)

    def build_restore_group(self, content: Gtk.Box) -> None:
        restore_group = Adw.PreferencesGroup(title="Restore")
        content.append(restore_group)

        restore_box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)
        restore_box.set_margin_top(6)
        restore_group.add(restore_box)

        file_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        restore_box.append(file_row)

        choose_file_button = Gtk.Button.new_from_icon_name("document-open-symbolic")
        choose_file_button.set_tooltip_text("Choose a file to restore")
        choose_file_button.connect("clicked", lambda _button: self.choose_file())
        file_row.append(choose_file_button)

        self.file_label = Gtk.Label(label="No file selected", xalign=0)
        self.file_label.set_hexpand(True)
        self.file_label.set_ellipsize(Pango.EllipsizeMode.END)
        file_row.append(self.file_label)

        self.find_versions_button = Gtk.Button(label="Find Versions")
        self.find_versions_button.connect("clicked", lambda _button: self.find_versions())
        self.find_versions_button.set_sensitive(False)
        file_row.append(self.find_versions_button)

        self.version_list = Gtk.ListBox()
        self.version_list.add_css_class("boxed-list")
        self.version_list.set_selection_mode(Gtk.SelectionMode.SINGLE)
        self.version_list.connect("row-selected", self.on_version_selected)
        restore_box.append(self.version_list)

        destination_row = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        restore_box.append(destination_row)

        choose_destination_button = Gtk.Button.new_from_icon_name("document-save-as-symbolic")
        choose_destination_button.set_tooltip_text("Choose restore destination")
        choose_destination_button.connect("clicked", lambda _button: self.choose_destination())
        destination_row.append(choose_destination_button)

        self.destination_label = Gtk.Label(label="No restore destination selected", xalign=0)
        self.destination_label.set_hexpand(True)
        self.destination_label.set_ellipsize(Pango.EllipsizeMode.END)
        destination_row.append(self.destination_label)

        self.restore_button = Gtk.Button(label="Restore")
        self.restore_button.add_css_class("suggested-action")
        self.restore_button.set_sensitive(False)
        self.restore_button.connect("clicked", lambda _button: self.restore_selected_version())
        destination_row.append(self.restore_button)

    def build_log_group(self, content: Gtk.Box) -> None:
        log_group = Adw.PreferencesGroup(title="Activity")
        content.append(log_group)

        self.log_buffer = Gtk.TextBuffer()
        self.log_view = Gtk.TextView(buffer=self.log_buffer)
        self.log_view.set_editable(False)
        self.log_view.set_monospace(True)
        self.log_view.set_wrap_mode(Gtk.WrapMode.WORD_CHAR)
        self.log_view.set_size_request(-1, 150)
        log_group.add(self.log_view)

    def log(self, message: str) -> None:
        end = self.log_buffer.get_end_iter()
        self.log_buffer.insert(end, message.rstrip() + "\n")

    def toast(self, message: str) -> None:
        self.toast_overlay.add_toast(Adw.Toast(title=message))

    def is_busy(self) -> bool:
        return self.busy_count > 0

    def begin_command(self) -> None:
        self.busy_count += 1
        self.update_controls()

    def end_command(self) -> None:
        self.busy_count = max(0, self.busy_count - 1)
        self.update_controls()

    def update_controls(self) -> None:
        busy = self.is_busy()
        self.sync_button.set_sensitive(not busy)
        self.snapshot_button.set_sensitive(not busy)
        self.find_versions_button.set_sensitive((not busy) and bool(self.selected_file))
        self.restore_button.set_sensitive((not busy) and self.can_restore())
        self.cache_button.set_sensitive((not busy) and bool(self.current_volume_name()))
        self.catalog_up_button.set_sensitive((not busy) and bool(self.current_volume_name() and self.catalog_path))

    def can_restore(self) -> bool:
        return bool(self.selected_file and self.restore_destination and self.selected_version)

    def cli_environment(self) -> dict[str, str]:
        env = os.environ.copy()
        env["CONFIG_FILE"] = self.config_file
        return env

    def command_for(self, args: list[str], privileged: bool) -> list[str]:
        base = [str(self.cli), *args]
        if not privileged or os.geteuid() == 0:
            return base

        pkexec = shutil.which("pkexec")
        if not pkexec:
            return base

        return [
            pkexec,
            "/usr/bin/env",
            f"CONFIG_FILE={self.config_file}",
            str(self.cli),
            *args,
        ]

    def run_cli(
        self,
        args: list[str],
        *,
        parse_json: bool = False,
        privileged: bool = False,
        on_success: Any | None = None,
        log_stdout: bool = True,
        stream_output: bool = False,
    ) -> None:
        command = self.command_for(args, privileged)
        self.log(f"$ {' '.join(command)}")
        self.begin_command()

        def worker() -> None:
            if stream_output and not parse_json:
                self.run_streaming_command(command, on_success)
                return

            try:
                completed = subprocess.run(
                    command,
                    check=False,
                    capture_output=True,
                    text=True,
                    env=self.cli_environment(),
                )
            except Exception as exc:  # pragma: no cover - UI reporting path
                GLib.idle_add(self.command_failed, str(exc))
                return

            if completed.returncode != 0:
                output = completed.stderr.strip() or completed.stdout.strip()
                GLib.idle_add(self.command_failed, output or f"Command exited {completed.returncode}")
                return

            result: Any = completed.stdout
            if parse_json:
                try:
                    result = json.loads(completed.stdout or "null")
                except json.JSONDecodeError as exc:
                    GLib.idle_add(self.command_failed, f"Could not parse JSON: {exc}")
                    return

            stdout = completed.stdout if log_stdout else ""
            GLib.idle_add(self.command_succeeded, stdout, result, on_success)

        threading.Thread(target=worker, daemon=True).start()

    def run_streaming_command(self, command: list[str], on_success: Any | None) -> None:
        output_lines: list[str] = []

        try:
            process = subprocess.Popen(
                command,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                env=self.cli_environment(),
                bufsize=1,
            )
        except Exception as exc:  # pragma: no cover - UI reporting path
            GLib.idle_add(self.command_failed, str(exc))
            return

        assert process.stdout is not None

        for line in process.stdout:
            line = line.rstrip()
            output_lines.append(line)
            GLib.idle_add(self.log, line)

        return_code = process.wait()

        if return_code != 0:
            GLib.idle_add(self.command_failed, f"Command exited {return_code}")
            return

        GLib.idle_add(self.command_succeeded, "", "\n".join(output_lines), on_success)

    def command_failed(self, message: str) -> bool:
        self.end_command()
        self.log(message)
        self.toast("Command failed")
        return False

    def command_succeeded(self, stdout: str, result: Any, on_success: Any | None) -> bool:
        self.end_command()
        if stdout.strip():
            self.log(stdout)
        if on_success:
            on_success(result)
        return False

    def refresh_all(self) -> None:
        self.run_cli(["status", "--json"], parse_json=True, on_success=self.update_status, log_stdout=False)

    def update_status(self, status: dict[str, Any]) -> None:
        mounted = "mounted" if status.get("backup_mounted") else "not mounted"
        self.mount_label.set_text(f"{status.get('backup_mount', '')} is {mounted}")

        for row in self.volume_rows:
            for label in row.values():
                self.volume_grid.remove(label)
        self.volume_rows.clear()

        volumes = list(status.get("volumes", []))
        self.volume_records = volumes

        for index, volume in enumerate(volumes, start=1):
            labels = {
                "name": Gtk.Label(label=str(volume.get("name", "")), xalign=0),
                "local": Gtk.Label(label=str(volume.get("local_count", 0)), xalign=0),
                "latest_local": Gtk.Label(label=str(volume.get("latest_local") or "-"), xalign=0),
                "remote": Gtk.Label(label=str(volume.get("remote_count", 0)), xalign=0),
                "latest_remote": Gtk.Label(label=str(volume.get("latest_remote") or "-"), xalign=0),
                "source": Gtk.Label(label=str(volume.get("source", "")), xalign=0),
            }
            labels["source"].set_ellipsize(Pango.EllipsizeMode.END)

            for column, label in enumerate(labels.values()):
                self.volume_grid.attach(label, column, index, 1, 1)

            self.volume_rows.append(labels)

        self.update_volume_selector([str(volume.get("name", "")) for volume in volumes])

    def update_volume_selector(self, names: list[str]) -> None:
        current = self.current_volume_name()
        clean_names = [name for name in names if name]

        if clean_names == self.volume_names:
            if not self.current_volume_name() and clean_names:
                self.volume_selector.set_selected(0)
            self.load_catalog()
            return

        self.volume_names = clean_names
        model = Gtk.StringList.new(clean_names)
        self.volume_selector.set_model(model)

        selected_index = 0
        if current in clean_names:
            selected_index = clean_names.index(current)

        if clean_names:
            self.volume_selector.set_selected(selected_index)
            self.catalog_path = ""
            self.load_catalog()
        else:
            self.clear_catalog("No drives configured")

    def current_volume_name(self) -> str:
        selected = self.volume_selector.get_selected()
        if selected >= len(self.volume_names):
            return ""
        return self.volume_names[selected]

    def current_source_filter(self) -> str:
        selected = self.catalog_source_filter.get_selected()
        if selected == 1:
            return "local"
        if selected == 2:
            return "remote"
        return "any"

    def on_volume_changed(self, *_args: Any) -> None:
        self.catalog_path = ""
        self.load_catalog()

    def on_catalog_source_changed(self, *_args: Any) -> None:
        self.catalog_path = ""
        self.load_catalog()

    def clear_listbox(self, listbox: Gtk.ListBox) -> None:
        child = listbox.get_first_child()
        while child is not None:
            next_child = child.get_next_sibling()
            listbox.remove(child)
            child = next_child

    def clear_catalog(self, label: str) -> None:
        self.catalog_path = ""
        self.catalog_path_label.set_text(label)
        self.catalog_up_button.set_sensitive(False)
        self.clear_listbox(self.file_list)

    def load_catalog(self) -> None:
        volume = self.current_volume_name()
        if not volume:
            return

        args = [
            "catalog",
            "--json",
            "--stale-cache",
            "--volume",
            volume,
            "--source",
            self.current_source_filter(),
        ]

        if self.catalog_path:
            args.extend(["--path", self.catalog_path])

        self.run_cli(args, parse_json=True, on_success=self.update_catalog, log_stdout=False)

    def refresh_catalog_cache(self) -> None:
        volume = self.current_volume_name()
        if not volume:
            return

        args = [
            "cache",
            "--volume",
            volume,
            "--source",
            self.current_source_filter(),
        ]
        self.run_cli(args, privileged=True, on_success=lambda _out: self.load_catalog(), stream_output=True)

    def update_catalog(self, catalog: dict[str, Any]) -> None:
        self.clear_listbox(self.file_list)
        self.catalog_path = str(catalog.get("path") or "")
        volume = str(catalog.get("volume") or self.current_volume_name())
        shown_path = f"/{self.catalog_path}" if self.catalog_path else "/"
        self.catalog_path_label.set_text(f"{volume} {shown_path}")
        self.catalog_up_button.set_sensitive(bool(self.catalog_path))

        entries = list(catalog.get("entries") or [])
        if not entries:
            self.file_list.append(self.simple_row("No files found in snapshots"))
            return

        for entry in entries:
            row = self.catalog_row(entry)
            self.file_list.append(row)

    def simple_row(self, text: str) -> Gtk.ListBoxRow:
        row = Gtk.ListBoxRow()
        label = Gtk.Label(label=text, xalign=0)
        label.set_margin_top(10)
        label.set_margin_bottom(10)
        label.set_margin_start(12)
        label.set_margin_end(12)
        row.set_child(label)
        return row

    def two_line_row(self, title: str, subtitle: str) -> Gtk.ListBoxRow:
        row = Gtk.ListBoxRow()
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
        box.set_margin_top(8)
        box.set_margin_bottom(8)
        box.set_margin_start(12)
        box.set_margin_end(12)
        title_label = Gtk.Label(label=title, xalign=0)
        title_label.add_css_class("heading")
        subtitle_label = Gtk.Label(label=subtitle, xalign=0)
        subtitle_label.add_css_class("dim-label")
        subtitle_label.set_ellipsize(Pango.EllipsizeMode.END)
        box.append(title_label)
        box.append(subtitle_label)
        row.set_child(box)
        return row

    def catalog_row(self, entry: dict[str, Any]) -> Gtk.ListBoxRow:
        entry_type = str(entry.get("type", "other"))
        name = str(entry.get("name", ""))
        deleted = bool(entry.get("deleted"))
        title = f"{name}/" if entry_type == "directory" else name

        if deleted:
            title = f"{title}  [deleted]"

        versions = entry.get("versions_count", 0)
        latest = entry.get("latest_snapshot", "")
        subtitle = f"{entry_type}  {versions} version(s)  latest {latest}  {entry.get('restore_path', '')}"
        row = self.two_line_row(title, subtitle)
        row.file_record = entry  # type: ignore[attr-defined]
        return row

    def go_up_catalog_folder(self) -> None:
        if not self.catalog_path:
            return
        parts = [part for part in self.catalog_path.split("/") if part]
        self.catalog_path = "/".join(parts[:-1])
        self.load_catalog()

    def on_catalog_file_activated(self, _listbox: Gtk.ListBox, row: Gtk.ListBoxRow) -> None:
        if not hasattr(row, "file_record"):
            return

        entry = row.file_record  # type: ignore[attr-defined]
        if entry.get("type") == "directory":
            self.catalog_path = str(entry.get("relative_path") or "")
            self.load_catalog()
        else:
            self.use_catalog_file(entry, find_versions=True)

    def on_catalog_file_selected(self, _listbox: Gtk.ListBox, row: Gtk.ListBoxRow | None) -> None:
        if row is None or not hasattr(row, "file_record"):
            return

        entry = row.file_record  # type: ignore[attr-defined]
        if entry.get("type") == "directory":
            self.selected_version = None
            self.restore_button.set_sensitive(False)
            return

        self.use_catalog_file(entry, find_versions=True)

    def use_catalog_file(self, entry: dict[str, Any], *, find_versions: bool) -> None:
        self.selected_file = str(entry.get("restore_path") or "")
        self.file_label.set_text(self.selected_file)
        self.selected_version = None
        self.clear_listbox(self.version_list)
        self.find_versions_button.set_sensitive(bool(self.selected_file))
        self.restore_button.set_sensitive(False)

        if find_versions and self.selected_file:
            self.find_versions()

    def run_snapshot(self) -> None:
        self.run_cli(["snapshot"], privileged=True, on_success=lambda _out: self.refresh_all())

    def run_sync(self) -> None:
        self.run_cli(["sync"], privileged=True, on_success=lambda _out: self.refresh_all())

    def choose_file(self) -> None:
        dialog = Gtk.FileChooserNative.new(
            "Choose File to Restore",
            self,
            Gtk.FileChooserAction.OPEN,
            "_Open",
            "_Cancel",
        )
        dialog.connect("response", self.on_file_chosen)
        dialog.show()

    def on_file_chosen(self, dialog: Gtk.FileChooserNative, response: int) -> None:
        if response == Gtk.ResponseType.ACCEPT:
            file = dialog.get_file()
            if file:
                self.selected_file = file.get_path() or ""
                self.file_label.set_text(self.selected_file)
                self.find_versions_button.set_sensitive(True)
                self.selected_version = None
                self.clear_listbox(self.version_list)
        dialog.destroy()

    def find_versions(self) -> None:
        if not self.selected_file:
            return
        self.run_cli(
            ["versions", self.selected_file, "--json", "--source", self.current_source_filter()],
            parse_json=True,
            on_success=self.update_versions,
            log_stdout=False,
        )

    def update_versions(self, versions: list[dict[str, Any]]) -> None:
        self.selected_version = None
        self.clear_listbox(self.version_list)

        if not versions:
            self.version_list.append(self.simple_row("No versions found"))
            self.restore_button.set_sensitive(False)
            return

        for version in versions:
            title = f"{version.get('snapshot', '')}  {version.get('location', '')}"
            subtitle = str(version.get("path", ""))
            row = self.two_line_row(title, subtitle)
            row.version_record = version  # type: ignore[attr-defined]
            self.version_list.append(row)

    def on_version_selected(self, _listbox: Gtk.ListBox, row: Gtk.ListBoxRow | None) -> None:
        if row is None or not hasattr(row, "version_record"):
            self.selected_version = None
        else:
            self.selected_version = row.version_record  # type: ignore[attr-defined]
        self.restore_button.set_sensitive(self.can_restore())

    def choose_destination(self) -> None:
        dialog = Gtk.FileChooserNative.new(
            "Choose Restore Destination",
            self,
            Gtk.FileChooserAction.SAVE,
            "_Select",
            "_Cancel",
        )
        if self.selected_file:
            dialog.set_current_name(Path(self.selected_file).name)
        dialog.connect("response", self.on_destination_chosen)
        dialog.show()

    def on_destination_chosen(self, dialog: Gtk.FileChooserNative, response: int) -> None:
        if response == Gtk.ResponseType.ACCEPT:
            file = dialog.get_file()
            if file:
                self.restore_destination = file.get_path() or ""
                self.destination_label.set_text(self.restore_destination)
        dialog.destroy()
        self.restore_button.set_sensitive(self.can_restore())

    def restore_selected_version(self) -> None:
        if not self.can_restore() or not self.selected_version:
            return

        args = [
            "restore",
            self.selected_file,
            "--version",
            str(self.selected_version.get("snapshot", "")),
            "--source",
            str(self.selected_version.get("location", "any")),
            "--to",
            self.restore_destination,
        ]
        self.run_cli(args, privileged=True, on_success=lambda _out: self.toast("Restore complete"))


class BackupApplication(Adw.Application):
    def __init__(self) -> None:
        super().__init__(application_id=APP_ID, flags=Gio.ApplicationFlags.DEFAULT_FLAGS)

    def do_activate(self) -> None:
        window = self.props.active_window
        if window is None:
            window = BackupWindow(self)
        window.present()


def main() -> int:
    app = BackupApplication()
    return app.run(None)


if __name__ == "__main__":
    raise SystemExit(main())
