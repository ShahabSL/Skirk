import argparse
import errno
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from skirk_live_capacity_compare import ExactCleanupError, cleanup_exact, finalize_arm, run_arm


class Child:
    def __init__(self, pid, running=False):
        self.pid = pid
        self.running = running

    def poll(self):
        return None if self.running else 0


def snapshot(units=100):
    return {"quota": {"calls": 1, "units": units, "errors": 0}}


class CapacityTeardownTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.services = [Child(1000 + i) for i in range(6)]
        self.benches = [Child(2000 + i) for i in range(5)]
        self.profiles = [self.directory / f"profile-{i}.json" for i in range(6)]
        for path in self.profiles:
            path.write_text("private-runtime-config")
        self.metrics = [self.directory / f"metrics-{i}.json" for i in range(6)]
        for path in self.metrics:
            path.write_text(json.dumps(snapshot()))
        self.guard = argparse.Namespace(completed_units=1000,
            latest_snapshots=[snapshot() for _ in self.metrics],
            args=argparse.Namespace(max_total_units=10000))
        self.row = {"ok": False, "failure": "drive_error_or_backoff"}
        self.cleanup_calls = []

    def cleanup(self):
        self.cleanup_calls.append(True)
        self.assertTrue(all(not process.running for process in self.services))
        return {"ok": True, "estimated_units": 50}

    def run_teardown(self, stop=None, manifests=None):
        stop = stop or (lambda *_: {"ok": True, "all_stopped": True})
        with patch("skirk_live_capacity_compare.stop_processes", side_effect=stop):
            return finalize_arm(self.row, self.directory, self.services, self.benches,
                manifests or [], self.metrics, self.profiles, self.cleanup, self.guard)

    def test_signal_permission_error_still_attempts_all_six_writers_and_retains_original(self):
        calls = []
        for process in self.services:
            process.running = True

        def stop(processes, _records, **_):
            calls.append([process.pid for process in processes])
            # Outcome exists before even the first failed teardown operation.
            original = json.loads((self.directory / "workload-outcome.json").read_text())
            self.assertEqual(original["failure"], "drive_error_or_backoff")
            if len(calls) == 1:
                raise PermissionError(errno.EPERM, "Operation not permitted")
            for process in processes:
                process.running = False
            return {"ok": True, "all_stopped": True}

        result = self.run_teardown(stop)
        self.assertEqual(calls[1:12], [[process.pid] for process in [*self.benches, *self.services]])
        self.assertEqual(result["failure"], "drive_error_or_backoff")
        self.assertEqual(result["workload_failure"], "drive_error_or_backoff")
        self.assertTrue(result["all_original_google_writers_stopped"])
        self.assertEqual(result["teardown_failures"][0]["errno"], errno.EPERM)
        self.assertEqual(len(self.cleanup_calls), 1)
        self.assertTrue(all(not path.exists() for path in self.profiles))
        self.assertEqual(self.guard.completed_units, 1650)
        final = json.loads((self.directory / "teardown-outcome.json").read_text())
        self.assertEqual(final["failure"], "drive_error_or_backoff")

    def test_live_writer_blocks_all_drive_cleanup_but_profiles_and_budget_are_retained(self):
        self.services[-1].running = True
        result = self.run_teardown(lambda *_: {"ok": False, "all_stopped": False})
        self.assertEqual(self.cleanup_calls, [])
        self.assertEqual(result["cleanup"]["calls_started"], 0)
        self.assertEqual(result["cleanup"]["blocked"], "google_writer_exit_not_verified")
        self.assertEqual(self.guard.completed_units, 1600)
        self.assertTrue(all(not path.exists() for path in self.profiles))

    def test_exact_cleanup_exception_does_not_replace_original_failure(self):
        self.cleanup = lambda: (_ for _ in ()).throw(RuntimeError("cleanup_failed"))
        result = self.run_teardown()
        self.assertEqual(result["failure"], "drive_error_or_backoff")
        self.assertEqual(result["workload_failure"], "drive_error_or_backoff")
        self.assertTrue(any(f["phase"] == "exact_drive_cleanup" for f in result["teardown_failures"]))
        self.assertEqual(self.guard.completed_units, 1600)

    def test_failed_outcome_write_cannot_skip_owned_resource_shutdown(self):
        calls = []
        with patch("skirk_live_capacity_compare.durable_json", side_effect=[OSError(errno.EIO, "disk"), None]):
            result = self.run_teardown(lambda processes, _: calls.append(processes) or {"ok": True})
        self.assertEqual(calls[0], [*self.benches, *self.services])
        self.assertEqual(len(self.cleanup_calls), 1)
        self.assertEqual(result["teardown_failures"][0]["phase"], "persist_workload_outcome")

    def test_child_manifest_is_read_after_parent_shutdown_and_new_child_is_verified(self):
        main = self.directory / "processes.jsonl"
        child = self.directory / "curl.jsonl"
        main.write_text("")
        calls = []
        child_record = {"pid": 3000, "identity_status": "captured", "role": "curl"}

        def stop(processes, records):
            calls.append((list(processes), list(records)))
            if processes:
                child.write_text(json.dumps(child_record) + "\n")
            return {"ok": True, "all_stopped": True}

        with patch("skirk_live_capacity_compare.load_process_records", side_effect=lambda p: [json.loads(l) for l in p.read_text().splitlines()]):
            self.run_teardown(stop, [main, child])
        self.assertEqual(calls[1], ([], [child_record]))

    def test_unregistered_spawn_intent_is_failure_not_authority_to_signal_its_pid(self):
        main = self.directory / "processes.jsonl"
        child = self.directory / "curl.jsonl"
        child.with_name(child.name + ".spawn-pending").write_text('{"pid":123,"argv":[]}')
        calls = []
        result = self.run_teardown(lambda procs, records: calls.append(records) or {"ok": True}, [main, child])
        self.assertTrue(any(f["type"] == "UnresolvedSpawnIntent" for f in result["teardown_failures"]))
        self.assertEqual(calls[1], [])
        self.assertEqual(len(self.cleanup_calls), 1)  # All actual Google writers are dead.

    def test_live_benchmark_journal_cannot_hold_teardown_on_its_append_lock(self):
        main = self.directory / "processes.jsonl"
        child = self.directory / "curl.jsonl"
        main.write_text("")
        child.write_text("possibly locked")
        self.benches[-1].running = True
        reads = []

        def load(path):
            reads.append(path)
            self.assertNotEqual(path, child)
            return []

        with patch("skirk_live_capacity_compare.load_process_records", side_effect=load):
            result = self.run_teardown(lambda *_: {"ok": False}, [main, child])
        self.assertEqual(reads, [main])
        self.assertEqual(result["child_process_teardown"]["blocked"], "benchmark_exit_not_verified")
        self.assertEqual(len(self.cleanup_calls), 1)  # Google writers already stopped.

    def test_bad_manifest_does_not_skip_services_or_exact_cleanup(self):
        manifest = self.directory / "processes.jsonl"
        manifest.write_text("broken")
        with patch("skirk_live_capacity_compare.load_process_records", side_effect=ValueError("bad")):
            result = self.run_teardown(manifests=[manifest])
        self.assertTrue(any(f["phase"] == "read_process_manifest" for f in result["teardown_failures"]))
        self.assertTrue(result["all_original_google_writers_stopped"])
        self.assertEqual(len(self.cleanup_calls), 1)

    def test_missing_or_regressed_final_snapshot_cannot_erase_last_observed_units(self):
        self.metrics[0].write_text('{"quota":null}')
        self.metrics[1].write_text(json.dumps(snapshot(1)))
        result = self.run_teardown()
        self.assertFalse(result["telemetry_complete"])
        self.assertEqual(self.guard.completed_units, 1650)
        self.assertEqual(result["last_observed_role_quotas"]["exit"]["units"], 100)
        self.assertEqual(result["last_observed_role_quotas"]["client-0"]["units"], 100)

    def test_original_outcome_is_immutable_and_not_replaced_by_teardown_fields(self):
        self.run_teardown()
        original = json.loads((self.directory / "workload-outcome.json").read_text())
        self.assertNotIn("process_teardown", original)
        self.assertEqual(original["workload_failure"], "drive_error_or_backoff")
        self.assertEqual((self.directory / "workload-outcome.json").stat().st_mode & 0o777, 0o600)

    def test_partial_cleanup_timeout_keeps_successful_deletions_and_minimum_units(self):
        session = "00112233445566778899aabbccddeeff"
        commands = []

        def invoke(command, **_):
            commands.append(command)
            if len(commands) == 2:
                raise subprocess.TimeoutExpired(command, 90)
            prefix = command[command.index("--prefix") + 1]
            result = {"prefix": prefix, "dry_run": False, "pages": 1,
                      "matched": 2, "deleted": 2, "failed": 0, "truncated": False}
            return argparse.Namespace(returncode=0, stdout=json.dumps(result).encode())

        with patch("skirk_live_capacity_compare.subprocess.run", side_effect=invoke), self.assertRaises(ExactCleanupError) as caught:
            cleanup_exact(Path("/owned/skirk"), Path("/owned/exit.json"), session)
        partial = caught.exception.partial_result
        self.assertEqual(partial["deleted"], 2)
        self.assertEqual(partial["estimated_units"], 200)
        self.assertEqual(partial["commands_started"], 2)
        self.assertFalse(partial["operations"][-1]["complete"])
        self.cleanup = lambda: (_ for _ in ()).throw(caught.exception)
        result = self.run_teardown()
        self.assertEqual(result["cleanup"]["deleted"], 2)
        self.assertEqual(result["failure"], "drive_error_or_backoff")
        self.assertEqual(self.guard.completed_units, 1800)

    def test_partial_profile_write_is_inside_teardown_protection(self):
        binary = self.directory / "fake-skirk"
        binary.write_bytes(b"not-executed")
        args = argparse.Namespace(out=self.directory, binary=binary, control_binary=binary, port_base=18100)
        originals = ({"drive": {}, "tunnel": {}}, {"drive": {}, "tunnel": {}})
        from skirk_live_capacity_compare import private_json
        writes = []

        def write(path, value):
            writes.append(path)
            if len(writes) == 3:
                raise OSError(errno.EIO, "partial profile setup")
            private_json(path, value)

        with patch("skirk_live_capacity_compare.private_json", side_effect=write), \
             patch("skirk_live_capacity_compare.subprocess.Popen", side_effect=AssertionError("no child should start")):
            result = run_arm(args, 0, "baseline", originals, self.guard,
                             self.directory / "unused", "unused", 1, 2, [0])
        self.assertEqual(result["workload_failure"], "OSError")
        self.assertTrue((self.directory / "0-baseline/workload-outcome.json").exists())
        self.assertEqual(list((self.directory / "0-baseline").glob("*config.json")), [])
        self.assertEqual(result["cleanup"]["calls_started"], 0)

    def test_binary_read_failure_has_a_durable_outcome_and_no_credentials_or_child(self):
        missing = self.directory / "missing-skirk"
        args = argparse.Namespace(out=self.directory, binary=missing, control_binary=missing, port_base=18100)
        with patch("skirk_live_capacity_compare.subprocess.Popen", side_effect=AssertionError("no child should start")):
            result = run_arm(args, 0, "baseline", ({}, {}), self.guard,
                             self.directory / "unused", "unused", 1, 2, [0])
        self.assertEqual(result["workload_failure"], "FileNotFoundError")
        self.assertTrue((self.directory / "0-baseline/workload-outcome.json").exists())
        self.assertEqual(list((self.directory / "0-baseline").glob("*config.json")), [])


if __name__ == "__main__":
    unittest.main()
