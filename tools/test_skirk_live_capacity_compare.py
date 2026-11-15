import argparse
import contextlib
import hashlib
import io
import json
import tempfile
import unittest
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock

from skirk_live_capacity_compare import (CLIENTS, QuotaGuard, arm_config,
                                         bulk_window, cleanup_exact, main, serve_origins)


class Running:
    def poll(self):
        return None


def metrics(units=100, errors=0):
    return {"updated_at": datetime.now(timezone.utc).isoformat(),
            "quota": {"calls": 1, "units": units, "errors": errors},
            "recent_quota_per_minute": {"units": units},
            "drive_backoff": {"active": False}}


class CapacityCompareTests(unittest.TestCase):
    def test_config_isolates_epoch_metadata_without_mutating_kit(self):
        original = {"session_id": "old", "secret": "private", "route": {"mode": "google_front_pinned"},
                    "tunnel": {"sparse_priority": True}, "drive": {"metadata_only_priority": True}}
        baseline = arm_config(original, "baseline", "new")
        epoch = arm_config(original, "epoch", "next")
        self.assertEqual(original["session_id"], "old")
        self.assertTrue(original["tunnel"]["sparse_priority"])
        self.assertEqual(baseline["session_id"], "new")
        self.assertEqual(epoch["session_id"], "next")
        self.assertFalse(baseline["tunnel"]["sparse_priority"])
        self.assertFalse(baseline["tunnel"]["interaction_epoch"])
        self.assertFalse(baseline["drive"]["metadata_only_priority"])
        self.assertTrue(epoch["tunnel"]["interaction_epoch"])
        self.assertTrue(epoch["drive"]["metadata_only_priority"])
        self.assertEqual(baseline["route"], epoch["route"])

    def test_validate_only_never_starts_live_work_or_prints_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            kit = root / "kit"
            kit.mkdir()
            config = {"secret": "do-not-print-secret", "session_id": "00112233445566778899aabbccddeeff",
                      "route": {"mode": "google_front_pinned", "google_ip": "127.0.0.1"},
                      "drive": {"folder_id": "private-folder"},
                      "auth": {"refresh_token": "do-not-print-token"}}
            for role in ("client", "exit"):
                (kit / f"{role}.json").write_text(json.dumps(config))
            binary = root / "skirk"
            binary.write_bytes(b"fake-binary")
            output = root / ".skirk-runs" / "capacity"
            output.parent.mkdir()
            stream = io.StringIO()
            with contextlib.redirect_stdout(stream), mock.patch("subprocess.Popen", side_effect=AssertionError("live process")):
                code = main(["--binary", str(binary), "--kit-dir", str(kit), "--out", str(output), "--validate-only"])
            self.assertEqual(code, 0)
            self.assertFalse(output.exists())
            self.assertNotIn("do-not-print", stream.getvalue())
            self.assertIn('"live_calls": false', stream.getvalue())

    def test_both_end_quota_guard_stops_on_one_client_error(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths = [root / f"metrics-{i}.json" for i in range(CLIENTS + 1)]
            for path in paths:
                path.write_text(json.dumps(metrics()))
            args = argparse.Namespace(max_arm_units=80_000, max_total_units=175_000,
                                      cleanup_reserve_units=20_000, max_rolling_units=100_000,
                                      max_recent_units_per_minute=150_000)
            guard = QuotaGuard(args)
            self.assertEqual(len(guard.check(paths, [Running()])), CLIENTS + 1)
            paths[-1].write_text(json.dumps(metrics(errors=1)))
            with self.assertRaisesRegex(RuntimeError, "drive_error_or_backoff"):
                guard.check(paths, [Running()])

    def test_cleanup_uses_only_exact_session_prefix_and_checks_twice(self):
        session = "00112233445566778899aabbccddeeff"
        prefix = f"muxv4/{session}/"
        commands = []

        def run(command, **_):
            commands.append(command)
            delete = "--delete" in command
            result = {"prefix": prefix, "dry_run": not delete, "pages": 1,
                      "matched": 2 if delete else 0, "deleted": 2 if delete else 0,
                      "failed": 0, "truncated": False}
            return argparse.Namespace(returncode=0, stdout=json.dumps(result).encode())

        with mock.patch("subprocess.run", side_effect=run), mock.patch("time.sleep"):
            result = cleanup_exact(Path("/private/skirk"), Path("/private/exit.json"), session)
        self.assertTrue(result["ok"])
        self.assertEqual(len(commands), 3)
        self.assertEqual(result["estimated_units"], 3 * 100 + 2 * 50)
        for command in commands:
            self.assertEqual(command[command.index("--prefix") + 1], prefix)
            self.assertNotIn("--all", command)

    def test_bulk_window_requires_five_simultaneous_verified_files(self):
        reports = []
        for index in range(CLIENTS):
            reports.append({"probe_started_utc": "2026-11-14T00:00:00+00:00",
                            "samples": [{"kind": "bulk", "ok": True,
                                         "start_offset_ms": index * 10, "total_ms": 1000}]})
        seconds, peak = bulk_window(reports)
        self.assertEqual(peak, CLIENTS)
        self.assertAlmostEqual(seconds, 1.04, places=2)

    def test_local_origin_accepts_only_exact_file_hash(self):
        payload = b"synthetic-file-contents"
        server, echo, failures = serve_origins(hashlib.sha256(payload).hexdigest(), len(payload))
        try:
            url = f"http://127.0.0.1:{server.server_address[1]}/file"
            with urllib.request.urlopen(urllib.request.Request(url, data=payload, method="POST"), timeout=2) as response:
                self.assertEqual(response.read(), b"skirk-capacity-upload-ok")
            with self.assertRaises(urllib.error.HTTPError) as error:
                urllib.request.urlopen(urllib.request.Request(url, data=b"X" + payload[1:], method="POST"), timeout=2)
            error.exception.close()
            self.assertEqual(failures[0], 1)
        finally:
            server.shutdown()
            echo.shutdown()
            server.server_close()
            echo.server_close()


if __name__ == "__main__":
    unittest.main()
