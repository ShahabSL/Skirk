import errno
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import skirk_ab_finalize as module
from skirk_ab_finalize import ArmFinalizer
import skirk_owned_processes as owned


class FakeProcess:
    next_pid = 900000

    def __init__(self, denied=False, bad_poll=False):
        FakeProcess.next_pid += 1
        self.pid = FakeProcess.next_pid
        self.args = ["public-owned-fixture", str(self.pid)]
        self.returncode = None
        self.denied, self.bad_poll = denied, bad_poll
        self.signals = []

    def poll(self):
        if self.bad_poll:
            raise OSError(errno.EIO, "public poll failure")
        return self.returncode

    def terminate(self):
        self.signals.append("TERM")
        if self.denied:
            raise PermissionError(errno.EPERM, "public denied fixture")
        self.returncode = -15

    def kill(self):
        self.signals.append("KILL")
        if self.denied:
            raise PermissionError(errno.EPERM, "public denied fixture")
        self.returncode = -9

    def wait(self, timeout=None):
        if self.returncode is None:
            raise subprocess.TimeoutExpired("public fixture", timeout)
        return self.returncode


class FinalizeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name) / ".skirk-runs" / "arm"
        self.directory.mkdir(parents=True, mode=0o700)
        self.directory.chmod(0o700)
        self.finalizer = ArmFinalizer(self.directory)
        self.register_patch = mock.patch.object(owned, "append_process_record", side_effect=self.record)
        self.register_patch.start()
        self.addCleanup(self.register_patch.stop)
        self.capture_patch = mock.patch.object(owned, "capture_identity", side_effect=owned.ProcessIdentityError("public_native_unavailable"))
        self.capture_patch.start()
        self.addCleanup(self.capture_patch.stop)
        real_terminate = owned.terminate_records
        self.fast_terminate = lambda records, known_handles=None, **_: real_terminate(records, known_handles=known_handles, term_timeout=0, kill_timeout=0)
        self.stop_patch = mock.patch.object(owned, "terminate_records", side_effect=self.fast_terminate)
        self.stop_patch.start()
        self.addCleanup(self.stop_patch.stop)

    @staticmethod
    def record(path, pid, role, argv):
        return {"pid": pid, "role": role, "observer_pid": os.getpid(),
                "requested_argv": list(argv), "identity_status": "unavailable"}

    def register(self, role, *, writer=False, denied=False, bad_poll=False):
        process = FakeProcess(denied=denied, bad_poll=bad_poll)
        self.finalizer.register(process, role, writer=writer)
        return process

    def test_primary_outcome_is_durable_before_first_signal(self):
        process = self.register("exit", writer=True)
        original_term = process.terminate
        def term():
            self.assertEqual(json.loads((self.directory / "workload-outcome.json").read_text())["failure"], "original_integrity_failure")
            original_term()
        process.terminate = term
        row = {"failure": "original_integrity_failure"}
        result = self.finalizer.finalize(row)
        self.assertEqual(row, {"failure": "original_integrity_failure"})
        self.assertTrue(result["primary_outcome_persisted"])
        self.assertTrue(result["writers_stopped"])

    def test_denied_benchmark_never_skips_writers_or_profile_removal(self):
        bench = self.register("bench", denied=True)
        writers = [self.register(role, writer=True) for role in ("exit", "client")]
        profile = self.directory / "profile.json"
        profile.write_text("public fixture")
        row = {"failure": "quota_guard"}
        result = self.finalizer.finalize(row, config_paths=[profile])
        self.assertTrue(result["writers_stopped"])
        self.assertFalse(result["all_stopped"])
        self.assertFalse(result["cohort_continuation_allowed"])
        self.assertFalse(profile.exists())
        self.assertTrue(all(p.returncode is not None for p in writers))
        self.assertIn("TERM", bench.signals)
        self.assertEqual(row["failure"], "quota_guard")
        self.assertTrue(result["original_children"]["errors"])

    def test_unexpected_helper_exception_attempts_every_original_child(self):
        processes = [self.register(role, writer=role != "bench") for role in ("bench", "client", "exit")]
        with mock.patch.object(owned, "terminate_records", side_effect=RuntimeError("public helper fault")) as stop:
            result = self.finalizer.finalize({"failure": "original_timeout"})
        self.assertEqual(stop.call_count, 1 + len(processes))
        self.assertTrue(all(p.returncode is not None for p in processes))
        self.assertTrue(result["writers_stopped"])
        self.assertFalse(result["cohort_continuation_allowed"])

    def test_registration_write_fault_still_retains_original_handle(self):
        process = FakeProcess()
        with mock.patch.object(owned, "append_process_record", side_effect=OSError(errno.ENOSPC, "public full disk")):
            with self.assertRaisesRegex(RuntimeError, "registration_failed"):
                self.finalizer.register(process, "exit", writer=True)
        result = self.finalizer.finalize({"failure": "setup_registration"})
        self.assertIsNotNone(process.returncode)
        self.assertTrue(result["writers_stopped"])
        self.assertFalse(result["cohort_continuation_allowed"])

    def test_partial_setup_without_children_removes_private_profile(self):
        profile = self.directory / "profile.json"
        profile.write_text("public partial profile")
        result = self.finalizer.finalize({"failure": "setup_hash_read"}, config_paths=[profile])
        self.assertFalse(profile.exists())
        self.assertTrue(result["writers_stopped"])

    def test_live_writer_blocks_drive_cleanup(self):
        self.register("exit", writer=True, denied=True)
        result = self.finalizer.finalize({"failure": "original_guard"})
        self.assertFalse(result["writers_stopped"])
        self.assertFalse(result["drive_cleanup_allowed"])

    def test_unknown_poll_never_becomes_original_signal_authority(self):
        process = self.register("exit", writer=True, bad_poll=True)
        result = self.finalizer.finalize({"failure": "original_guard"})
        self.assertFalse(process.signals)
        self.assertFalse(result["writers_stopped"])

    def test_child_journal_read_only_after_parent_reaped(self):
        parent = self.register("bench")
        journal = self.directory / "curl.jsonl"
        journal.write_text("public test fixture")
        self.finalizer.register_child_journal(journal, parent)
        def load(path):
            self.assertIsNotNone(parent.returncode)
            return []
        with mock.patch.object(owned, "load_process_records", side_effect=load) as loader:
            self.finalizer.finalize({"failure": "original"})
        loader.assert_called_once_with(journal)

    def test_live_parent_skips_child_journal_lock_and_stops_cohort(self):
        parent = self.register("bench", denied=True)
        journal = self.directory / "curl.jsonl"
        self.finalizer.register_child_journal(journal, parent)
        with mock.patch.object(owned, "load_process_records") as loader:
            result = self.finalizer.finalize({"failure": "original"})
        loader.assert_not_called()
        self.assertFalse(result["all_stopped"])

    def test_unresolved_spawn_intent_never_authorizes_guessed_kill(self):
        parent = self.register("bench")
        journal = self.directory / "curl.jsonl"
        journal.with_name(journal.name + ".spawn-pending").write_text("public intent")
        self.finalizer.register_child_journal(journal, parent)
        with mock.patch.object(os, "kill") as kill:
            result = self.finalizer.finalize({"failure": "original"})
        kill.assert_not_called()
        self.assertFalse(result["all_stopped"])

    def test_reused_child_pid_is_not_signaled(self):
        parent = self.register("bench")
        journal = self.directory / "curl.jsonl"
        journal.write_text("fixture")
        self.finalizer.register_child_journal(journal, parent)
        record = {"pid": 777777, "role": "curl", "observer_pid": os.getpid(),
                  "requested_argv": ["publiccurl"], "identity_status": "captured",
                  "kernel_creation": "darwin:123:456", "uid": os.geteuid(),
                  "executable": "/public/curl", "argv": ["publiccurl"]}
        current = {**record, "kernel_creation": "darwin:124:456"}
        def identity(pid, **kwargs):
            if pid == 777777:
                return current
            raise owned.ProcessIdentityError("public_native_unavailable")
        with mock.patch.object(owned, "load_process_records", return_value=[record]), mock.patch.object(owned, "capture_identity", side_effect=identity), mock.patch.object(os, "kill") as kill:
            result = self.finalizer.finalize({"failure": "original"})
        kill.assert_not_called()
        self.assertTrue(result["all_stopped"])
        self.assertEqual(result["benchmark_children"]["results"][0]["status"], "original_pid_reused_no_signal")

    def test_profile_outside_arm_never_unlinked(self):
        outside = self.directory.parent / "unrelated.json"
        outside.write_text("public unrelated fixture")
        result = self.finalizer.finalize({"failure": "original"}, config_paths=[outside])
        self.assertTrue(outside.exists())
        self.assertFalse(result["cohort_continuation_allowed"])

    def test_existing_outcome_is_never_overwritten(self):
        path = self.directory / "workload-outcome.json"
        path.write_text('{"failure":"earlier"}')
        result = self.finalizer.finalize({"failure": "current"})
        self.assertEqual(json.loads(path.read_text())["failure"], "earlier")
        self.assertFalse(result["primary_outcome_persisted"])

    def test_failing_log_close_does_not_skip_next_stream(self):
        bad = mock.Mock()
        bad.close.side_effect = OSError(errno.EIO, "public close fixture")
        good = io.StringIO()
        result = self.finalizer.finalize({"failure": "original"}, logs=[bad, good])
        self.assertTrue(good.closed)
        self.assertFalse(result["cohort_continuation_allowed"])

    def test_real_native_original_child_and_private_proof(self):
        self.register_patch.stop()
        self.capture_patch.stop()
        self.stop_patch.stop()
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        try:
            self.finalizer.register(process, "public-sleeper", writer=True)
            result = self.finalizer.finalize({"failure": "public_original_fixture"})
            self.assertTrue(result["all_stopped"])
            self.assertTrue(result["writers_stopped"])
            self.assertIsNotNone(process.poll())
            for name in ["workload-outcome.json", "teardown-result.json", "owned-processes.jsonl"]:
                self.assertEqual((self.directory / name).stat().st_mode & 0o777, 0o600)
        finally:
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
