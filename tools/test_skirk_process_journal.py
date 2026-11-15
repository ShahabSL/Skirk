import argparse
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from skirk_live_mix_bench import execute_curl
from skirk_persistent_text_bench import owned_process_observer
from skirk_owned_processes import load_process_records, terminate_records


class ProcessJournalTests(unittest.TestCase):
    def test_intent_precedes_spawn_and_only_native_registration_releases_it(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "curl.jsonl"
            pending = path.with_name(path.name + ".spawn-pending")
            observer = owned_process_observer(path)
            observer("before", None, ["curl", "http://127.0.0.1/file"])
            self.assertTrue(pending.exists())
            self.assertEqual(pending.stat().st_mode & 0o777, 0o600)
            with patch("skirk_owned_processes.append_process_record", return_value={"identity_status": "captured"}) as append:
                observer("spawn", argparse.Namespace(pid=1234), ["curl"])
            append.assert_called_once_with(path, 1234, "curl", ["curl"])
            self.assertFalse(pending.exists())

    def test_missing_native_identity_keeps_intent_until_child_is_proven_exited(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "curl.jsonl"
            pending = path.with_name(path.name + ".spawn-pending")
            observer = owned_process_observer(path)
            observer("before", None, ["curl"])
            with patch("skirk_owned_processes.append_process_record", return_value={"identity_status": "unavailable"}), self.assertRaisesRegex(RuntimeError, "identity_unavailable"):
                observer("spawn", argparse.Namespace(pid=1234), ["curl"])
            self.assertTrue(pending.exists())
            observer("exit", argparse.Namespace(pid=1234), ["curl"])
            self.assertFalse(pending.exists())

    def test_callback_failure_kills_and_reaps_only_the_direct_unreaped_child(self):
        events = []
        process = Mock(pid=1234, returncode=-9)
        process.communicate.return_value = ("", "")
        process.poll.return_value = -9

        def observe(kind, child, command):
            events.append(kind)
            if kind == "spawn":
                raise RuntimeError("journal_failure")

        with patch("skirk_live_mix_bench.subprocess.Popen", return_value=process), self.assertRaisesRegex(RuntimeError, "journal_failure"):
            execute_curl(["curl"], 1, None, None, observe)
        process.kill.assert_called_once()
        process.communicate.assert_called_once()
        self.assertEqual(events, ["before", "spawn", "exit"])

    def test_spawn_failure_clears_intent_without_claiming_a_child_identity(self):
        events = []
        with patch("skirk_live_mix_bench.subprocess.Popen", side_effect=OSError("missing")), self.assertRaises(OSError):
            execute_curl(["curl"], 1, None, None, lambda event, *_: events.append(event))
        self.assertEqual(events, ["before", "spawn_failed"])

    def test_failed_kill_and_bounded_reap_preserve_the_original_callback_exception(self):
        process = Mock(pid=1234)
        process.kill.side_effect = PermissionError(1, "denied")
        process.communicate.side_effect = subprocess.TimeoutExpired(["curl"], 3)
        process.poll.return_value = None

        def observe(kind, *_):
            if kind == "spawn":
                raise RuntimeError("original_journal_failure")

        with patch("skirk_live_mix_bench.subprocess.Popen", return_value=process), self.assertRaisesRegex(RuntimeError, "original_journal_failure") as caught:
            execute_curl(["curl"], 1, None, None, observe)
        process.communicate.assert_called_once_with(timeout=3)
        self.assertEqual([row["phase"] for row in caught.exception.skirk_cleanup_failures], ["kill", "reap"])
        self.assertEqual(caught.exception.skirk_cleanup_failures[0]["errno"], 1)

    def test_default_execution_retains_original_subprocess_run_path(self):
        result = subprocess.CompletedProcess(["curl"], 0, "", "")
        with patch("skirk_live_mix_bench.subprocess.run", return_value=result) as run, \
             patch("skirk_live_mix_bench.subprocess.Popen", side_effect=AssertionError("unexpected observer path")):
            self.assertIs(execute_curl(["curl"], 1, None, None)[0], result)
        run.assert_called_once()

    def test_real_owned_child_has_native_identity_and_is_reaped_without_group_signals(self):
        with tempfile.TemporaryDirectory() as directory:
            private = Path(directory) / ".skirk-runs"
            private.mkdir(mode=0o700)
            journal = private / "curl.jsonl"
            command = [sys.executable, "-c", "import time; time.sleep(0.15)"]
            completed, failure = execute_curl(command, 2, None, None, owned_process_observer(journal))
            self.assertEqual(completed.returncode, 0)
            self.assertIsNone(failure)
            records = load_process_records(journal)
            self.assertEqual(len(records), 1)
            self.assertEqual(records[0]["identity_status"], "captured")
            self.assertEqual(records[0]["requested_argv"], command)
            self.assertFalse(journal.with_name(journal.name + ".spawn-pending").exists())
            self.assertTrue(terminate_records(records, term_timeout=0, kill_timeout=0)["all_stopped"])


if __name__ == "__main__":
    unittest.main()
