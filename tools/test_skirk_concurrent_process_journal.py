import argparse
import concurrent.futures
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest import mock

from skirk_process_observer import owned_process_observer, pending_paths, validate_journal_path
from skirk_live_mix_bench import parse_args


class ConcurrentJournalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.private = Path(self.temp.name) / ".skirk-runs"
        self.private.mkdir(mode=0o700)
        self.journal = self.private / "curl.jsonl"

    def test_concurrent_spawns_have_distinct_intents_and_exact_native_records(self):
        observer = owned_process_observer(self.journal, concurrent=True)
        barrier = threading.Barrier(2)
        calls = []
        lock = threading.Lock()
        def append(path, pid, role, argv):
            with lock:
                calls.append((pid, argv))
            return {"identity_status": "captured"}
        def task(index):
            command = ["publiccurl", str(index)]
            observer("before", None, command)
            barrier.wait(timeout=5)
            files = pending_paths(self.journal)
            self.assertEqual(len(files), 2)
            self.assertTrue(all(path.stat().st_mode & 0o777 == 0o600 for path in files))
            barrier.wait(timeout=5)
            observer("spawn", argparse.Namespace(pid=1000 + index), command)
            observer("exit", argparse.Namespace(pid=1000 + index), command)
        with mock.patch("skirk_owned_processes.append_process_record", side_effect=append):
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                list(pool.map(task, range(2)))
        self.assertEqual(sorted(calls), [(1000, ["publiccurl", "0"]), (1001, ["publiccurl", "1"])])
        self.assertFalse(pending_paths(self.journal))

    def test_unknown_identity_keeps_only_that_threads_intent(self):
        observer = owned_process_observer(self.journal, concurrent=True)
        observer("before", None, ["publiccurl"])
        with mock.patch("skirk_owned_processes.append_process_record", return_value={"identity_status": "unavailable"}):
            with self.assertRaisesRegex(RuntimeError, "identity_unavailable"):
                observer("spawn", argparse.Namespace(pid=1000), ["publiccurl"])
        self.assertEqual(len(pending_paths(self.journal)), 1)
        observer("exit", argparse.Namespace(pid=1000), ["publiccurl"])
        self.assertFalse(pending_paths(self.journal))

    def test_spawn_failure_clears_its_intent_without_claiming_pid(self):
        observer = owned_process_observer(self.journal, concurrent=True)
        observer("before", None, ["publiccurl"])
        observer("spawn_failed", None, ["publiccurl"])
        self.assertFalse(pending_paths(self.journal))

    def test_nested_before_rejects_without_overwriting_original_intent(self):
        observer = owned_process_observer(self.journal, concurrent=True)
        observer("before", None, ["publiccurl"])
        before = pending_paths(self.journal)
        with self.assertRaisesRegex(RuntimeError, "already_active"):
            observer("before", None, ["other"])
        self.assertEqual(pending_paths(self.journal), before)
        observer("spawn_failed", None, ["publiccurl"])

    def test_journal_path_validation_rejects_existing_unique_intent(self):
        validate_journal_path(self.journal)
        self.journal.with_name(self.journal.name + ".spawn-pending-" + "a" * 32).touch()
        with self.assertRaises(ValueError):
            validate_journal_path(self.journal)

    def test_generic_mix_cli_accepts_private_journal_and_retains_default_none(self):
        base = ["bench", "--small-url", "http://127.0.0.1/small", "--bulk-url", "http://127.0.0.1/bulk"]
        with mock.patch("sys.argv", base):
            self.assertIsNone(parse_args().owned_process_journal)
        with mock.patch("sys.argv", base + ["--owned-process-journal", str(self.journal)]):
            self.assertEqual(parse_args().owned_process_journal, self.journal)


if __name__ == "__main__":
    unittest.main()
