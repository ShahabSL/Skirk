import json
import hashlib
import subprocess
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

from skirk_live_capacity_bench import require_metrics
from skirk_live_ab import arm_config, validate_arm_order, workload_completed
from skirk_live_mix_bench import execute_curl, run_one
from skirk_stage_analysis import arm_summary, duration_ms


class CapacityTelemetryTests(unittest.TestCase):
    def test_missing_metrics_never_mean_zero_usage(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                require_metrics(Path(directory) / "missing.json")

    def test_stale_metrics_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "metrics.json"
            path.write_text(json.dumps({"updated_at": (datetime.now(timezone.utc) - timedelta(minutes=1)).isoformat(),
                                        "quota": {"calls": 12, "units": 1200, "errors": 0}}))
            with self.assertRaises(RuntimeError):
                require_metrics(path)

    def test_fresh_zero_usage_is_valid(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "metrics.json"
            path.write_text(json.dumps({"updated_at": datetime.now(timezone.utc).isoformat(),
                                        "quota": {"calls": 0, "units": 0, "errors": 0}}))
            self.assertEqual(require_metrics(path)["quota"]["units"], 0)

    def test_malformed_telemetry_is_rejected(self):
        for timestamp, units in [(42, 0), (datetime.now(timezone.utc).isoformat(), True)]:
            with self.subTest(timestamp=timestamp, units=units), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "metrics.json"
                path.write_text(json.dumps({"updated_at": timestamp, "quota": {"calls": 0, "units": units, "errors": 0}}))
                with self.assertRaises(RuntimeError):
                    require_metrics(path)


class BodyIntegrityTests(unittest.TestCase):
    def test_exact_length_and_hash_and_private_body_cleanup(self):
        expected = b"correct response"
        for body, want in [(expected, None), (b"corrupt response", "body_hash_mismatch"),
                           (expected[:-1], "body_size_mismatch")]:
            with self.subTest(want=want):
                paths = []

                def fake_curl(command, **kwargs):
                    path = Path(command[command.index("--output") + 1])
                    paths.append(path)
                    path.write_bytes(body)
                    return subprocess.CompletedProcess(command, 0, "{}", "")

                with patch("skirk_live_mix_bench.subprocess.run", side_effect=fake_curl):
                    _, failure = execute_curl(["curl", "--output", "/dev/null"], 5, len(expected), hashlib.sha256(expected).hexdigest())
                self.assertEqual(failure, want)
                self.assertFalse(paths[0].parent.exists())

    def test_hash_mismatch_is_request_failure_even_after_http_200(self):
        timing = json.dumps({"http_code": 200, "time_starttransfer": 1, "time_total": 2, "size_download": 16})
        result = subprocess.CompletedProcess([], 0, timing, "")
        with patch("skirk_live_mix_bench.time.monotonic", return_value=10), \
             patch("skirk_live_mix_bench.execute_curl", return_value=(result, "body_hash_mismatch")):
            sample = run_one("small", 0, "http://127.0.0.1/small", 1, 9, 8, 20, 5, 2, 1, 1, 100, True, [], 16, "0" * 64)
        self.assertFalse(sample["ok"])
        self.assertEqual(sample["failure"], "body_hash_mismatch")
        self.assertEqual(sample["scheduled_ttfb_ms"], 2000)

    def test_body_cleanup_on_timeout(self):
        paths = []

        def timeout(command, **kwargs):
            path = Path(command[command.index("--output") + 1])
            paths.append(path)
            path.write_bytes(b"partial")
            raise subprocess.TimeoutExpired(command, 1)

        with patch("skirk_live_mix_bench.subprocess.run", side_effect=timeout), self.assertRaises(subprocess.TimeoutExpired):
            execute_curl(["curl", "--output", "/dev/null"], 1, 42, None)
        self.assertFalse(paths[0].parent.exists())


class MainComparisonSafetyTests(unittest.TestCase):
    def test_only_balanced_main_comparison_is_allowed(self):
        for order in (["main", "current", "current", "main"], ["current", "main", "main", "current"]):
            self.assertTrue(validate_arm_order(order, Path("main-binary")))
        for order, main_binary in [(["main", "current"], Path("main-binary")),
                                   (["main", "current", "current", "main"], None),
                                   (["main", "inline", "inline", "main"], Path("main-binary"))]:
            with self.subTest(order=order), self.assertRaises(ValueError):
                validate_arm_order(order, main_binary)

    def test_matching_config_removes_secondary_project_and_experiments_without_touching_kit(self):
        original = {"secret": "fictional", "session_id": "old", "route": {"mode": "google_front_pinned"},
                    "drive": {"folder_id": "test-folder", "quota_project": "different-project",
                              "inline_small_objects": True, "upload_hedge_delay_ms": 3000,
                              "separate_upload_pool": True, "metadata_only_priority": True,
                              "separate_bulk_media_pool": True},
                    "tunnel": {"chunk_size": 8388608}}
        first = arm_config(original, "main", "fresh-one", 0, True)
        second = arm_config(original, "current", "fresh-two", 0, True)
        self.assertEqual(first["drive"], second["drive"])
        self.assertEqual(first["drive"], {"folder_id": "test-folder"})
        self.assertEqual(first["route"], second["route"])
        self.assertEqual(first["tunnel"], second["tunnel"])
        self.assertEqual(first["session_id"], "fresh-one")
        self.assertEqual(second["session_id"], "fresh-two")
        self.assertEqual(original["session_id"], "old")
        self.assertEqual(original["drive"]["quota_project"], "different-project")

    def test_metadata_arm_is_opt_in_and_leaves_default_route(self):
        original = {"session_id": "old", "route": {"mode": "google_front_pinned"}, "drive": {}}
        self.assertFalse(arm_config(original, "baseline", "a", 0, False)["drive"]["metadata_only_priority"])
        candidate = arm_config(original, "metadata", "b", 0, False)
        self.assertTrue(candidate["drive"]["metadata_only_priority"])
        self.assertFalse(candidate["drive"]["inline_small_objects"])
        self.assertEqual(candidate["route"]["mode"], "google_front_pinned")
        self.assertEqual(original["drive"], {})

    def test_bulk_pool_arm_is_opt_in_and_leaves_default_route(self):
        original = {"session_id": "old", "route": {"mode": "google_front_pinned"}, "drive": {}}
        self.assertFalse(arm_config(original, "baseline", "a", 0, False)["drive"]["separate_bulk_media_pool"])
        candidate = arm_config(original, "bulk_pool", "b", 0, False)
        self.assertTrue(candidate["drive"]["separate_bulk_media_pool"])
        self.assertFalse(candidate["drive"]["metadata_only_priority"])
        self.assertEqual(candidate["route"]["mode"], "google_front_pinned")
        hedged = arm_config(original, "bulk_pool_hedge", "c", 0, False)
        self.assertTrue(hedged["drive"]["separate_bulk_media_pool"])
        self.assertEqual(hedged["drive"]["upload_hedge_delay_ms"], 3000)

    def test_incomplete_workload_stays_a_failed_arm(self):
        result = {"small": {"scheduled": 24, "successes": 23}, "bulk": {"scheduled": 1, "successes": 1}}
        self.assertFalse(workload_completed(result, 24, 1))
        result["small"]["successes"] = 24
        self.assertTrue(workload_completed(result, 24, 0))
        result["small"]["scheduled"] = 23
        with self.assertRaises(RuntimeError):
            workload_completed(result, 24, 0)


class StageAnalysisPrivacyTests(unittest.TestCase):
    def test_go_durations(self):
        self.assertEqual(duration_ms("1m2.5s"), 62500)
        self.assertAlmostEqual(duration_ms("150µs"), 0.15)
        with self.assertRaises(ValueError):
            duration_ms("not-a-duration")

    def test_only_aggregate_stage_numbers_leave_logs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            arm = root / "0-main"
            arm.mkdir()
            (arm / "exit.log").write_text(
                "mux upload role=exit priority=false target=secret-target object=secret-object "
                "queue_delay=120ms slot_wait=1s put_duration=2.5s error=none\n"
                "mux poll role=exit source=prefix_normal prefix=secret-prefix duration=150ms\n")
            (arm / "exit-metrics.json").write_text(json.dumps({"quota": {
                "calls": 2, "units": "secret-token", "errors": 0,
                "ops": {"upload": {"calls": 1, "units": 50, "errors": 0}}}}))
            result = arm_summary(root, {"run": "0-main", "ok": True,
                                        "failure": "secret-failure", "bulk_mbps": 10.0}, 0)
            encoded = json.dumps(result)
            self.assertNotIn("secret", encoded)
            self.assertEqual(result["quota"]["exit"]["estimated_units"], None)
            upload = next(stage for stage in result["stage_samples"] if stage["kind"] == "upload")
            self.assertEqual(upload["timings"]["put_duration_ms"]["p95_ms"], 2500)
            self.assertEqual(upload["timings"]["slot_wait_ms"]["p50_ms"], 1000)


if __name__ == "__main__":
    unittest.main()
