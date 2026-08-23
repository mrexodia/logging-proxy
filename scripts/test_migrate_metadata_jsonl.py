#!/usr/bin/env python3

from __future__ import annotations

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import migrate_metadata_jsonl as migration  # noqa: E402


class MetadataMigrationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.log_dir = Path(self.temporary.name)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write_metadata(
        self,
        prefix: str,
        short_id: str,
        stream_type: str,
        *,
        started_at: str,
        completed_at: str | None,
        completed: bool | None,
        body: bytes | None,
    ) -> Path:
        filename = f"{prefix}_{short_id}_{stream_type}.bin"
        document = {
            "stream_type": stream_type,
            "metadata": {
                "id": f"{short_id}-1234-1234-1234-123456789abc",
                "method": "POST",
                "source_url": "http://proxy.test/api",
                "target_url": "https://backend.test/api",
                "request_started_at": "2026-08-23T15:00:00.100+02:00",
            },
            "timestamp": started_at,
            "started_at": started_at,
            "filename": filename,
        }
        if completed is not None:
            document["completed"] = completed
        if completed_at is not None:
            document["completed_at"] = completed_at
            document["duration_ms"] = 50
        if body is not None:
            (self.log_dir / filename).write_bytes(body)
            document["bytes_written"] = len(body)

        path = self.log_dir / f"{prefix}_{short_id}_{stream_type}_metadata.json"
        path.write_text(json.dumps(document), encoding="utf-8")
        return path

    def read_jsonl(self) -> tuple[Path, list[dict]]:
        matches = list(self.log_dir.glob("*_metadata.jsonl"))
        self.assertEqual(len(matches), 1)
        lines = matches[0].read_text(encoding="utf-8").splitlines()
        return matches[0], [json.loads(line) for line in lines]

    def test_migrates_request_and_response_to_one_jsonl(self) -> None:
        request = self.write_metadata(
            "2026-08-23_15-00-00.100",
            "abcdef12",
            "request",
            started_at="2026-08-23T15:00:00.100+02:00",
            completed_at="2026-08-23T15:00:00.150+02:00",
            completed=True,
            body=b"request",
        )
        response = self.write_metadata(
            "2026-08-23_15-00-00.200",
            "abcdef12",
            "response",
            started_at="2026-08-23T15:00:00.200+02:00",
            completed_at="2026-08-23T15:00:00.250+02:00",
            completed=True,
            body=b"response",
        )

        result = migration.migrate(self.log_dir, dry_run=False, overwrite=False, delete_old=False)
        self.assertEqual(result, 0)
        destination, events = self.read_jsonl()
        self.assertEqual(destination.name, "2026-08-23_15-00-00.100_abcdef12_metadata.jsonl")
        self.assertEqual(
            [event["event"] for event in events],
            ["request_started", "request_completed", "response_started", "response_completed"],
        )
        self.assertEqual(events[1]["bytes_written"], len(b"request"))
        self.assertEqual(events[3]["bytes_written"], len(b"response"))
        self.assertTrue(request.exists())
        self.assertTrue(response.exists())

    def test_infers_completion_for_legacy_metadata(self) -> None:
        prefix = "2025-09-13_22-56-08.474"
        short_id = "fe78a802"
        body_filename = f"{prefix}_{short_id}_request.bin"
        (self.log_dir / body_filename).write_bytes(b"legacy body")
        legacy = {
            "stream_type": "request",
            "metadata": {"id": "fe78a802-1234-1234-1234-123456789abc", "method": "GET"},
            "timestamp": "2025-09-13T22:56:08.474915+02:00",
            "filename": body_filename,
        }
        (self.log_dir / f"{prefix}_{short_id}_request_metadata.json").write_text(
            json.dumps(legacy), encoding="utf-8"
        )

        result = migration.migrate(self.log_dir, dry_run=False, overwrite=False, delete_old=False)
        self.assertEqual(result, 0)
        _, events = self.read_jsonl()
        self.assertEqual([event["event"] for event in events], ["request_started", "request_completed"])
        self.assertTrue(events[1]["completed"])
        self.assertEqual(events[1]["bytes_written"], len(b"legacy body"))

    def test_preserves_merged_source_file_dates(self) -> None:
        request = self.write_metadata(
            "2026-08-23_15-00-00.100",
            "feedface",
            "request",
            started_at="2026-08-23T15:00:00.100+02:00",
            completed_at="2026-08-23T15:00:00.150+02:00",
            completed=True,
            body=b"request",
        )
        response = self.write_metadata(
            "2026-08-23_15-00-00.200",
            "feedface",
            "response",
            started_at="2026-08-23T15:00:00.200+02:00",
            completed_at="2026-08-23T15:00:00.250+02:00",
            completed=True,
            body=b"response",
        )
        request_times = (1_600_000_001_000_000_000, 1_600_000_002_000_000_000)
        response_times = (1_600_000_003_000_000_000, 1_600_000_004_000_000_000)
        os.utime(request, ns=request_times)
        os.utime(response, ns=response_times)
        expected_creation = min(request.stat().st_ctime_ns, response.stat().st_ctime_ns)

        result = migration.migrate(self.log_dir, dry_run=False, overwrite=False, delete_old=False)
        self.assertEqual(result, 0)
        destinations = list(self.log_dir.glob("*_metadata.jsonl"))
        self.assertEqual(len(destinations), 1)
        destination_stat = destinations[0].stat()
        self.assertEqual(destination_stat.st_atime_ns, response_times[0])
        self.assertEqual(destination_stat.st_mtime_ns, response_times[1])
        if os.name == "nt":
            self.assertEqual(destination_stat.st_ctime_ns, expected_creation)
        self.read_jsonl()

    def test_repairs_existing_jsonl_dates_from_events(self) -> None:
        self.write_metadata(
            "2026-08-23_15-00-00.100",
            "cafebabe",
            "request",
            started_at="2026-08-23T15:00:00.1000001+02:00",
            completed_at="2026-08-23T15:00:00.1500002+02:00",
            completed=True,
            body=b"request",
        )
        self.write_metadata(
            "2026-08-23_15-00-00.200",
            "cafebabe",
            "response",
            started_at="2026-08-23T15:00:00.2000003+02:00",
            completed_at="2026-08-23T15:00:00.2500004+02:00",
            completed=True,
            body=b"response",
        )
        result = migration.migrate(
            self.log_dir,
            dry_run=False,
            overwrite=False,
            delete_old=True,
            preserve_file_times=False,
        )
        self.assertEqual(result, 0)
        destination = next(self.log_dir.glob("*_metadata.jsonl"))

        result = migration.repair_jsonl_file_times(self.log_dir)
        self.assertEqual(result, 0)
        destination_stat = destination.stat()
        self.assertEqual(
            destination_stat.st_mtime_ns,
            migration.epoch_ns_from_iso("2026-08-23T15:00:00.2500004+02:00"),
        )
        if os.name == "nt":
            self.assertEqual(
                destination_stat.st_ctime_ns,
                migration.epoch_ns_from_iso("2026-08-23T15:00:00.1000001+02:00"),
            )

    def test_preserves_explicit_incomplete_stream_as_unmatched_start(self) -> None:
        self.write_metadata(
            "2026-08-23_15-00-00.100",
            "deadbeef",
            "request",
            started_at="2026-08-23T15:00:00.100+02:00",
            completed_at=None,
            completed=False,
            body=b"partial",
        )

        result = migration.migrate(self.log_dir, dry_run=False, overwrite=False, delete_old=False)
        self.assertEqual(result, 0)
        _, events = self.read_jsonl()
        self.assertEqual([event["event"] for event in events], ["request_started"])

    def test_delete_old_removes_only_metadata_json(self) -> None:
        old = self.write_metadata(
            "2026-08-23_15-00-00.100",
            "12345678",
            "request",
            started_at="2026-08-23T15:00:00.100+02:00",
            completed_at="2026-08-23T15:00:00.150+02:00",
            completed=True,
            body=b"body",
        )
        body_path = self.log_dir / "2026-08-23_15-00-00.100_12345678_request.bin"

        result = migration.migrate(self.log_dir, dry_run=False, overwrite=False, delete_old=True)
        self.assertEqual(result, 0)
        self.assertFalse(old.exists())
        self.assertTrue(body_path.exists())
        self.read_jsonl()

    def test_dry_run_does_not_write(self) -> None:
        self.write_metadata(
            "2026-08-23_15-00-00.100",
            "87654321",
            "request",
            started_at="2026-08-23T15:00:00.100+02:00",
            completed_at="2026-08-23T15:00:00.150+02:00",
            completed=True,
            body=b"body",
        )

        result = migration.migrate(self.log_dir, dry_run=True, overwrite=False, delete_old=False)
        self.assertEqual(result, 0)
        self.assertEqual(list(self.log_dir.glob("*_metadata.jsonl")), [])


if __name__ == "__main__":
    unittest.main()
