#!/usr/bin/env python3
"""Migrate legacy per-stream metadata JSON files to transaction JSONL.

The migration is non-destructive by default: old metadata JSON files and body
files are retained. Stop logging-proxy (or migrate a copy of its log directory)
so an in-progress legacy metadata file cannot change during migration.
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
EVENT_PRIORITY = {
    "request_started": 0,
    "response_started": 1,
    "request_completed": 2,
    "response_completed": 3,
}


@dataclass(frozen=True)
class LegacyMetadata:
    path: Path
    prefix: str
    short_id: str
    stream_type: str
    document: dict[str, Any]

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


class MigrationError(Exception):
    pass


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
    return parser.parse_args(argv)


def load_legacy_metadata(log_dir: Path) -> tuple[list[LegacyMetadata], list[str]]:
    records: list[LegacyMetadata] = []
    errors: list[str] = []
    for path in sorted(log_dir.glob("*_metadata.json")):
        match = OLD_NAME_RE.match(path.name)
        if not match:
            errors.append(f"unrecognized legacy metadata filename: {path.name}")
            continue
        try:
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


def write_jsonl_atomic(path: Path, events: list[dict[str, Any]]) -> None:
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
        os.replace(temporary, path)
    except Exception:
        try:
            temporary.unlink()
        except OSError:
            pass
        raise


def migrate(log_dir: Path, dry_run: bool, overwrite: bool, delete_old: bool) -> int:
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
            write_jsonl_atomic(destination, events)
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
    if options.dry_run and options.delete_old:
        print("error: --dry-run and --delete-old cannot be used together", file=sys.stderr)
        return 2
    return migrate(
        options.log_dir,
        dry_run=options.dry_run,
        overwrite=options.overwrite,
        delete_old=options.delete_old,
    )


if __name__ == "__main__":
    raise SystemExit(main())
