"""Durable arm evidence and verified teardown for owned A/B benchmark children.

No Google calls are made here. A caller must gate later Drive cleanup on
``writers_stopped`` and stop the cohort on any unresolved child or cleanup error.
Only original Popen handles and native-identity journal records are authority.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import stat
import time
from typing import Any

import skirk_owned_processes as owned
from skirk_process_observer import pending_paths


def _error(stage: str, error: BaseException) -> dict[str, Any]:
    row: dict[str, Any] = {"stage": stage, "error_type": type(error).__name__}
    if isinstance(error, OSError):
        row["errno"] = error.errno
    if isinstance(error, owned.ProcessIdentityError):
        row["identity_error"] = error.code
    return row


def _atomic_private_json(path: Path, value: Any) -> None:
    raw = (json.dumps(value, indent=2) + "\n").encode()
    temp = path.with_name(path.name + ".tmp")
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        parent = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(parent)
        finally:
            os.close(parent)
    finally:
        temp.unlink(missing_ok=True)


class ArmFinalizer:
    def __init__(self, directory: Path):
        self.directory = Path(directory)
        self.manifest = self.directory / "owned-processes.jsonl"
        info = self.directory.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o077
                or info.st_uid != os.geteuid() or ".skirk-runs" not in self.directory.resolve().parts):
            raise ValueError("arm_finalizer_requires_private_owned_directory")
        self.processes: list[dict[str, Any]] = []
        self.child_journals: list[tuple[Path, subprocess.Popen[Any]]] = []
        self.failures: list[dict[str, Any]] = []
        self.result: dict[str, Any] | None = None

    def register(self, process: subprocess.Popen[Any], role: str,
                 argv: list[str] | None = None, *, writer: bool = False) -> subprocess.Popen[Any]:
        if self.result is not None or any(item["handle"] is process for item in self.processes):
            raise ValueError("duplicate_or_closed_arm_registration")
        command = argv if argv is not None else list(process.args) if isinstance(process.args, (list, tuple)) else None
        fallback = {"pid": process.pid, "role": role, "observer_pid": os.getpid(),
                    "requested_argv": command, "identity_status": "unavailable"}
        entry = {"handle": process, "record": fallback, "writer": bool(writer)}
        self.processes.append(entry)  # Ownership survives a manifest-write fault.
        try:
            entry["record"] = owned.append_process_record(self.manifest, process.pid, role, command)
        except Exception as error:
            self.failures.append(_error("register_" + role, error))
            raise RuntimeError("owned_process_registration_failed") from error
        return process

    def register_child_journal(self, path: Path, parent: subprocess.Popen[Any]) -> None:
        if not any(item["handle"] is parent for item in self.processes):
            raise ValueError("child_journal_parent_not_owned")
        if self.result is not None:
            raise ValueError("arm_already_finalized")
        path = Path(path)
        if not path.resolve().is_relative_to(self.directory.resolve()):
            raise ValueError("child_journal_outside_arm")
        self.child_journals.append((path, parent))

    def run(self, command: list[str], *, role: str, timeout: float,
            text: bool = False, writer: bool = False) -> subprocess.CompletedProcess[Any]:
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=text)
        self.register(process, role, command, writer=writer)
        stdout, stderr = process.communicate(timeout=timeout)
        result = subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
        result.check_returncode()
        return result

    def persist_post_stop_proof(self, value: Any) -> None:
        if self.result is None or not self.result.get("all_stopped"):
            raise ValueError("post_stop_proof_requires_all_owned_children_stopped")
        _atomic_private_json(self.directory / "post-stop-origin-proof.json", value)

    @staticmethod
    def _poll_state(process: subprocess.Popen[Any]) -> str:
        try:
            return "stopped" if process.poll() is not None else "running"
        except Exception:
            return "unknown"

    @staticmethod
    def _reaped(process: subprocess.Popen[Any]) -> bool:
        return ArmFinalizer._poll_state(process) == "stopped"

    def _stop_records(self, records: list[dict[str, Any]], handles: dict[int, subprocess.Popen[Any]]) -> dict[str, Any]:
        try:
            result = owned.terminate_records(records, known_handles=handles)
            if not isinstance(result, dict) or type(result.get("all_stopped")) is not bool:
                raise ValueError("invalid_native_teardown_result")
            return result
        except Exception as error:
            self.failures.append(_error("terminate_records", error))
        # An unexpected aggregate helper failure must not skip later owners.
        results, errors = [], []
        for record in records:
            try:
                result = owned.terminate_records([record], known_handles=handles, term_timeout=0, kill_timeout=0)
                results.extend(result["results"])
                errors.extend(result["errors"])
            except Exception as error:
                self.failures.append(_error("terminate_record", error))
                results.append({"pid": record.get("pid"), "role": record.get("role"),
                                "verified_stopped": False, "status": "helper_failed"})
        return {"ok": False, "all_stopped": all(r.get("verified_stopped") is True for r in results),
                "results": results, "errors": errors}

    def _fallback_original_handles(self) -> None:
        # Exactly the native helper's unreaped-original-Popen fallback: poll
        # proves it is still our unreaped child; no PID/group/name search.
        for item in self.processes:
            process = item["handle"]
            if self._poll_state(process) == "running":
                try:
                    process.terminate()
                except Exception as error:
                    self.failures.append(_error("original_child_term", error))
        deadline = time.monotonic() + 3
        for item in self.processes:
            process = item["handle"]
            if self._poll_state(process) == "running":
                try:
                    process.wait(timeout=max(0, deadline - time.monotonic()))
                except subprocess.TimeoutExpired:
                    pass
                except Exception as error:
                    self.failures.append(_error("original_child_wait", error))
        for item in self.processes:
            process = item["handle"]
            if self._poll_state(process) == "running":
                try:
                    process.kill()
                except Exception as error:
                    self.failures.append(_error("original_child_kill", error))
        deadline = time.monotonic() + 3
        for item in self.processes:
            process = item["handle"]
            if self._poll_state(process) == "running":
                try:
                    process.wait(timeout=max(0, deadline - time.monotonic()))
                except Exception as error:
                    self.failures.append(_error("original_child_reap", error))

    def finalize(self, row: dict[str, Any], *, logs=(), config_paths=()) -> dict[str, Any]:
        try:
            return self._finalize(row, logs=logs, config_paths=config_paths)
        except Exception as error:
            # A new observer/helper defect must not erase the primary outcome
            # or skip remaining original children. Unknown children stay unknown.
            self.failures.append(_error("unexpected_finalize", error))
            outcome = self.directory / "workload-outcome.json"
            try:
                if not outcome.exists():
                    _atomic_private_json(outcome, row)
            except Exception as secondary:
                self.failures.append(_error("emergency_persist_workload", secondary))
            try:
                self._fallback_original_handles()
            except Exception as secondary:
                self.failures.append(_error("emergency_original_children", secondary))
            for path, parent in self.child_journals:
                if not self._reaped(parent):
                    continue
                try:
                    records = owned.load_process_records(path) if path.exists() else []
                    for record in records:
                        try:
                            owned.terminate_records([record], term_timeout=0, kill_timeout=0)
                        except Exception as secondary:
                            self.failures.append(_error("emergency_native_child", secondary))
                except Exception as secondary:
                    self.failures.append(_error("emergency_child_journal", secondary))
            for stream in logs:
                try:
                    stream.close()
                except Exception as secondary:
                    self.failures.append(_error("emergency_close_log", secondary))
            for value in config_paths:
                try:
                    path = Path(value)
                    if path.resolve().is_relative_to(self.directory.resolve()):
                        path.unlink(missing_ok=True)
                except Exception as secondary:
                    self.failures.append(_error("emergency_remove_config", secondary))
            stopped = all(self._reaped(item["handle"]) for item in self.processes if item["writer"])
            self.result = {"writers_stopped": stopped, "drive_cleanup_allowed": stopped,
                           "all_stopped": False, "cohort_continuation_allowed": False,
                           "cleanup_failures": self.failures,
                           "primary_outcome_persisted": outcome.exists(), "emergency_finalization": True}
            try:
                _atomic_private_json(self.directory / "teardown-result.json", self.result)
            except Exception as secondary:
                self.failures.append(_error("emergency_persist_teardown", secondary))
            return self.result

    def _finalize(self, row: dict[str, Any], *, logs=(), config_paths=()) -> dict[str, Any]:
        if self.result is not None:
            return self.result
        outcome = self.directory / "workload-outcome.json"
        primary_persisted = False
        try:
            if outcome.exists():
                raise FileExistsError("original_workload_outcome_already_exists")
            _atomic_private_json(outcome, row)
            primary_persisted = True
        except Exception as error:
            self.failures.append(_error("persist_workload_before_teardown", error))
        records = [item["record"] for item in self.processes]
        handles = {item["handle"].pid: item["handle"] for item in self.processes}
        original = self._stop_records(records, handles)
        if not all(self._reaped(item["handle"]) for item in self.processes):
            self._fallback_original_handles()
        children = []
        for path, parent in self.child_journals:
            if not self._reaped(parent):
                self.failures.append({"stage": "child_journal", "error_type": "ParentStillRunning"})
                continue  # It may still hold the journal's append lock.
            if pending_paths(path):
                self.failures.append({"stage": "child_journal", "error_type": "UnresolvedSpawnIntent"})
            try:
                if path.exists():
                    children.extend(owned.load_process_records(path))
            except Exception as error:
                self.failures.append(_error("load_child_journal", error))
        child_result = self._stop_records(children, {}) if children else {"ok": True, "all_stopped": True, "results": [], "errors": []}
        for stream in logs:
            try:
                stream.close()
            except Exception as error:
                self.failures.append(_error("close_log", error))
        for value in config_paths:
            try:
                path = Path(value)
                if not path.resolve().is_relative_to(self.directory.resolve()):
                    raise ValueError("config_outside_owned_arm")
                path.unlink(missing_ok=True)
            except Exception as error:
                self.failures.append(_error("remove_private_config", error))
        writers_stopped = all(self._reaped(item["handle"]) for item in self.processes if item["writer"])
        parents_stopped = all(self._reaped(item["handle"]) for item in self.processes)
        child_unknown = any(e["stage"] == "child_journal" or e["stage"] == "load_child_journal" for e in self.failures)
        all_stopped = parents_stopped and child_result["all_stopped"] and not child_unknown
        self.result = {"writers_stopped": writers_stopped, "all_stopped": all_stopped,
                       "drive_cleanup_allowed": writers_stopped, "cohort_continuation_allowed": False,
                       "original_children": original, "benchmark_children": child_result,
                       "cleanup_failures": self.failures,
                       "primary_outcome_persisted": primary_persisted}
        # Signal errors remain failures even if a later observation found the
        # process exited. Never replace the primary workload failure with them.
        self.result["cohort_continuation_allowed"] = bool(all_stopped and original.get("ok") and child_result.get("ok") and not self.failures)
        try:
            _atomic_private_json(self.directory / "teardown-result.json", self.result)
        except Exception as error:
            self.failures.append(_error("persist_teardown", error))
            self.result["cohort_continuation_allowed"] = False
        return self.result
