import json
import hashlib
import subprocess
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

from skirk_live_capacity_bench import require_metrics
from skirk_live_mix_bench import execute_curl, run_one


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


if __name__ == "__main__":
    unittest.main()
