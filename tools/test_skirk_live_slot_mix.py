import argparse
import contextlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

from skirk_live_slot_mix import (TEXT_COUNT, TEXT_INTERVAL_MS, control_summary,
                                 main, slot_summary)


class SlotMixTests(unittest.TestCase):
    def test_validate_only_rejects_live_work_and_hides_kit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            kit = root / "kit"
            kit.mkdir()
            config = {"secret": "never-print-secret", "session_id": "00112233445566778899aabbccddeeff",
                      "route": {"mode": "google_front_pinned", "google_ip": "127.0.0.1"},
                      "drive": {"folder_id": "private-folder"},
                      "auth": {"refresh_token": "never-print-token"}}
            for role in ("client", "exit"):
                (kit / f"{role}.json").write_text(json.dumps(config))
            binary = root / "skirk"
            slot = root / "ack-slot"
            binary.write_bytes(b"fake-skirk")
            slot.write_bytes(b"fake-slot")
            out = root / ".skirk-runs" / "mixed"
            out.parent.mkdir()
            stream = io.StringIO()
            with contextlib.redirect_stdout(stream), mock.patch("subprocess.Popen", side_effect=AssertionError("live process")):
                code = main(["--binary", str(binary), "--slot-binary", str(slot),
                             "--kit-dir", str(kit), "--out", str(out), "--validate-only"])
            self.assertEqual(code, 0)
            value = json.loads(stream.getvalue())
            self.assertFalse(out.exists())
            self.assertFalse(value["live_calls"])
            self.assertEqual(value["comparison"], "mixed_substrate_not_mux_integration")
            self.assertEqual(value["reader_pause_ms"], 0)
            self.assertNotIn("never-print", stream.getvalue())

    def test_control_one_way_arrival_requires_every_verified_echo(self):
        started = datetime(2026, 11, 15, tzinfo=timezone.utc)
        samples = [{"kind": "small", "index": index, "ok": True,
                    "overlapped_bulk_at_start": True, "scheduled_ttfb_ms": 800.0}
                   for index in range(TEXT_COUNT)]
        samples.append({"kind": "bulk", "ok": True, "integrity_checked": True,
                        "http_status": 200, "transfer_bytes": 1024 * 1024,
                        "goodput_mbps": 9.0, "total_ms": 10000.0})
        report = {"prelude": {"ok": True}, "probe_started_utc": started.isoformat(),
                  "samples": samples}
        arrivals = {index: started + timedelta(milliseconds=250 + index * TEXT_INTERVAL_MS + 500)
                    for index in range(TEXT_COUNT)}
        args = argparse.Namespace(file_mib=1)
        result = control_summary(report, args, arrivals)
        self.assertTrue(result["workload_ok"])
        self.assertEqual(result["text_one_way_scheduled_p95_ms"], 500.0)
        self.assertEqual(result["text_scheduled_p95_ms"], 800.0)
        del arrivals[12]
        self.assertFalse(control_summary(report, args, arrivals)["workload_ok"])

    def test_slot_requires_file_overlap_and_exact_cleanup(self):
        started = datetime(2026, 11, 15, tzinfo=timezone.utc)
        completed = started + timedelta(seconds=7)
        report = {"ok": True, "frames_received": TEXT_COUNT,
                  "frames_acknowledged": TEXT_COUNT, "cleanup_verified": True,
                  "reader_pause_ms": 0, "workload_completed_utc": completed.isoformat(),
                  "text_scheduled_p95_ms": 700.0, "quota_units_upper_bound": 3000}
        sample = {"ok": True, "integrity_checked": True, "http_status": 200,
                  "transfer_bytes": 1024 * 1024, "goodput_mbps": 9.0,
                  "total_ms": 11000.0}
        args = argparse.Namespace(file_mib=1, reader_pause_ms=0)
        result = slot_summary(report, sample, args, started, started + timedelta(seconds=11),
                              started + timedelta(milliseconds=100), False)
        self.assertTrue(result["workload_ok"])
        self.assertEqual(result["text_one_way_scheduled_p95_ms"], 700.0)
        self.assertFalse(slot_summary(report, sample, args, started, started + timedelta(seconds=5),
                                      started + timedelta(milliseconds=100), False)["workload_ok"])
        self.assertFalse(slot_summary(report, sample, args, started, started + timedelta(seconds=11),
                                      started + timedelta(milliseconds=100), True)["workload_ok"])

    def test_bulk_worker_cannot_touch_proxy_without_live_flag(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / ".skirk-runs"
            root.mkdir()
            ready = root / "ready"
            out = root / "bulk.json"
            worker = Path(__file__).with_name("skirk_slot_bulk_worker.py")
            command = [sys.executable, str(worker), "--port", "18200",
                       "--url", "http://127.0.0.1:12345/file", "--direction", "download",
                       "--file-mib", "1", "--expected-bytes", "1048576",
                       "--expected-sha256", "0" * 64,
                       "--ready-file", str(ready), "--out", str(out)]
            completed = subprocess.run(command, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(completed.returncode, 0)
            self.assertFalse(ready.exists())
            self.assertFalse(out.exists())


if __name__ == "__main__":
    unittest.main()
