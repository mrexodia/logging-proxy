#!/usr/bin/env python3
"""Migrate legacy per-stream metadata JSON files to transaction JSONL.

The migration is non-destructive by default: old metadata JSON files and body
files are retained, and source filesystem dates are carried to the merged JSONL.
Stop logging-proxy (or migrate a copy of its log directory) so an in-progress
legacy metadata file cannot change during migration.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tempfile
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any, Iterable

OLD_NAME_RE = re.compile(
    r"^(?P<prefix>\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}\.\d{3})_"
    r"(?P<short_id>[^_]+)_(?P<stream>request|response)_metadata\.json$"
)
ISO_TIMESTAMP_RE = re.compile(
    r"^(?P<date>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"
    r"(?:\.(?P<fraction>\d{1,9}))?(?P<timezone>Z|[+-]\d{2}:\d{2})?$"
)
EVENT_PRIORITY = {
    "request_started": 0,
    "response_started": 1,
    "request_completed": 2,
    "response_completed": 3,
}


@dataclass(frozen=True)
class FileDates:
    accessed_ns: int
    modified_ns: int
    created_ns: int | None


@dataclass(frozen=True)
class LegacyMetadata:
    path: Path
    prefix: str
    short_id: str
    stream_type: str
    document: dict[str, Any]
    file_dates: FileDates

    @property
    def metadata(self) -> dict[str, Any]:
        value = self.document.get("metadata", {})
        return value if isinstance(value, dict) else {}

    @property
    def transaction_id(self) -> str:
        value = self.metadata.get("id")
        return str(value) if value else self.short_id

    @property
    def body_filename(self) -> str:
        value = self.document.get("filename")
        if value:
            return str(value)
        return f"{self.prefix}_{self.short_id}_{self.stream_type}.bin"


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Convert *_request_metadata.json and *_response_metadata.json "
            "pairs into one append-only *_metadata.jsonl per transaction."
        )
    )
    parser.add_argument("log_dir", type=Path, help="directory containing legacy logs")
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="validate and report what would be written without changing files",
    )
    parser.add_argument(
        "--overwrite",
        action="store_true",
        help="replace an existing destination JSONL instead of skipping it",
    )
    parser.add_argument(
        "--delete-old",
        action="store_true",
        help="delete legacy metadata JSON files after their JSONL is written successfully",
    )
    parser.add_argument(
        "--no-preserve-file-times",
        action="store_true",
        help="use the migration time for new JSONL filesystem timestamps",
    )
    parser.add_argument(
        "--repair-file-times",
        action="store_true",
        help="repair existing JSONL dates from lifecycle events instead of migrating",
    )
    return parser.parse_args(argv)


def source_creation_time_ns(source_stat: os.stat_result) -> int | None:
    # There is no portable API for setting a birth time. Preserve it on Windows,
    # where SetFileTime is available, and avoid treating POSIX st_ctime (inode
    # change time) as a creation time.
    if os.name != "nt":
        return None
    birthtime_ns = getattr(source_stat, "st_birthtime_ns", None)
    if birthtime_ns is not None:
        return int(birthtime_ns)
    # Before Python 3.12, st_ctime is the creation time on Windows.
    return int(source_stat.st_ctime_ns)


def load_legacy_metadata(log_dir: Path) -> tuple[list[LegacyMetadata], list[str]]:
    records: list[LegacyMetadata] = []
    errors: list[str] = []
    for path in sorted(log_dir.glob("*_metadata.json")):
        match = OLD_NAME_RE.match(path.name)
        if not match:
            errors.append(f"unrecognized legacy metadata filename: {path.name}")
            continue
        try:
            source_stat = path.stat()
            document = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            errors.append(f"failed to read {path.name}: {exc}")
            continue
        if not isinstance(document, dict):
            errors.append(f"metadata root is not an object: {path.name}")
            continue

        filename_stream = match.group("stream")
        document_stream = document.get("stream_type", filename_stream)
        if document_stream != filename_stream:
            errors.append(
                f"stream type mismatch in {path.name}: "
                f"filename={filename_stream!r}, document={document_stream!r}"
            )
            continue
        records.append(
            LegacyMetadata(
                path=path,
                prefix=match.group("prefix"),
                short_id=match.group("short_id"),
                stream_type=filename_stream,
                document=document,
                file_dates=FileDates(
                    accessed_ns=source_stat.st_atime_ns,
                    modified_ns=source_stat.st_mtime_ns,
                    created_ns=source_creation_time_ns(source_stat),
                ),
            )
        )
    return records, errors


def group_transactions(records: Iterable[LegacyMetadata]) -> tuple[dict[str, list[LegacyMetadata]], list[str]]:
    transactions: dict[str, list[LegacyMetadata]] = {}
    errors: list[str] = []
    for record in records:
        group = transactions.setdefault(record.transaction_id, [])
        if any(existing.stream_type == record.stream_type for existing in group):
            errors.append(
                f"duplicate {record.stream_type} metadata for transaction "
                f"{record.transaction_id}: {record.path.name}"
            )
            continue
        group.append(record)
    return transactions, errors


def first_nonempty(*values: Any) -> Any:
    for value in values:
        if value is not None and value != "":
            return value
    return None


def iso_from_mtime(path: Path) -> str:
    return datetime.fromtimestamp(path.stat().st_mtime).astimezone().isoformat()


def parse_iso(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def epoch_ns_from_iso(value: Any) -> int | None:
    if not isinstance(value, str):
        return None
    match = ISO_TIMESTAMP_RE.match(value)
    if match is None:
        return None
    timezone = match.group("timezone") or ""
    if timezone == "Z":
        timezone = "+00:00"
    try:
        whole_second = datetime.fromisoformat(match.group("date") + timezone)
        seconds = int(whole_second.timestamp())
    except (OSError, OverflowError, ValueError):
        return None
    fraction = (match.group("fraction") or "").ljust(9, "0")
    return seconds * 1_000_000_000 + int(fraction or "0")


def duration_ms(started_at: Any, completed_at: Any) -> int | None:
    start = parse_iso(started_at)
    complete = parse_iso(completed_at)
    if start is None or complete is None:
        return None
    try:
        return max(0, int((complete - start).total_seconds() * 1000))
    except TypeError:
        # A mixture of timezone-aware and naive historical timestamps cannot be
        # compared reliably; leave the inferred duration absent.
        return None


def make_events(record: LegacyMetadata, log_dir: Path) -> list[dict[str, Any]]:
    document = record.document
    started_at = first_nonempty(document.get("started_at"), document.get("timestamp"))
    if started_at is None:
        started_at = iso_from_mtime(record.path)

    base = {
        "stream_type": record.stream_type,
        "metadata": record.metadata,
        "started_at": started_at,
        "bytes_written": 0,
        "completed": False,
        "filename": record.body_filename,
    }
    started = dict(base)
    started["event"] = f"{record.stream_type}_started"
    started["timestamp"] = started_at
    events = [started]

    body_path = log_dir / record.body_filename
    explicit_completed = document.get("completed")
    error = document.get("error")
    if explicit_completed is None:
        completed = body_path.is_file()
    else:
        completed = bool(explicit_completed)

    # An explicit incomplete record without an error represents a stream that
    # was still active (or interrupted) and should remain unmatched in JSONL.
    if not completed and not error:
        return events

    completed_at = document.get("completed_at")
    if completed_at is None:
        completed_at = iso_from_mtime(record.path)

    bytes_written = document.get("bytes_written")
    if bytes_written is None:
        try:
            bytes_written = body_path.stat().st_size
        except OSError:
            bytes_written = 0

    completed_event = dict(base)
    completed_event.update(
        {
            "event": f"{record.stream_type}_completed",
            "timestamp": completed_at,
            "completed_at": completed_at,
            "bytes_written": int(bytes_written),
            "completed": completed,
        }
    )
    old_duration = document.get("duration_ms")
    inferred_duration = old_duration if old_duration is not None else duration_ms(started_at, completed_at)
    if inferred_duration:
        completed_event["duration_ms"] = int(inferred_duration)
    if error:
        completed_event["error"] = str(error)
    events.append(completed_event)
    return events


def event_sort_key(event: dict[str, Any]) -> tuple[datetime, int]:
    timestamp = parse_iso(event.get("timestamp"))
    if timestamp is None:
        timestamp = datetime.min
    # Normalize aware timestamps to their wall-clock-free numeric value while
    # retaining support for old naive timestamps.
    if timestamp.tzinfo is not None:
        timestamp = datetime.fromtimestamp(timestamp.timestamp())
    return timestamp, EVENT_PRIORITY.get(str(event.get("event")), 99)


def destination_for(records: list[LegacyMetadata]) -> Path:
    request = next((record for record in records if record.stream_type == "request"), None)
    anchor = request or min(records, key=lambda record: record.prefix)
    return anchor.path.parent / f"{anchor.prefix}_{anchor.short_id}_metadata.jsonl"


def merged_file_dates(records: list[LegacyMetadata]) -> FileDates:
    creation_times = [
        record.file_dates.created_ns
        for record in records
        if record.file_dates.created_ns is not None
    ]
    return FileDates(
        accessed_ns=max(record.file_dates.accessed_ns for record in records),
        modified_ns=max(record.file_dates.modified_ns for record in records),
        created_ns=min(creation_times) if creation_times else None,
    )


def set_windows_creation_time(path: Path, created_ns: int) -> None:
    if os.name != "nt":
        return

    import ctypes
    from ctypes import wintypes

    file_write_attributes = 0x0100
    file_share_all = 0x00000001 | 0x00000002 | 0x00000004
    open_existing = 3
    file_attribute_normal = 0x00000080
    invalid_handle_value = ctypes.c_void_p(-1).value

    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    create_file = kernel32.CreateFileW
    create_file.argtypes = [
        wintypes.LPCWSTR,
        wintypes.DWORD,
        wintypes.DWORD,
        wintypes.LPVOID,
        wintypes.DWORD,
        wintypes.DWORD,
        wintypes.HANDLE,
    ]
    create_file.restype = wintypes.HANDLE
    set_file_time = kernel32.SetFileTime
    set_file_time.argtypes = [
        wintypes.HANDLE,
        ctypes.POINTER(wintypes.FILETIME),
        ctypes.POINTER(wintypes.FILETIME),
        ctypes.POINTER(wintypes.FILETIME),
    ]
    set_file_time.restype = wintypes.BOOL
    close_handle = kernel32.CloseHandle
    close_handle.argtypes = [wintypes.HANDLE]
    close_handle.restype = wintypes.BOOL

    handle = create_file(
        str(path),
        file_write_attributes,
        file_share_all,
        None,
        open_existing,
        file_attribute_normal,
        None,
    )
    if handle == invalid_handle_value:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        # Windows FILETIME is a count of 100 ns intervals since 1601-01-01.
        ticks = created_ns // 100 + 116_444_736_000_000_000
        creation_time = wintypes.FILETIME(ticks & 0xFFFFFFFF, ticks >> 32)
        if not set_file_time(handle, ctypes.byref(creation_time), None, None):
            raise ctypes.WinError(ctypes.get_last_error())
    finally:
        close_handle(handle)


def apply_file_dates(path: Path, dates: FileDates) -> None:
    os.utime(path, ns=(dates.accessed_ns, dates.modified_ns))
    if dates.created_ns is not None:
        set_windows_creation_time(path, dates.created_ns)


def write_jsonl_atomic(
    path: Path,
    events: list[dict[str, Any]],
    file_dates: FileDates | None = None,
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        dir=path.parent,
        prefix=f".{path.name}.",
        suffix=".tmp",
        text=True,
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as output:
            for event in events:
                output.write(json.dumps(event, separators=(",", ":"), ensure_ascii=False))
                output.write("\n")
        # Apply dates before the atomic replace. If this fails, an existing
        # destination remains untouched and legacy source files remain intact.
        if file_dates is not None:
            apply_file_dates(temporary, file_dates)
        os.replace(temporary, path)
    except Exception:
        try:
            temporary.unlink()
        except OSError:
            pass
        raise


def dates_from_jsonl(path: Path) -> FileDates:
    events: list[dict[str, Any]] = []
    with path.open("rb") as source:
        for line_number, line in enumerate(source, start=1):
            if not line.endswith(b"\n"):
                raise ValueError(f"incomplete line {line_number}")
            try:
                event = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"invalid JSON on line {line_number}: {exc}") from exc
            if not isinstance(event, dict):
                raise ValueError(f"line {line_number} is not a JSON object")
            events.append(event)
    if not events:
        raise ValueError("file contains no events")

    event_times: list[int] = []
    started_times: list[int] = []
    body_modified_times: list[int] = []
    body_creation_times: list[int] = []
    for event in events:
        event_time = epoch_ns_from_iso(event.get("timestamp"))
        if event_time is not None:
            event_times.append(event_time)
            if str(event.get("event", "")).endswith("_started"):
                started_times.append(event_time)

        filename = event.get("filename")
        if not isinstance(filename, str) or Path(filename).name != filename:
            continue
        body_path = path.parent / filename
        try:
            body_stat = body_path.stat()
        except OSError:
            continue
        body_modified_times.append(body_stat.st_mtime_ns)
        body_created = source_creation_time_ns(body_stat)
        if body_created is not None:
            body_creation_times.append(body_created)

    if not event_times and not body_modified_times:
        raise ValueError("events and referenced body files have no usable timestamps")

    modified_ns = max(event_times) if event_times else max(body_modified_times)
    if started_times:
        created_ns = min(started_times)
    elif body_creation_times:
        created_ns = min(body_creation_times)
    elif event_times:
        created_ns = min(event_times)
    else:
        created_ns = None
    return FileDates(
        accessed_ns=modified_ns,
        modified_ns=modified_ns,
        created_ns=created_ns if os.name == "nt" else None,
    )


def repair_jsonl_file_times(log_dir: Path, dry_run: bool = False) -> int:
    if not log_dir.is_dir():
        print(f"error: log directory does not exist: {log_dir}", file=sys.stderr)
        return 2

    repaired = 0
    errors: list[str] = []
    for path in sorted(log_dir.glob("*_metadata.jsonl")):
        try:
            dates = dates_from_jsonl(path)
            if not dry_run:
                apply_file_dates(path, dates)
        except (OSError, ValueError) as exc:
            errors.append(f"failed to repair {path.name}: {exc}")
            continue
        action = "would repair" if dry_run else "repair"
        print(f"{action} {path.name}")
        repaired += 1

    print(f"summary: repaired={repaired} errors={len(errors)}")
    for error in errors:
        print(f"error: {error}", file=sys.stderr)
    return 1 if errors else 0


def migrate(
    log_dir: Path,
    dry_run: bool,
    overwrite: bool,
    delete_old: bool,
    preserve_file_times: bool = True,
) -> int:
    if not log_dir.is_dir():
        print(f"error: log directory does not exist: {log_dir}", file=sys.stderr)
        return 2

    records, errors = load_legacy_metadata(log_dir)
    transactions, grouping_errors = group_transactions(records)
    errors.extend(grouping_errors)
    migrated = skipped = 0

    for transaction_id, group in sorted(transactions.items(), key=lambda item: destination_for(item[1]).name):
        destination = destination_for(group)
        if destination.exists() and not overwrite:
            print(f"skip {transaction_id}: destination exists: {destination.name}")
            skipped += 1
            continue

        events: list[dict[str, Any]] = []
        for record in group:
            events.extend(make_events(record, log_dir))
        events.sort(key=event_sort_key)

        action = "would write" if dry_run else "write"
        sources = ", ".join(record.path.name for record in sorted(group, key=lambda value: value.stream_type))
        print(f"{action} {destination.name}: {len(events)} events from {sources}")
        if dry_run:
            migrated += 1
            continue

        try:
            file_dates = merged_file_dates(group) if preserve_file_times else None
            write_jsonl_atomic(destination, events, file_dates)
        except OSError as exc:
            errors.append(f"failed to write {destination.name}: {exc}")
            continue

        if delete_old:
            deletion_failed = False
            for record in group:
                try:
                    record.path.unlink()
                except OSError as exc:
                    errors.append(f"failed to delete {record.path.name}: {exc}")
                    deletion_failed = True
            if deletion_failed:
                # The JSONL is valid; report the transaction as migrated while
                # retaining a non-zero exit status for incomplete cleanup.
                pass
        migrated += 1

    print(f"summary: migrated={migrated} skipped={skipped} errors={len(errors)}")
    for error in errors:
        print(f"error: {error}", file=sys.stderr)
    return 1 if errors else 0


def main(argv: list[str] | None = None) -> int:
    options = parse_args(sys.argv[1:] if argv is None else argv)
    if options.repair_file_times:
        incompatible = options.overwrite or options.delete_old or options.no_preserve_file_times
        if incompatible:
            print(
                "error: --repair-file-times cannot be combined with --overwrite, "
                "--delete-old, or --no-preserve-file-times",
                file=sys.stderr,
            )
            return 2
        return repair_jsonl_file_times(options.log_dir, dry_run=options.dry_run)
    if options.dry_run and options.delete_old:
        print("error: --dry-run and --delete-old cannot be used together", file=sys.stderr)
        return 2
    return migrate(
        options.log_dir,
        dry_run=options.dry_run,
        overwrite=options.overwrite,
        delete_old=options.delete_old,
        preserve_file_times=not options.no_preserve_file_times,
    )


if __name__ == "__main__":
    raise SystemExit(main())
