import errno
import json
import os
import signal
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import skirk_owned_processes as owned


def identity(pid=401, creation="darwin:1:2", argv=None):
    return {"pid": pid, "ppid": os.getpid(), "pgid": pid, "sid": pid,
            "uid": os.geteuid(), "kernel_creation": creation,
            "executable": "/public/fixture", "argv": argv or ["fixture", "--public-data"]}


def record(pid=401, **updates):
    return {**identity(pid), "role": "client-1", "observer_pid": os.getpid(),
            "requested_argv": ["fixture", "--public-data"],
            "identity_status": "captured", **updates}


class FakeChild:
    def __init__(self, pid=401, returncode=None):
        self.pid = pid
        self.args = ["fixture", "--public-data"]
        self.returncode = returncode
        self.signals = []

    def poll(self):
        return self.returncode

    def terminate(self):
        self.signals.append("TERM")
        self.returncode = -signal.SIGTERM

    def kill(self):
        self.signals.append("KILL")
        self.returncode = -signal.SIGKILL


class OwnedProcessTests(unittest.TestCase):
    def test_native_darwin_structure_matches_installed_sdk(self):
        self.assertEqual(owned.ctypes.sizeof(owned._ProcBSDInfo), 136)
        self.assertEqual(owned._ProcBSDInfo.start_sec.offset, 120)
        self.assertEqual(owned._ProcBSDInfo.start_usec.offset, 128)

    def test_darwin_args_preserve_exact_argv_and_never_parse_environment(self):
        raw = (struct.pack("=i", 3) + b"/public/fixture\0\0fixture\0\0last\0"
               + b"SECRET_ENV=not-owned-arguments\0")
        self.assertEqual(owned._parse_darwin_args(raw), ["fixture", "", "last"])
        with self.assertRaises(owned.ProcessIdentityError):
            owned._parse_darwin_args(struct.pack("=i", 2) + b"/fixture\0arg\0missing")

    def test_linux_stat_handles_parentheses_and_retains_creation_ticks(self):
        fields = ["S", "7", "8", "9"] + ["0"] * 15 + ["12345"] + ["0"] * 8
        found = owned._parse_linux_stat("401 (fixture ) worker) " + " ".join(fields))
        self.assertEqual(found, {"pid": 401, "ppid": 7, "pgid": 8, "sid": 9, "start_ticks": 12345})
        with self.assertRaises(owned.ProcessGone):
            owned._parse_linux_stat("401 (fixture) Z " + " ".join(fields[1:]))

    def test_linux_exact_argv_keeps_trailing_empty_argument(self):
        with mock.patch.object(owned.sys, "platform", "linux"), \
                mock.patch.object(owned, "_linux_core", return_value=identity()), \
                mock.patch.object(owned.os, "readlink", return_value="/public/fixture"), \
                mock.patch.object(owned, "_read_bounded", return_value=b"fixture\0last\0\0"):
            self.assertEqual(owned.capture_identity(401)["argv"], ["fixture", "last", ""])

    def test_capture_rejects_creation_change_during_native_argument_read(self):
        with mock.patch.object(owned.sys, "platform", "darwin"), \
                mock.patch.object(owned, "_darwin_core", side_effect=[identity(), identity(creation="darwin:3:4")]), \
                mock.patch.object(owned, "_darwin_details", return_value=("/public/fixture", ["fixture"])):
            with self.assertRaisesRegex(owned.ProcessIdentityError, "identity_changed"):
                owned.capture_identity(401, expected_argv=["fixture"])

    def test_darwin_requires_known_nonempty_argv_and_rejects_shifted_environment_candidate(self):
        with mock.patch.object(owned.sys, "platform", "darwin"):
            for argv in (None, ["", "fixture"]):
                with self.assertRaisesRegex(owned.ProcessIdentityError, "require_expected_argv"):
                    owned.capture_identity(401, expected_argv=argv)
            with mock.patch.object(owned, "_darwin_core", return_value=identity()), \
                    mock.patch.object(owned, "_darwin_details", return_value=("/public/fixture", ["arg1", "ENV=private"])):
                with self.assertRaisesRegex(owned.ProcessIdentityError, "owned_arguments_changed"):
                    owned.capture_identity(401, expected_argv=["fixture", "arg1"])

    def test_corrupt_creation_token_cannot_establish_original_process_exit(self):
        with mock.patch.object(owned, "capture_identity", return_value=identity()), \
                mock.patch.object(owned.os, "kill") as kill:
            result = owned.terminate_records([record(kernel_creation="corrupt")], term_timeout=0, kill_timeout=0)
        self.assertFalse(result["all_stopped"])
        self.assertEqual(result["results"][0]["status"], "invalid_owned_record")
        kill.assert_not_called()

    def test_short_darwin_query_cannot_inherit_stale_process_gone_errno(self):
        library = mock.Mock()
        library.proc_pidinfo.return_value = 1  # Unexpected short native result.
        owned.ctypes.set_errno(errno.ESRCH)
        with mock.patch.object(owned.ctypes, "CDLL", return_value=library):
            with self.assertRaises(owned.ProcessIdentityError) as caught:
                owned._darwin_core(401)
        self.assertNotIsInstance(caught.exception, owned.ProcessGone)
        self.assertEqual(caught.exception.errno, 0)

    def test_private_manifest_round_trip_and_failed_capture_is_not_silently_lost(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / ".skirk-runs"
            root.mkdir(mode=0o700)
            path = root / "owned.jsonl"
            with mock.patch.object(owned, "capture_identity", side_effect=[identity(), owned.ProcessIdentityError("denied", errno.EPERM)]):
                captured = owned.append_process_record(path, 401, "client-1", ["fixture", "--public-data"])
                unavailable = owned.append_process_record(path, 402, "curl", ["fixture"])
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(owned.load_process_records(path), [captured, unavailable])
            self.assertEqual(unavailable["identity_status"], "unavailable")
            self.assertEqual(unavailable["identity_errno"], errno.EPERM)

    def test_manifest_rejects_public_parent_symlink_public_file_and_truncation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / ".skirk-runs"
            root.mkdir(mode=0o700)
            path = root / "owned.jsonl"
            path.write_text(json.dumps(record()) + "\n")
            path.chmod(0o644)
            with self.assertRaises(owned.ProcessIdentityError):
                owned.load_process_records(path)
            path.chmod(0o600)
            link = root / "link.jsonl"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                owned.load_process_records(link)
            path.write_text(json.dumps(record()))
            with self.assertRaisesRegex(owned.ProcessIdentityError, "truncated"):
                owned.load_process_records(path)
            root.chmod(0o755)
            with self.assertRaisesRegex(owned.ProcessIdentityError, "private_directory"):
                owned.load_process_records(path)

    def test_malformed_and_reused_records_never_signal(self):
        reused = identity(creation="darwin:999:1")
        with mock.patch.object(owned, "capture_identity", return_value=reused), \
                mock.patch.object(owned.os, "kill") as kill:
            result = owned.terminate_records([record(), record(pid=-1), record(kernel_creation=None), None], term_timeout=0, kill_timeout=0)
        kill.assert_not_called()
        self.assertTrue(result["results"][0]["verified_stopped"])
        self.assertEqual(result["results"][0]["status"], "original_pid_reused_no_signal")
        self.assertFalse(result["all_stopped"])
        self.assertTrue(all(row["status"] == "invalid_owned_record" for row in result["results"][1:]))

    def test_reaped_original_child_is_stopped_even_if_its_old_pid_is_reused(self):
        child = FakeChild(returncode=0)
        with mock.patch.object(owned, "capture_identity", side_effect=AssertionError("no native lookup needed")), \
                mock.patch.object(owned.os, "kill") as kill:
            result = owned.terminate_records([record()], {401: child}, term_timeout=0, kill_timeout=0)
        self.assertTrue(result["ok"])
        self.assertTrue(result["all_stopped"])
        kill.assert_not_called()

    def test_unreaped_original_handle_fallback_does_not_require_native_query_permission(self):
        child = FakeChild()
        with mock.patch.object(owned, "capture_identity", side_effect=owned.ProcessIdentityError("denied", errno.EPERM)), \
                mock.patch.object(owned.os, "kill") as kill:
            result = owned.terminate_records([record(identity_status="unavailable")], {401: child}, term_timeout=0, kill_timeout=0)
        self.assertTrue(result["all_stopped"])
        self.assertEqual(child.signals, ["TERM"])
        kill.assert_not_called()

    def test_foreign_handle_and_changed_identity_fail_closed(self):
        child = FakeChild()
        with mock.patch.object(owned, "capture_identity", side_effect=owned.ProcessIdentityError("denied", errno.EPERM)):
            result = owned.terminate_records([record(observer_pid=os.getpid() + 1)], {401: child}, term_timeout=0, kill_timeout=0)
        self.assertFalse(result["all_stopped"])
        self.assertEqual(child.signals, [])
        for changed in ({"uid": os.geteuid() + 1}, {"argv": ["different"]}, {"executable": "/different"}):
            with self.subTest(changed=changed), \
                    mock.patch.object(owned, "capture_identity", return_value={**identity(), **changed}), \
                    mock.patch.object(owned.os, "kill") as kill:
                result = owned.terminate_records([record()], term_timeout=0, kill_timeout=0)
                self.assertFalse(result["all_stopped"])
                kill.assert_not_called()

    def test_permission_failure_is_retained_and_does_not_skip_other_owned_processes(self):
        alive = {401, 402}
        signals = []

        def capture(pid, **_kwargs):
            if pid not in alive:
                raise owned.ProcessGone("gone")
            return identity(pid)

        def send(pid, sig):
            signals.append((pid, sig))
            if pid == 401:
                raise PermissionError(errno.EPERM, "public fixture denial")
            alive.remove(pid)

        with mock.patch.object(owned.sys, "platform", "darwin"), \
                mock.patch.object(owned, "capture_identity", side_effect=capture), \
                mock.patch.object(owned.os, "kill", side_effect=send), \
                mock.patch.object(owned.os, "killpg", side_effect=AssertionError("never signal a process group")):
            result = owned.terminate_records([record(), record(402)], term_timeout=0, kill_timeout=0)
        self.assertEqual(signals, [(401, signal.SIGTERM), (402, signal.SIGTERM), (401, signal.SIGKILL)])
        self.assertFalse(result["all_stopped"])
        self.assertTrue(result["results"][1]["verified_stopped"])
        self.assertEqual([error["errno"] for error in result["errors"] if "errno" in error], [errno.EPERM, errno.EPERM])

    def test_all_term_signals_precede_kills_and_wait_is_global_not_per_process(self):
        clock = [0.0]
        signals = []

        def sleep(seconds):
            clock[0] += seconds

        with mock.patch.object(owned.sys, "platform", "darwin"), \
                mock.patch.object(owned, "capture_identity", side_effect=lambda pid, **kwargs: identity(pid)), \
                mock.patch.object(owned.os, "kill", side_effect=lambda pid, sig: signals.append((pid, sig))), \
                mock.patch.object(owned.time, "monotonic", side_effect=lambda: clock[0]), \
                mock.patch.object(owned.time, "sleep", side_effect=sleep):
            result = owned.terminate_records([record(401), record(402), record(403)], term_timeout=0.1, kill_timeout=0.1)
        self.assertEqual([sig for _, sig in signals], [signal.SIGTERM] * 3 + [signal.SIGKILL] * 3)
        self.assertLessEqual(clock[0], 0.201)
        self.assertFalse(result["all_stopped"])

    def test_recorded_children_are_the_only_actual_processes_terminated(self):
        # Public sleeping Python children only; no Google or network activity.
        children = [subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"]) for _ in range(2)]
        try:
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory) / ".skirk-runs"
                root.mkdir(mode=0o700)
                manifest = root / "processes.jsonl"
                row = owned.append_process_record(manifest, children[0].pid, "owned-sleeper", children[0].args)
                self.assertEqual(row["identity_status"], "captured")
                self.assertEqual(row["ppid"], os.getpid())
                self.assertTrue(row["kernel_creation"].startswith("darwin:" if sys.platform == "darwin" else "linux:"))
                result = owned.terminate_records(owned.load_process_records(manifest), {children[0].pid: children[0]}, term_timeout=1, kill_timeout=1)
                self.assertTrue(result["all_stopped"], result)
                self.assertIsNone(children[1].poll(), "unrecorded child must remain untouched")
        finally:
            for child in children:
                if child.poll() is None:
                    child.kill()
                child.wait(timeout=3)


if __name__ == "__main__":
    unittest.main()
