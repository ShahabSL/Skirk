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
                                         bulk_window, cleanup_exact, main, serve_origins,
                                         require_client_namespaces, require_slot_activity)


class Running:
    def poll(self):
        return None


def metrics(units=100, errors=0, recent_units=None):
    return {"updated_at": datetime.now(timezone.utc).isoformat(),
            "quota": {"calls": 1, "units": units, "errors": errors},
            "recent_quota_per_minute": {"units": units if recent_units is None else recent_units},
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
            self.assertEqual(json.loads(stream.getvalue())["between_arm_seconds"], 65)
            self.assertEqual(json.loads(stream.getvalue())["bulk_stall_seconds"], 60)

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

    def test_recent_projection_does_not_masquerade_as_actual_rolling_usage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths = [root / f"metrics-{i}.json" for i in range(CLIENTS + 1)]
            actual = (14855, 4555, 3805, 3955, 4755, 4355)
            projected = (98400, 28200, 18600, 19200, 28800, 22800)
            for path, units, recent in zip(paths, actual, projected):
                path.write_text(json.dumps(metrics(units=units, recent_units=recent)))
            args = argparse.Namespace(max_arm_units=80_000, max_total_units=175_000,
                                      cleanup_reserve_units=20_000, max_rolling_units=100_000,
                                      max_recent_units_per_minute=250_000)
            guard = QuotaGuard(args)
            guard.check(paths, [Running()])
            self.assertEqual(guard.max_rolling, 36_280)
            self.assertEqual(guard.max_recent, 216_000)
            args.max_recent_units_per_minute = 150_000
            with self.assertRaisesRegex(RuntimeError, "recent_rate_guard"):
                guard.check(paths, [Running()])

    def test_actual_rolling_ceiling_has_its_own_abort_reason(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths = [root / f"metrics-{i}.json" for i in range(CLIENTS + 1)]
            for index, path in enumerate(paths):
                path.write_text(json.dumps(metrics(units=100_001 if index == 0 else 0,
                                                   recent_units=1)))
            args = argparse.Namespace(max_arm_units=200_000, max_total_units=250_000,
                                      cleanup_reserve_units=20_000, max_rolling_units=100_000,
                                      max_recent_units_per_minute=250_000)
            with self.assertRaisesRegex(RuntimeError, "rolling_quota_guard"):
                QuotaGuard(args).check(paths, [Running()])

    def test_cleanup_uses_only_exact_session_prefix_and_checks_twice(self):
        session = "00112233445566778899aabbccddeeff"
        prefix = f"muxv4/{session}/"
        commands = []

        def run(command, **_):
            commands.append(command)
            delete = "--delete" in command
            actual_prefix = command[command.index("--prefix") + 1]
            result = {"prefix": actual_prefix, "dry_run": not delete, "pages": 1,
                      "matched": 2 if delete else 0, "deleted": 2 if delete else 0,
                      "failed": 0, "truncated": False}
            return argparse.Namespace(returncode=0, stdout=json.dumps(result).encode())

        with mock.patch("subprocess.run", side_effect=run), mock.patch("time.sleep"):
            result = cleanup_exact(Path("/private/skirk"), Path("/private/exit.json"), session)
        self.assertTrue(result["ok"])
        self.assertEqual(len(commands), 6)
        self.assertEqual(result["estimated_units"], 6 * 100 + 4 * 50)
        for command in commands:
            self.assertIn(command[command.index("--prefix") + 1], [prefix, prefix.replace("muxv4/", "slotv1/")])
            self.assertNotIn("--all", command)

    def test_async_package_clears_saved_experiments_and_preserves_route(self):
        original = {"session_id": "old", "route": {"mode": "google_front_pinned", "google_ip": "127.0.0.1"},
                    "drive": {"metadata_slot_primary_v1": True, "metadata_slot_async_v1": True,
                              "upload_stall_hedge_delay_ms": 2500, "normal_list_page_size": 100,
                              "quota_project": "test-project"}, "tunnel": {"interaction_epoch": True}}
        saved = json.loads(json.dumps(original))
        baseline = arm_config(original, "baseline", "new")
        candidate = arm_config(original, "async", "next")
        self.assertEqual(original, saved)
        for config in (baseline, candidate):
            self.assertEqual(config["route"], original["route"])
            for key in ("metadata_slot_primary_v1", "upload_stall_hedge_delay_ms", "normal_list_page_size", "quota_project"):
                self.assertNotIn(key, config["drive"])
        self.assertNotIn("metadata_slot_async_v1", baseline["drive"])
        self.assertFalse(baseline["tunnel"]["interaction_epoch"])
        self.assertTrue(candidate["tunnel"]["interaction_epoch"])
        self.assertTrue(candidate["drive"]["metadata_slot_delivery_v1"])
        self.assertTrue(candidate["drive"]["metadata_slot_async_v1"])
        self.assertFalse(candidate["drive"]["metadata_only_priority"])

    def test_runtime_run_id_reuse_rejected_even_when_client_ids_differ(self):
        logs = [f"skirk client SOCKS5 listening on127.0.0.1 client=c{i} run=reused route=google_front_pinned" for i in range(CLIENTS)]
        with self.assertRaisesRegex(RuntimeError, "not_unique"):
            require_client_namespaces(logs)
        logs = [f"skirk client SOCKS5 listening client=c{i} run=r{i}" for i in range(CLIENTS)]
        self.assertEqual(len(set(require_client_namespaces(logs))), CLIENTS)

    def test_flag_only_or_prelude_only_async_activity_is_rejected(self):
        before = ["mux slot delivery active"] * CLIENTS
        mixed = ["\n".join(("mux slot async handoff", "mux slot async storage_ack",
                              "mux slot delivery receiver_processed", "mux slot delivery sender_ack"))] * CLIENTS
        self.assertEqual(len(require_slot_activity("async", before, mixed)), CLIENTS)
        missing = mixed.copy(); missing[3] = "mux slot async handoff"
        with self.assertRaisesRegex(RuntimeError, "activity_missing"):
            require_slot_activity("async", before, missing)
        with self.assertRaisesRegex(RuntimeError, "unexpected_control"):
            require_slot_activity("baseline", before, mixed)

    def test_late_slot_object_requires_second_full_visibility_check(self):
        session = "00112233445566778899aabbccddeeff"
        calls = {"slot_dry": 0}; delays = []
        def run(command, **_):
            prefix = command[command.index("--prefix") + 1]
            delete = "--delete" in command
            matched = 0
            if prefix.startswith("slotv1/") and not delete:
                calls["slot_dry"] += 1
                matched = 1 if calls["slot_dry"] == 2 else 0
            return argparse.Namespace(returncode=0, stdout=json.dumps({"prefix": prefix,
                "dry_run": not delete, "pages": 1, "matched": matched,
                "deleted": 0, "failed": 0, "truncated": False}).encode())
        with mock.patch("subprocess.run", side_effect=run), mock.patch("time.sleep", side_effect=delays.append):
            result = cleanup_exact(Path("/private/skirk"), Path("/private/exit.json"), session)
        self.assertTrue(result["ok"])
        self.assertEqual(delays, [31, 31])
        self.assertEqual(len(result["delayed_checks"]), 2)

    def test_bulk_window_requires_five_simultaneous_verified_files(self):
        reports = []
        for index in range(CLIENTS):
            reports.append({"probe_started_utc": "2026-11-14T00:00:00+00:00",
                            "samples": [{"kind": "bulk", "ok": True,
                                         "start_offset_ms": index * 10, "total_ms": 1000}]})
        seconds, peak = bulk_window(reports)
        self.assertEqual(peak, CLIENTS)
        self.assertAlmostEqual(seconds, 1.04, places=2)
        reports[2]["samples"][0]["ok"] = False
        seconds, peak = bulk_window(reports)
        self.assertIsNone(seconds)
        self.assertEqual(peak, CLIENTS)

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
